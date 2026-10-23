// Regression: the Pricing modal's PUT must carry the write field `model` (the
// handler rejects unknown fields, so a read-side `model_id` failed every save),
// and the saved rate must reload into the table.
import { start, json, check, finish } from "./mockdash.mjs";

const dash = await start();
try {
  let payload;
  const saved = [];
  const page = await dash.page(async (route, url) => {
    if (url.pathname === "/admin/prices") return json(route, saved), true;
    if (url.pathname === "/admin/accounts/1/prices" && route.request().method() === "PUT") {
      payload = route.request().postDataJSON();
      // Mirror handleSetPrice: unknown fields and a missing model are 400s.
      const allowed = ["model", "effective_from", "currency", "price_in", "price_out", "price_cache_read", "price_cache_write", "note"];
      const unknown = Object.keys(payload).find((k) => !allowed.includes(k));
      if (unknown || !payload.model) {
        return json(route, { error: { type: "invalid_request_error", message: unknown ? `invalid JSON body: json: unknown field "${unknown}"` : "model is required", code: 400 } }, 400), true;
      }
      const row = { account_id: 1, model_id: payload.model, effective_from: payload.effective_from, currency: "USD", price_in: payload.price_in, price_out: payload.price_out, price_cache_read: payload.price_cache_read, price_cache_write: payload.price_cache_write, note: payload.note };
      saved.push(row);
      return json(route, row), true;
    }
  });
  await page.goto(dash.base + "/providers");
  await page.getByText("Mock provider", { exact: true }).click();
  await page.getByRole("button", { name: "Pricing", exact: true }).click();
  await page.getByRole("combobox", { name: "model", exact: true }).selectOption("model-a");
  for (const [key, v] of [["price_in", "1.25"], ["price_out", "10"], ["price_cache_read", "0.125"], ["price_cache_write", "0"]]) {
    await page.getByRole("textbox", { name: key, exact: true }).fill(v);
  }
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await page.waitForTimeout(800);
  check("PUT sends model, not model_id", payload?.model === "model-a" && !("model_id" in (payload ?? {})), payload);
  check("no error shown", (await page.getByText("unknown field", { exact: false }).count()) === 0);
  check("saved rate reloads", (await page.getByRole("dialog").getByText("$1.25", { exact: false }).count()) > 0
    || (await page.getByRole("dialog").getByText("1.25", { exact: false }).count()) > 0);
} finally { await dash.close(); }
finish();
