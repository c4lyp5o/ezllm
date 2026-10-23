// Regression: a grouped rule action fans out one request per rule, so a
// failure partway leaves earlier writes stored. The page must reload what is
// actually persisted and the error must name which rules changed and which
// failed. model-a's write succeeds, model-b's returns 500.
import { start, json, check, finish } from "./mockdash.mjs";

const rule = (id, model_id) => ({ id, account_id: 1, model_id, cap_tokens: 1000, cap_window: "monthly", win_start: "", win_end: "", win_days: "", win_tz: "UTC", enabled: true, note: "" });

async function run(name, act) {
  const persisted = [rule(1, "model-a"), rule(2, "model-b")];
  let reads = 0;
  const page = await dash.page(async (route, url) => {
    const method = route.request().method();
    if (url.pathname === "/admin/model-rules" && method === "GET") return reads++, json(route, persisted), true;
    const m = url.pathname.match(/^\/admin\/model-rules\/(\d+)$/);
    if (!m) return false;
    const i = persisted.findIndex((r) => r.id === Number(m[1]));
    if (persisted[i]?.model_id === "model-b") {
      return json(route, { error: { type: "api_error", message: "SECOND_RULE_FAILED", code: 500 } }, 500), true;
    }
    if (method === "PATCH") Object.assign(persisted[i], route.request().postDataJSON());
    if (method === "DELETE") persisted.splice(i, 1);
    return json(route, method === "DELETE" ? null : persisted[i]), true;
  });
  await page.goto(dash.base + "/rules");
  await page.getByRole("switch", { name: "enable model-a, model-b", exact: true }).waitFor();
  const before = reads;
  await act(page);
  await page.getByText("SECOND_RULE_FAILED", { exact: false }).first().waitFor();
  await page.waitForTimeout(500);
  const msg = await page.getByText("SECOND_RULE_FAILED", { exact: false }).first().innerText();
  check(`${name}: persisted state reloaded`, reads > before, { reads: reads - before });
  check(`${name}: error names the changed and failed rules`, msg.includes("model-a") && msg.includes("model-b"), msg);
  return { page, persisted };
}

const dash = await start();
try {
  {
    const { page, persisted } = await run("toggle", (p) => p.getByRole("switch", { name: "enable model-a, model-b", exact: true }).click());
    // The two rules now differ, so they no longer render as one group.
    const a = await page.getByRole("switch", { name: "enable model-a", exact: true }).getAttribute("aria-checked").catch(() => null);
    const b = await page.getByRole("switch", { name: "enable model-b", exact: true }).getAttribute("aria-checked").catch(() => null);
    check("toggle: switches show stored state", a === "false" && b === "true", { a, b, stored: persisted.map((r) => r.enabled) });
    await page.context().close();
  }
  {
    const { page, persisted } = await run("delete", async (p) => {
      p.once("dialog", (d) => d.accept());
      await p.getByRole("button", { name: /delete/i }).first().click();
      const confirm = p.getByRole("dialog").getByRole("button", { name: /delete/i });
      if (await confirm.count()) await confirm.first().click();
    });
    const shownA = await page.getByText("model-a", { exact: true }).count();
    check("delete: removed rule no longer shown", persisted.length === 1 && shownA === 0, { stored: persisted.map((r) => r.model_id), shownA });
    await page.context().close();
  }
  {
    const { page, persisted } = await run("save", async (p) => {
      await p.getByRole("button", { name: "Edit", exact: true }).first().click();
      await p.locator("#rl-cap").fill("5000");
      await p.getByRole("button", { name: "Save rule", exact: true }).click();
    });
    const shown = await page.getByText("5,000", { exact: false }).count();
    check("save: stored cap shown behind the modal", persisted[0].cap_tokens === 5000 && shown > 0, { stored: persisted.map((r) => r.cap_tokens), shown });
    await page.context().close();
  }
} finally { await dash.close(); }
finish();
