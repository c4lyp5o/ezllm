// Stats range chips: each range asks /admin/usage for its own look-back.
// Runs against the built web/dist with fixtures (see mockdash.mjs).
import { start, check, finish } from "./mockdash.mjs";

const t = await start();
const froms = [];
const page = await t.page(async (route, url) => {
  if (url.pathname === "/admin/usage") froms.push(url.searchParams.get("from"));
  return false;
});
try {
  await page.goto(t.base + "/stats");
  await page.getByRole("button", { name: "24h", exact: true }).first().waitFor();
  const hours = {};
  for (const label of ["7d", "30d", "1y", "24h"]) { // 24h is the default, so it goes last
    const before = froms.length;
    await page.getByRole("button", { name: label, exact: true }).first().click();
    await page.waitForTimeout(600);
    const asked = froms.slice(before).map((f) => Math.round((Date.now() - Date.parse(f)) / 3600e3));
    hours[label] = Math.min(...asked);
  }
  check("each range asks for its own look-back", hours["24h"] === 24 && hours["7d"] === 168 && hours["30d"] === 720 && hours["1y"] >= 8759 && hours["1y"] <= 8785, hours);
} catch (e) {
  check("driver ran to completion", false, String(e));
} finally {
  await t.close();
}
finish();
