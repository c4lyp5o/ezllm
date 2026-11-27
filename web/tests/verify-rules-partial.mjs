// Regression: a grouped rule action is one transaction (POST
// /admin/model-rules/batch): every rule saves or none does. When the batch
// fails, nothing is stored, so the page must reload and keep showing the
// stored rules unchanged, show the server's error, and keep an open editor's
// work.
import { start, json, check, finish } from "./mockdash.mjs";

const rule = (id, model_id) => ({ id, account_id: 1, model_id, cap_tokens: 1000, cap_window: "monthly", win_start: "", win_end: "", win_days: "", win_tz: "UTC", enabled: true, note: "" });

async function run(name, act) {
  const persisted = [rule(1, "model-a"), rule(2, "model-b")];
  let reads = 0, batches = 0;
  const page = await dash.page(async (route, url) => {
    const method = route.request().method();
    if (url.pathname === "/admin/model-rules" && method === "GET") return reads++, json(route, persisted), true;
    if (url.pathname === "/admin/model-rules/batch" && method === "POST") {
      batches++;
      return json(route, { error: { type: "api_error", message: "BATCH_FAILED", code: 500 } }, 500), true;
    }
    return false;
  });
  await page.goto(dash.base + "/rules");
  await page.getByRole("switch", { name: "enable model-a, model-b", exact: true }).waitFor();
  const before = reads;
  await act(page);
  await page.getByText("BATCH_FAILED", { exact: false }).first().waitFor();
  await page.waitForTimeout(500);
  check(`${name}: one batch, no per-rule writes`, batches === 1, { batches });
  check(`${name}: the stored rules are untouched`, persisted.length === 2 && persisted.every((r) => r.enabled && r.cap_tokens === 1000));
  return { page, reads: reads - before };
}

const dash = await start();
try {
  {
    const { page, reads } = await run("toggle", (p) => p.getByRole("switch", { name: "enable model-a, model-b", exact: true }).click());
    check("toggle: stored state reloaded", reads > 0, { reads });
    const on = await page.getByRole("switch", { name: "enable model-a, model-b", exact: true }).getAttribute("aria-checked");
    check("toggle: the group still shows as one, still on", on === "true", { on });
    await page.context().close();
  }
  {
    const { page, reads } = await run("delete", async (p) => {
      await p.getByRole("button", { name: /delete/i }).first().click();
      await p.getByRole("dialog").getByRole("button", { name: /delete/i }).first().click();
    });
    check("delete: stored state reloaded", reads > 0, { reads });
    check("delete: both rules still shown", (await page.getByRole("switch", { name: "enable model-a, model-b", exact: true }).count()) === 1);
    await page.context().close();
  }
  {
    const { page } = await run("save", async (p) => {
      await p.getByRole("button", { name: "Edit", exact: true }).first().click();
      await p.locator("#rl-cap").fill("5000");
      await p.getByRole("button", { name: "Save rule", exact: true }).click();
    });
    check("save: the editor stays open with the typed cap", (await page.getByRole("dialog").count()) === 1 && (await page.locator("#rl-cap").inputValue()) === "5000");
    await page.context().close();
  }
} finally { await dash.close(); }
finish();
