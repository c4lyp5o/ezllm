// Get started with a subscription instead of a key: each plan's card shows
// where its bridge stands (ready, needs sign-in, not set up), Check again reads
// the status afresh, and Connect adds the provider with the bridge, no key.
import { start, json, check, finish } from "./mockdash.mjs";

const docs = "https://github.com/c4lyp5o/ezllm/blob/vtest/";
const bridge = (id, env, setup, o = {}) => ({ id, label: id, kind: "openai-compatible", base_url: `http://127.0.0.1:87${id.length}/v1`,
  usage_convention: "", enabled: false, env, setup, runbook: "docs/x-runbook.md", docs, ...o });
let bridges = [
  bridge("claude-sub", "EZLLM_CLAUDE_BRIDGE", "docs/bridges/claude.md", { enabled: true, status: { login: "logged in (Max)" } }),
  bridge("chatgpt-sub", "EZLLM_CODEX_BRIDGE", "docs/bridges/codex.md"),
  bridge("copilot-sub", "EZLLM_COPILOT_BRIDGE", "docs/bridges/copilot.md", { enabled: true, status: { login: "not logged in" } }),
  bridge("antigravity-sub", "EZLLM_AGY_BRIDGE", "docs/bridges/antigravity.md"),
];
let bridgeReads = 0;
let accounts = [];
const posted = { accounts: [], keys: [] };

const t = await start();
const page = await t.page(async (route, url) => {
  const p = url.pathname, m = route.request().method();
  if (p === "/admin/bridges") { bridgeReads++; await json(route, bridges); return true; }
  if (p === "/admin/accounts" && m === "GET") { await json(route, accounts); return true; }
  if (p === "/admin/accounts" && m === "POST") {
    const body = route.request().postDataJSON();
    posted.accounts.push(body);
    const a = { id: 7, ...body, enabled: true, keys: [], quota: null, models_count: 0 };
    accounts = [a];
    await json(route, a, 201);
    return true;
  }
  if (p === "/admin/accounts/7/keys") {
    posted.keys.push(route.request().postDataJSON());
    await json(route, { key: { id: 1, label: "primary", hint: "bridge", enabled: true },
      test: { ok: true, steps: [{ step: "format", ok: true }, { step: "auth", ok: true }], catalog: [{ id: "claude-x" }] } }, 201);
    return true;
  }
  return false;
});

await page.goto(`${t.base}/start`);
await page.getByRole("radio", { name: /I have a subscription/ }).or(page.getByRole("button", { name: "I have a subscription" })).first().click();
const plans = page.getByRole("radiogroup", { name: "Subscription" });
await plans.getByText("Needs sign-in").waitFor();
check("four plans are offered", (await plans.getByRole("radio").count()) === 4);
check("each plan shows where it stands",
  (await plans.getByRole("radio", { name: /Claude Pro\/Max.*Ready/ }).count()) === 1
  && (await plans.getByRole("radio", { name: /GitHub Copilot.*Needs sign-in/ }).count()) === 1
  && (await plans.getByRole("radio", { name: /ChatGPT Plus\/Pro.*Not set up/ }).count()) === 1);
check("the platform is said up front", (await page.getByText("The setup is for Linux. On Windows, use WSL.").count()) === 1);

// Not set up: the setting that turns it on, the steps for this version, Check again.
await plans.getByRole("radio", { name: /ChatGPT/ }).click();
check("a plan that isn't set up names the setting", (await page.getByText("EZLLM_CODEX_BRIDGE=on").count()) === 1);
const steps = page.getByRole("link", { name: "Open the steps for ChatGPT Plus/Pro" });
check("it links the steps for this version", (await steps.getAttribute("href")) === docs + "docs/bridges/codex.md");
check("no estimate of how long it takes", (await page.getByText(/minute/).count()) === 0);
bridges = bridges.map((b) => (b.id === "chatgpt-sub" ? { ...b, enabled: true, status: { login: "logged in" } } : b));
const before = bridgeReads;
await page.getByRole("button", { name: "Check again" }).click();
await plans.getByRole("radio", { name: /ChatGPT Plus\/Pro.*Ready/ }).waitFor();
check("Check again reads the status afresh", bridgeReads === before + 1);

// Needs sign-in: the sign-in command for that plan, and a way to connect after.
await plans.getByRole("radio", { name: /GitHub Copilot/ }).click();
check("a signed-out plan shows its sign-in command", (await page.getByText(/copilot login --device-code/).count()) === 1);
check("and a way to connect once signed in", (await page.getByRole("button", { name: "I've signed in, connect" }).count()) === 1);

// Ready: Connect adds the provider with the bridge, never a key.
await plans.getByRole("radio", { name: /Claude Pro\/Max/ }).click();
await page.getByRole("button", { name: "Connect", exact: true }).click();
// Every step title is on the page from the start; step 2's field appears only once step 1 is done.
await page.locator("#start-name").waitFor();
const acct = posted.accounts.at(-1), key = posted.keys.at(-1);
check("the provider is named after the plan", acct?.name === "Claude Pro/Max" && acct?.namespace === "claude-sub", acct);
check("it points at the bridge with the plan's settings", acct?.base_url === bridges[0].base_url && acct?.kind === "anthropic-compatible" && acct?.usage_convention === "anthropic", acct);
check("the key names the bridge and carries no key", key?.bridge === "claude-sub" && !("api_key" in (key ?? {})), key);

await t.close();
finish();
