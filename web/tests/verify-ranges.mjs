import { chromium } from "playwright";
const base = "http://127.0.0.1:20199", token = process.env.EZLLM_TOKEN_HERMES;

if (!token) { console.error("EZLLM_TOKEN_HERMES not set"); process.exit(3); }
const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1440, height: 1200 } });
const reqs = [];
page.on("request", r => { if (r.url().includes("/admin/usage")) reqs.push(r.url()); });
await page.addInitScript(t => localStorage.setItem("ezllm.admin.token", t), token);
await page.goto(base + "/stats", { waitUntil: "domcontentloaded" });
await page.waitForTimeout(1500);
for (const label of ["24h", "7d", "30d", "1y"]) {
  await page.getByRole("button", { name: label, exact: true }).first().click();
  await page.waitForTimeout(900);
}
await browser.close();
const parsed = reqs.map(u => new URL(u).searchParams.get("from"));
console.log(JSON.stringify({ n: reqs.length, froms: parsed, distinct: new Set(parsed).size }, null, 1));
process.exit(new Set(parsed).size >= 4 ? 0 : 1);
