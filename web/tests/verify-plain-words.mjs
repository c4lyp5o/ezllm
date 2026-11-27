// Plain words and the Advanced folds: a provider's models show one status by
// default, with per-API results, Force API and Test all behind Advanced (and
// remembered); Add provider derives the provider ID without showing it, and
// the fold says what it holds.
import { start, json, check, finish, account, openAdvanced } from "./mockdash.mjs";

const models = [
  { id: "works-model", chat: true, anthropic: false, responses: null, pin: "" },
  { id: "fresh-model", chat: null, anthropic: null, responses: null, pin: "" },
  { id: "broken-model", chat: false, anthropic: 404, responses: null, pin: "" },
  { id: "forced-model", chat: true, anthropic: null, responses: null, pin: "anthropic" },
];
const posts = [];

const t = await start();
const page = await t.page(async (route, url) => {
  const p = url.pathname, m = route.request().method();
  if (p === "/admin/accounts" && m === "GET") { await json(route, [{ ...account, models_count: models.length }]); return true; }
  if (p === "/admin/accounts/1/models") { await json(route, models); return true; }
  if (/\/sweep$/.test(p)) { await json(route, { running: false }); return true; }
  if (p === "/admin/accounts" && m === "POST") {
    posts.push(route.request().postDataJSON());
    await json(route, { error: { type: "invalid_request_error", message: "stop here", code: "test" } }, 400);
    return true;
  }
  return false;
});

const openModels = async () => {
  await page.goto(`${t.base}/providers`);
  await page.locator("button[aria-expanded]").filter({ hasText: account.name }).first().click();
  await page.getByText("works-model").waitFor();
};

await openModels();
const row = (id) => page.locator("tr", { hasText: id });
check("a model that passed on any API reads Works", (await row("works-model").getByText("Works", { exact: true }).count()) === 1);
check("an untested model reads Not tested", (await row("fresh-model").getByText("Not tested", { exact: true }).count()) === 1);
check("a model that failed everywhere reads Doesn't work", (await row("broken-model").getByText("Doesn't work", { exact: true }).count()) === 1);
check("a forced API is named on the row", (await row("forced-model").getByText(/always Anthropic Messages/).count()) === 1);
check("per-API results, Force API and Test all start hidden",
  (await page.getByRole("columnheader", { name: "Force API" }).count()) === 0
  && (await page.getByLabel("API for works-model").count()) === 0
  && (await page.getByRole("button", { name: "Test all models" }).count()) === 0);
check("no old words in the model list", (await page.getByText(/probe|Probed|Surface|Protocols/).count()) === 0);

await page.getByRole("switch", { name: "Advanced" }).click();
check("Advanced shows the APIs and Force API columns",
  (await page.getByRole("columnheader", { name: "APIs" }).count()) === 1
  && (await page.getByRole("columnheader", { name: "Force API" }).count()) === 1
  && (await page.getByLabel("API for forced-model").inputValue()) === "anthropic");
check("Advanced shows Test all models", (await page.getByRole("button", { name: "Test all models" }).count()) === 1);

await openModels();
check("the Advanced switch is remembered", (await page.getByRole("columnheader", { name: "Force API" }).count()) === 1);

// Add provider: the ID follows the name and stays folded away.
await page.goto(`${t.base}/providers`);
await page.getByRole("button", { name: /Add provider/ }).first().click();
await page.selectOption("#ap-kind", "openai-compatible");
await page.fill("#ap-name", "Acme Labs");
const fold = page.locator('details[data-advanced="add-provider"]');
check("the provider ID field starts folded away", !(await page.locator("#ap-ns").isVisible()));
check("the fold names the derived provider ID", (await fold.locator("summary").innerText()).includes("provider ID acme-labs"));
check("the key field says whose key", await page.getByLabel("Provider key").isVisible());
await page.fill("#ap-url", "https://api.example.com/v1");
await page.getByLabel("Provider key").fill("sk-test");
await page.getByRole("button", { name: "Save & validate key" }).click();
await page.getByText("stop here").waitFor();
check("the derived ID is saved without opening the fold", posts.at(-1)?.namespace === "acme-labs", posts.at(-1));

await openAdvanced(page, "add-provider");
check("opening the fold shows the ID", (await page.inputValue("#ap-ns")) === "acme-labs");
// Close it, then make the ID invalid: the fold must open itself to show why
// Save is off.
await page.fill("#ap-ns", "");
await fold.locator("summary").click();
await page.waitForTimeout(150);
check("an ID that needs fixing keeps the fold open", await page.locator("#ap-ns").isVisible());

await t.close();
finish();
