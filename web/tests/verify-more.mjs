// The extras: the sidebar's More group and its "in use" label, each extra's
// introduction until it's set up, the Dashboard's "What else ezllm does" card
// (after Get started, until closed), and Connect with the address first and a
// long key list cut to its first ten.
import { start, json, check, finish, account } from "./mockdash.mjs";

const iso = (ms) => new Date(Date.now() + ms).toISOString();
const tok = (id, name, o = {}) => ({ id, name, hint: `ez-…${id}`, roles: ["infer"], enabled: true, cap_window: "monthly", cap_tokens: 0,
  allowed_models: "", expires_at: "", last_used_at: iso(-3600e3), created_at: iso(-30 * 864e5), ...o });
const tokens = Array.from({ length: 25 }, (_, i) => tok(i + 1, `app-${String(i + 1).padStart(2, "0")}`));
let rules = [];

const t = await start();
const page = await t.page(async (route, url) => {
  const p = url.pathname;
  if (p === "/admin/accounts") { await json(route, [account]); return true; }
  if (p === "/admin/tokens") { await json(route, tokens); return true; }
  if (p === "/admin/compression-profiles") { await json(route, []); return true; }
  if (p === "/admin/combos") { await json(route, []); return true; }
  if (p === "/admin/model-rules") { await json(route, rules); return true; }
  if (p === "/admin/services") { await json(route, { services: [] }); return true; }
  if (p === "/admin/overview") {
    await json(route, { health: { status: "ok", version: "test", schema_version: 39, accounts: 1, provider_keys: 1, models: 2, combos: 0, ledger_rows: 0, dropped_rows: 0, uptime_s: 60 },
      usage: [], usage_by_surface: [], accounts: [account], recent_calls: [], block_cursors: [] });
    return true;
  }
  return false;
});

// Sidebar: core first, then More, then Settings.
await page.goto(`${t.base}/`);
const nav = page.getByRole("navigation", { name: "Main" });
await nav.getByText("More", { exact: true }).waitFor();
const order = (await nav.locator("a, div").allInnerTexts()).map((s) => s.trim().split("\n")[0]).filter(Boolean);
const at = (label) => order.findIndex((x) => x.toLowerCase() === label.toLowerCase());
check("the More heading is there", at("More") > 0, order);
check("core pages come before More", at("Connect") < at("More") && at("Analytics") < at("More"), order);
check("the extras sit under More, Settings last", ["Compression", "Rules", "Services", "Chat", "Neighbourhood"].every((l) => at(l) > at("More") && at(l) < at("Settings")), order);
check("nothing set up, nothing says in use", (await nav.getByText("in use").count()) === 0);

// Dashboard card: Get started is done (a key was used), so it shows.
const card = page.getByRole("region", { name: "What else ezllm does" });
await card.waitFor();
check("the card lists all five extras", (await card.getByRole("link").count()) === 5);
check("set-up extras say not set up, tools say nothing", (await card.getByText("not set up").count()) === 3);
await card.getByRole("button", { name: "Hide what else ezllm does" }).click();
await page.reload();
await page.getByRole("navigation", { name: "Main" }).waitFor();
await page.waitForTimeout(500);
check("a closed card stays closed", (await page.getByRole("region", { name: "What else ezllm does" }).count()) === 0);

// Each extra explains itself until it's set up.
await page.goto(`${t.base}/compression`);
const intro = page.getByRole("region", { name: "About Compression" });
await intro.waitFor();
check("compression explains what it does", (await intro.getByText(/Long chats resend the same text/).count()) === 1);
await intro.getByRole("button", { name: "Make a profile" }).click();
check("Make a profile opens the editor", await page.getByRole("dialog").isVisible());
await page.keyboard.press("Escape");

await page.goto(`${t.base}/rules`);
await page.getByRole("region", { name: "About Rules" }).waitFor();
check("rules explains what it does", (await page.getByText(/before anything is sent or billed/).count()) === 1);

await page.goto(`${t.base}/neighbourhood`);
const nb = page.getByRole("region", { name: "About Neighbourhood" });
await nb.waitFor();
await nb.getByRole("button", { name: /Hide the note/ }).click();
await page.reload();
await page.waitForTimeout(800);
check("a closed tool note stays closed", (await page.getByRole("region", { name: "About Neighbourhood" }).count()) === 0);

// A rule turned on: the sidebar says Rules is in use, and its page skips the intro.
rules = [{ id: 1, account_id: 1, model_id: "model-a", cap_tokens: 1000, cap_window: "daily", win_tz: "UTC", enabled: true, updated_at: iso(0) }];
await page.waitForTimeout(5200); // past the shared five-second read
await page.goto(`${t.base}/rules`);
await page.waitForTimeout(800);
const rulesLink = page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: /Rules/ });
check("an extra in use says so in the sidebar", (await rulesLink.innerText()).includes("in use"));
check("a set-up extra skips its intro", (await page.getByRole("region", { name: "About Rules" }).count()) === 0);

// Connect: the address first, then ten keys and a way to see the rest.
await page.goto(`${t.base}/connect`);
await page.getByText("app-01").waitFor();
const text = await page.locator("main").innerText();
check("the address comes before the keys", text.indexOf("Connect a client") >= 0 && text.indexOf("Connect a client") < text.indexOf("Your keys"));
check("a long list shows its first ten keys", (await page.getByText(/^app-\d\d$/).count()) === 10);
await page.getByRole("button", { name: "Show all 25 keys" }).click();
check("Show all shows every key", (await page.getByText(/^app-\d\d$/).count()) === 25);
await page.reload();
await page.getByText("app-01").waitFor();
await page.getByLabel("Search keys").fill("app-2");
check("search looks through every key", (await page.getByText(/^app-2\d$/).count()) === 6);

await t.close();
finish();
