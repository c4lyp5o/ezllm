import { chromium } from "playwright";
import fs from "node:fs";
const base = "http://127.0.0.1:20199", token = process.env.EZLLM_TOKEN_HERMES;

if (!token) { console.error("EZLLM_TOKEN_HERMES not set"); process.exit(3); }
const out = "/home/calypso/.hermes/cache/scratch/ezllm-verify/shots";
fs.mkdirSync(out, { recursive: true });
const browser = await chromium.launch();
const results = [], errors = [];
const page = await browser.newPage({ viewport: { width: 1440, height: 1200 } });
page.on("pageerror", e => errors.push(`PAGEERROR: ${e.message}`));
page.on("console", m => { if (m.type() === "error") errors.push(`CONSOLE: ${m.text()}`); });
page.on("requestfailed", r => { if (r.url().includes("/admin")) errors.push(`REQFAIL: ${r.url()}`); });
await page.addInitScript(t => localStorage.setItem("ezllm.admin.token", t), token);
await page.goto(base + "/stats", { waitUntil: "domcontentloaded" });
await page.waitForTimeout(2000);

const rowCount = () => page.locator("table tbody tr").count();

// Range chips at their REAL labels, with Model granularity selected (rows exist)
for (const label of ["24h", "7d", "30d", "1y"]) {
  await page.getByRole("button", { name: "Model", exact: true }).first().click({ timeout: 4000 });
  await page.waitForTimeout(700);
  await page.getByRole("button", { name: label, exact: true }).first().click({ timeout: 4000 });
  await page.waitForTimeout(1600);
  const txt = await page.evaluate(() => document.body.innerText);
  results.push({ step: `range:${label}`, rows: await rowCount(),
                 hasErr: /failed|unexpected/i.test(txt) ? "ERR-TEXT" : "ok" });
  await page.screenshot({ path: `${out}/10-range-${label}.png`, fullPage: true });
}

// Cumulative toggle
const hasCum = await page.getByRole("button", { name: "Cumulative" }).count();
if (hasCum) {
  await page.getByRole("button", { name: "Cumulative" }).first().click().catch(() => {});
  await page.waitForTimeout(1400);
  results.push({ step: "cumulative", rows: await rowCount() });
  await page.screenshot({ path: `${out}/11-cumulative.png`, fullPage: true });
} else results.push({ step: "cumulative", note: "button absent on Model granularity" });

// Time-series granularity -> line chart present?
await page.getByRole("button", { name: "Savings", exact: true }).first().click().catch(() => {});
await page.waitForTimeout(1600);
results.push({ step: "savings", svgPaths: await page.locator("svg path").count(), rows: await rowCount() });
await page.screenshot({ path: `${out}/12-savings.png`, fullPage: true });

// Phone viewport: text present, no horizontal overflow
const mob = await browser.newPage({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2 });
mob.on("pageerror", e => errors.push(`MOB: ${e.message}`));
await mob.addInitScript(t => localStorage.setItem("ezllm.admin.token", t), token);
await mob.goto(base + "/stats", { waitUntil: "domcontentloaded" });
await mob.waitForTimeout(2200);
results.push({ step: "phone", textLen: await mob.evaluate(() => document.body.innerText.length),
               overflowX: await mob.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth) });
await mob.screenshot({ path: `${out}/13-phone.png`, fullPage: true });

await browser.close();
console.log(JSON.stringify({ results, errors }, null, 1));
process.exit(errors.length ? 2 : 0);
