// Proves the Stats page feed is REACTIVE: snapshot rows render, then a real
// inference call lands in the DOM within seconds — no reload, no polling.
import { chromium } from "playwright";
import fs from "node:fs";

const base = "http://127.0.0.1:20199";
const token = process.env.EZLLM_TOKEN_HERMES;
if (!token) { console.error("EZLLM_TOKEN_HERMES not set in env"); process.exit(3); }
const out = "/home/calypso/.hermes/cache/scratch/ezllm-verify/shots";
fs.mkdirSync(out, { recursive: true });

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1440, height: 1400 } });
const errors = [];
page.on("pageerror", (e) => errors.push(`PAGEERROR: ${e.message}`));
page.on("console", (m) => { if (m.type() === "error") errors.push(`CONSOLE: ${m.text()}`); });

await page.addInitScript((t) => localStorage.setItem("ezllm.admin.token", t), token);
// NB: SSE keeps a connection open forever -> "networkidle" can never settle.
await page.goto(base + "/stats", { waitUntil: "domcontentloaded" });

// 1) feed renders with snapshot rows + live indicator
await page.waitForSelector("text=Recent requests", { timeout: 10000 });
const feedSel = '[data-testid="recent-feed"] ul li';
await page.waitForFunction(
  (sel) => document.querySelectorAll(sel).length > 0, feedSel, { timeout: 10000 });
const before = await page.locator(feedSel).count();
const liveLabel = await page.locator("text=live").first().isVisible().catch(() => false);
const rowsBefore = await page.evaluate((sel) =>
  [...document.querySelectorAll(sel)].slice(0, 3).map(li => li.innerText.replace(/\n/g, " ")), feedSel);

await page.screenshot({ path: `${out}/20-feed-snapshot.png`, fullPage: true });

// 2) fire a real inference call (dummy upstream refuses -> a failure or success row)
const t0 = Date.now();
let postStatus = "n/a", postBody = "";
try {
  const r = await page.request.post(`${base}/v1/chat/completions`, {
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
    data: { model: "gpt-6-luna", messages: [{ role: "user", content: "ping" }], max_tokens: 8 },
    timeout: 15000,
  });
  postStatus = String(r.status());
  postBody = (await r.text()).slice(0, 120);
} catch (e) {
  postStatus = `ERR ${String(e).slice(0, 80)}`;
}

// 3) the new row must appear WITHOUT navigation or polling
let appearedMs = -1, after = before;
try {
  await page.waitForFunction(
    (sel, n) => document.querySelectorAll(sel).length > n,
    feedSel, before, { timeout: 12000 });
  appearedMs = Date.now() - t0;
  after = await page.locator(feedSel).count();
} catch {
  /* row never arrived — reported below */
}

const rowsAfter = await page.evaluate((sel) =>
  [...document.querySelectorAll(sel)].slice(0, 3).map(li => li.innerText.replace(/\n/g, " ")), feedSel);

await page.screenshot({ path: `${out}/21-feed-live.png`, fullPage: true });
await browser.close();

console.log(JSON.stringify({
  liveLabel, before, after, appearedMs,
  postStatus, postBody,
  rowsBefore, rowsAfter,
  errors,
}, null, 1));
process.exit(errors.length ? 2 : 0);
