// Subscription-bridge presets in Add provider: serves the BUILT dashboard
// against fixtures and checks that a running bridge needs no key (the add
// names `bridge`, never an api_key, and declares the usage convention), and
// that a bridge that is off says how to turn it on.
// Build first (`npm run build`), then: node tests/verify-bridges.mjs
import { start, json, check, finish } from "./mockdash.mjs";

const bridges = [
  { id: "claude-sub", label: "Claude subscription (bridge)", kind: "anthropic-compatible", base_url: "http://127.0.0.1:8790/v1",
    usage_convention: "anthropic", enabled: true, env: "EZLLM_CLAUDE_BRIDGE", runbook: "docs/claude-bridge-runbook.md",
    status: { cli_version: "2.1.306 (Claude Code)", login: "logged-in", subscription: "Subscription: max" } },
  { id: "chatgpt-sub", label: "ChatGPT subscription (bridge)", kind: "openai-compatible", base_url: "http://127.0.0.1:8791/v1",
    usage_convention: "auto", enabled: false, env: "EZLLM_CODEX_BRIDGE", runbook: "docs/codex-bridge-runbook.md" },
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
await page.goto(`${t.base}/providers`);
await page.getByRole("button", { name: /Add provider/ }).click();
const options = await page.$$eval("#ap-kind option", (os) => os.map((o) => o.value));
check("both bridge presets offered", options.includes("claude-sub") && options.includes("chatgpt-sub"), options);

await page.selectOption("#ap-kind", "chatgpt-sub");
await page.getByText("This bridge is not running").waitFor();
const off = await page.locator("text=EZLLM_CODEX_BRIDGE").count();
check("off bridge names the variable that turns it on", off > 0);
check("off bridge links its runbook", (await page.locator('a[href$="docs/codex-bridge-runbook.md"]').count()) === 1);
check("save disabled while the bridge is off", await page.getByRole("button", { name: /Save & validate/ }).isDisabled());

// Fresh dialog for the running bridge so the name prefill applies.
await page.keyboard.press("Escape");
await page.getByRole("button", { name: /Add provider/ }).click();
await page.selectOption("#ap-kind", "claude-sub");
await page.getByText("Bridge running at").waitFor();
check("running bridge shows its boot facts", (await page.locator("text=2.1.306 (Claude Code)").count()) === 1);
check("key field hidden for a bridge", !(await page.locator("#ap-key").isVisible()));
check("name and namespace prefilled", (await page.inputValue("#ap-name")) === "Claude subscription" && (await page.inputValue("#ap-ns")) === "claude-sub");
await page.screenshot({ path: process.env.SHOT ?? "/tmp/bridges.png" });
await page.getByRole("button", { name: /Save & validate/ }).click();
await page.waitForTimeout(800);
const acct = posts.find((p) => p.path === "/admin/accounts")?.body;
const key = posts.find((p) => p.path.endsWith("/keys"))?.body;
check("account points at the bridge with the anthropic convention",
  acct?.base_url === "http://127.0.0.1:8790/v1" && acct?.kind === "anthropic-compatible" && acct?.usage_convention === "anthropic", acct);
check("key add names the bridge, sends no api_key", key?.bridge === "claude-sub" && !("api_key" in (key ?? {})), key);
await t.close();
finish();
