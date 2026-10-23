// Regression: on each analytics page, a response for an older selection that
// arrives after the newer one must not replace it. The first load is held,
// 7d is picked and answered, then the held response is released.
import { start, json, check, finish, requestsBody } from "./mockdash.mjs";

const usageRow = (k) => ({ k, calls: 1, tin: 10, tout: 5, cread: 0, cwrite: 0, reasoning: 0, saved: 0, errors: 0, context_pre: 0, context_saved: 0, p50_ttft_ms: 0, applied_saved: 0 });
const pages = [
  ["/requests", "/admin/requests", (m) => requestsBody(m)],
  ["/economics", "/admin/requests", (m) => requestsBody(m)],
  ["/stats", "/admin/usage", (m) => ({ from: "", to: "", group_by: "account", groups: [usageRow(m)], totals: usageRow("total") })],
];

const dash = await start();
try {
  for (const [path, endpoint, body] of pages) {
    let release;
    const held = new Promise((r) => (release = r));
    let sawFirst;
    const first = new Promise((r) => (sawFirst = r));
    let n = 0;
    const page = await dash.page(async (route, url) => {
      if (url.pathname !== endpoint) return false;
      const mine = ++n;
      if (mine === 1) { sawFirst(); await held; }
      await json(route, body(mine === 1 ? "OLD_RANGE" : "NEW_RANGE"));
      return true;
    });
    await page.goto(dash.base + path);
    await first;
    await page.getByRole("button", { name: "7d", exact: true }).first().click();
    await page.getByText("NEW_RANGE", { exact: false }).first().waitFor();
    release();
    await page.waitForTimeout(1000);
    const old = await page.getByText("OLD_RANGE", { exact: false }).count();
    const fresh = await page.getByText("NEW_RANGE", { exact: false }).count();
    check(`${path}: newer selection survives a late older response`, old === 0 && fresh > 0, { old, fresh, requests: n });
    await page.context().close();
  }
} finally { await dash.close(); }
finish();
