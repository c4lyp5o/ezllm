// Regression: saving the Policy modal without touching Expires must not send
// expires_at (a date-only value is re-read as end of the server's local day,
// moving the deadline). Changing or clearing the date still sends it.
import { start, json, check, finish } from "./mockdash.mjs";

const token = { id: 1, name: "client", hint: "dummy", roles: ["infer"], enabled: true, cap_window: "monthly", cap_tokens: 1000, allowed_models: "", expires_at: "2026-12-10T08:15:00Z", created_at: "2026-10-01T00:00:00Z" };
const dash = await start();
try {
  for (const [name, edit, want] of [
    ["unchanged expiry is not sent", null, undefined],
    ["changed expiry is sent", "2026-12-20", "2026-12-20"],
    ["cleared expiry is sent", "", ""],
  ]) {
    let posted;
    const page = await dash.page(async (route, url) => {
      if (url.pathname === "/admin/tokens") return json(route, [token]), true;
      if (url.pathname === "/admin/tokens/1" && route.request().method() === "PATCH") {
        posted = route.request().postDataJSON();
        return json(route, { ...token, ...posted }), true;
      }
    });
    await page.goto(dash.base + "/connect");
    await page.getByRole("button", { name: "Policy", exact: true }).click();
    if (edit !== null) await page.getByRole("dialog").locator('input[type="date"]').fill(edit);
    await page.getByRole("button", { name: "Save", exact: true }).click();
    await page.waitForFunction(() => !document.querySelector('[role="dialog"]'));
    check(name, posted !== undefined && posted.expires_at === want && posted.cap_tokens === 1000, posted);
    await page.context().close();
  }
} finally { await dash.close(); }
finish();
