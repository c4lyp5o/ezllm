// Stats: every range renders the Model breakdown without errors, and the page
// fits a phone with no sideways scroll. Runs against the built web/dist with
// fixtures (see mockdash.mjs).
import { start, json, check, finish } from "./mockdash.mjs";

const t = await start();
const row = (k, o = {}) => ({ k, calls: 3, tin: 300, tout: 60, cread: 0, cwrite: 0, reasoning: 0, saved: 0, errors: 0, context_pre: 0, context_saved: 0, p50_ttft_ms: 120, applied_saved: 0, ...o });
const handler = async (route, url) => {
  if (url.pathname !== "/admin/usage") return false;
  const g = url.searchParams.get("group_by");
  const groups = g === "model" ? [row("model-a"), row("model-b", { calls: 1 })] : g ? [row("mock")] : [];
  await json(route, { groups, totals: row("all", { calls: 4 }), group_by: g ?? "" });
  return true;
};
const errors = [];
const watch = (p) => {
  p.on("pageerror", (e) => errors.push(`PAGEERROR: ${e.message}`));
  p.on("console", (m) => { if (m.type() === "error") errors.push(`CONSOLE: ${m.text()}`); });
};
try {
  const page = await t.page(handler);
  watch(page);
  await page.goto(t.base + "/stats");
  await page.getByRole("button", { name: "Model", exact: true }).first().click();
  for (const label of ["24h", "7d", "30d", "1y"]) {
    await page.getByRole("button", { name: label, exact: true }).first().click();
    await page.getByText("model-a").first().waitFor();
    const text = await page.evaluate(() => document.body.innerText);
    check(`${label}: model rows shown, no error text`, (await page.locator("table tbody tr").count()) >= 2 && !/failed|unexpected/i.test(text));
  }
  const phone = await t.page(handler, { viewport: { width: 390, height: 844 }, deviceScaleFactor: 2 });
  watch(phone);
  await phone.goto(t.base + "/stats");
  await phone.getByRole("button", { name: "24h", exact: true }).first().waitFor();
  await phone.waitForTimeout(800);
  check("phone: no sideways scroll", await phone.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth));
  check("no page or console errors", errors.length === 0, errors);
} catch (e) {
  check("driver ran to completion", false, String(e));
} finally {
  await t.close();
}
finish();
