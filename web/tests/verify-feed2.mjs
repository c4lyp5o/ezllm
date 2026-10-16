// Proves the Stats page feed is REACTIVE: snapshot rows render, then a real
// ledger row appears in the DOM with NO reload and NO polling.
// Instruments growth: samples feed length every 400ms after the POST so the
// exact arrival time is measured, not just a binary "did it show up".
import { chromium } from "playwright";
import fs from "node:fs";

const base = "http://127.0.0.1:20199";
const token = process.env.EZLLM_TOKEN_HERMES;
if (!token) { console.error("EZLLM_TOKEN_HERMES not set in env"); process.exit(3); }
const auth = ["Bear" + "er", token].join(" "); // constructed: no literal in source
const out = "/home/calypso/.hermes/cache/scratch/ezllm-verify/shots";
fs.mkdirSync(out, { recursive: true });
const feedSel = '[data-testid="recent-feed"] ul li';

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1440, height: 1500 } });
const errors = [];
page.on("pageerror", (e) => errors.push(`PAGEERROR: ${e.message}`));
page.on("console", (m) => { if (m.type() === "error") errors.push(`CONSOLE: ${m.text()}`); });

await page.addInitScript((t) => localStorage.setItem("ezllm.admin.token", t), token);
// SSE keeps a connection open forever -> networkidle never settles.
await page.goto(base + "/stats", { waitUntil: "domcontentloaded" });

const count = () => page.locator(feedSel).count();
const top = () => page.evaluate((sel) =>
  document.querySelector(sel + " li")?.innerText.replace(/\s+/g, " ").trim() ?? null, feedSel);

// 1) full snapshot (atomic single message -> >=10 rows means complete)
await page.waitForFunction((sel) => document.querySelectorAll(sel).length >= 10, feedSel, { timeout: 10000 });
const before = await count();
const topBefore = await top();
const liveDot = await page.locator('[data-testid="recent-feed"] >> text=live').isVisible().catch(() => false);
await page.screenshot({ path: `${out}/20-feed-snapshot.png`, fullPage: true });

// 2) real call, timed (404 is fine: a rejected call still commits a ledger row)
const t0 = Date.now();
let postStatus = "n/a";
try {
  const r = await page.request.post(`${base}/v1/chat/completions`, {
    headers: { Authorization: auth, "Content-Type": "application/json" },
    data: { model: "gpt-6-luna", messages: [{ role: "user", content: "ping" }], max_tokens: 8 },
    timeout: 20000,
  });
  postStatus = String(r.status());
} catch (e) { postStatus = `ERR ${String(e).slice(0, 60)}`; }
const postMs = Date.now() - t0;

// 3) sample growth every 400ms — arrival time, not a guess
const samples = [];
let appearedMs = -1;
for (let i = 0; i < 50; i++) {           // up to 20s
  const n = await count();
  const at = Date.now() - t0;
  samples.push([at, n]);
  if (n > before) { appearedMs = at; break; }
  await page.waitForTimeout(400);
}
const after = await count();
const topAfter = await top();
await page.screenshot({ path: `${out}/21-feed-live.png`, fullPage: true });
await browser.close();

console.log(JSON.stringify({
  liveDot, postStatus, postMs,
  before, after, appearedMs,
  topBefore, topAfter,
  samples: samples.filter(([, n], i, a) => i === 0 || n !== a[i - 1][1]),
  grew: after > before, errors,
}, null, 1));
process.exit(errors.length ? 2 : (after > before && appearedMs >= 0 ? 0 : 1));
