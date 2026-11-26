// Subscription-bridge presets in Add provider: serves the BUILT dashboard
// against fixtures and checks that a running bridge needs no key (the add
// names `bridge`, never an api_key, and declares the usage convention), and
// that a bridge that is off says how to turn it on.
// Build first (`npm run build`), then: node tests/verify-bridges.mjs
import { start, json, check, finish, openAdvanced } from "./mockdash.mjs";

const bridges = [
  { id: "claude-sub", label: "Claude Pro/Max", kind: "anthropic-compatible", base_url: "http://127.0.0.1:8790/v1",
    usage_convention: "anthropic", enabled: true, env: "EZLLM_CLAUDE_BRIDGE", setup: "docs/bridges/claude.md", runbook: "docs/claude-bridge-runbook.md", docs: "https://github.com/c4lyp5o/ezllm/blob/v9.9.9/",
    status: { cli_version: "2.1.306 (Claude Code)", login: "logged-in", subscription: "Subscription: max" } },
  { id: "chatgpt-sub", label: "ChatGPT Plus/Pro", kind: "openai-compatible", base_url: "http://127.0.0.1:8791/v1",
    usage_convention: "auto", enabled: false, env: "EZLLM_CODEX_BRIDGE", setup: "docs/bridges/codex.md", runbook: "docs/codex-bridge-runbook.md", docs: "https://github.com/c4lyp5o/ezllm/blob/v9.9.9/" },
  { id: "antigravity-sub", label: "Google Antigravity", kind: "openai-compatible", base_url: "http://127.0.0.1:8792/v1",
    usage_convention: "auto", enabled: true, env: "EZLLM_AGY_BRIDGE", setup: "docs/bridges/antigravity.md", runbook: "docs/agy-bridge-runbook.md", docs: "https://github.com/c4lyp5o/ezllm/blob/v9.9.9/",
    // the Info map cmd/ezllm/agybridge.go reports
    status: { cli_version: "1.4.7", login: "logged in (token file; models listed at boot)",
      models: "gemini-3.8-flash-low, claude-sonnet-4-6", client_tools: "disabled (text chat only)" } },
];
const posts = [];
const handler = async (route, url) => {
  const req = route.request();
  if (url.pathname === "/admin/bridges") { await json(route, bridges); return true; }
  if (url.pathname === "/admin/accounts" && req.method() === "GET") { await json(route, []); return true; }
  if (url.pathname === "/admin/accounts" && req.method() === "POST") {
    posts.push({ path: url.pathname, body: req.postDataJSON() });
    await json(route, { id: 7, ...req.postDataJSON(), keys: [] }, 201); return true;
  }
  if (url.pathname === "/admin/accounts/7/keys") {
    posts.push({ path: url.pathname, body: req.postDataJSON() });
    await json(route, { key: { id: 1, label: "primary", hint: "…", enabled: true }, test: { ok: true, steps: [] } }, 201); return true;
  }
  return false;
};

const t = await start();
const page = await t.page(handler);
// Closing a dialog with edits in it asks first (a bridge preset fills in the
// name): confirm the discard so the next dialog starts fresh.
const closeDialog = async () => {
  await page.keyboard.press("Escape");
  const discard = page.getByRole("alertdialog", { name: "Discard unsaved changes?" });
  if (await discard.isVisible().catch(() => false)) await discard.getByRole("button", { name: "Discard" }).click();
  await page.locator(".fixed.inset-0.z-50").waitFor({ state: "detached" });
};
await page.goto(`${t.base}/providers`);
await page.getByRole("button", { name: /Add provider/ }).click();
const options = await page.$$eval("#ap-kind option", (os) => os.map((o) => o.value));
check("every bridge preset offered", ["claude-sub", "chatgpt-sub", "antigravity-sub"].every((o) => options.includes(o)), options);

const labels = await page.$$eval("#ap-kind option", (os) => Object.fromEntries(os.map((o) => [o.value, o.textContent])));
check("provider labels use the requested names", labels["claude-sub"] === "Claude Pro/Max (bridge)" && labels["chatgpt-sub"] === "ChatGPT Plus/Pro (bridge)" && labels["antigravity-sub"] === "Google Antigravity (bridge)" && labels.openai === "OpenAI Platform", labels);

await page.selectOption("#ap-kind", "antigravity-sub");
await page.getByText("Bridge running at").waitFor();
check("antigravity bridge lists its plan's models", (await page.locator("text=gemini-3.8-flash-low, claude-sonnet-4-6").count()) === 1);
check("disabled client tools have actionable guidance", (await page.getByText(/Client tool calls are unavailable/).count()) === 1);
check("running bridge links its runbook", (await page.locator('a[href$="docs/agy-bridge-runbook.md"]').count()) >= 1);
check("antigravity login is not misread as logged out", (await page.locator("text=isn't logged in").count()) === 0);
await closeDialog();
await page.getByRole("button", { name: /Add provider/ }).click();

await page.selectOption("#ap-kind", "chatgpt-sub");
await page.getByText("This bridge is not running").waitFor();
const off = await page.locator("text=EZLLM_CODEX_BRIDGE").count();
check("off bridge names the variable that turns it on", off > 0);
check("off bridge explains where to find startup errors", (await page.getByText(/docker compose logs ezllm/).count()) === 1);
check("off bridge links its setup page for this version", (await page.locator('a[href="https://github.com/c4lyp5o/ezllm/blob/v9.9.9/docs/bridges/codex.md"]').count()) === 1);
check("save disabled while the bridge is off", await page.getByRole("button", { name: /Save & validate/ }).isDisabled());

// Fresh dialog for the running bridge so the name prefill applies.
await closeDialog();
await page.getByRole("button", { name: /Add provider/ }).click();
await page.selectOption("#ap-kind", "claude-sub");
await page.getByText("Bridge running at").waitFor();
check("running bridge shows its boot facts", (await page.locator("text=2.1.306 (Claude Code)").count()) === 1);
check("key field hidden for a bridge", !(await page.locator("#ap-key").isVisible()));
await openAdvanced(page, "add-provider");
check("name and provider ID prefilled", (await page.inputValue("#ap-name")) === "Claude Pro/Max" && (await page.inputValue("#ap-ns")) === "claude-sub");
// The bridge supplies the key, but a live balance read needs the operator's
// claude.ai cookie — the field has to exist and reach management_key, which is
// what internal/provider/claudesub_quota.go reads.
check("claude.ai session cookie field is offered", await page.locator("#ap-extra").isVisible());
check("cookie field explains what to paste", (await page.getByText(/5-hour and weekly limits/).count()) === 1);
await page.fill("#ap-extra", "sessionKey=sk-ant-test; lastActiveOrg=org-1");
await page.screenshot({ path: process.env.SHOT ?? "/tmp/bridges.png" });
await page.getByRole("button", { name: /Save & validate/ }).click();
await page.waitForTimeout(800);
const acct = posts.find((p) => p.path === "/admin/accounts")?.body;
const key = posts.find((p) => p.path.endsWith("/keys"))?.body;
check("account points at the bridge with the anthropic convention",
  acct?.base_url === "http://127.0.0.1:8790/v1" && acct?.kind === "anthropic-compatible" && acct?.usage_convention === "anthropic", acct);
check("key add names the bridge, sends no api_key", key?.bridge === "claude-sub" && !("api_key" in (key ?? {})), key);
check("cookie reaches the account as its management key",
  acct?.management_key === "sessionKey=sk-ant-test; lastActiveOrg=org-1", acct?.management_key);
await t.close();
finish();
