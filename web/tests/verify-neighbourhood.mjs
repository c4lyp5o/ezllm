// Neighbourhood proof: serves the BUILT dashboard against fixtures and checks
// that the scene tells the truth — brightness follows tokens per minute,
// disabled keys go dark, a failing key draws a storm, the table prints exact
// figures, and reduced motion still renders the whole scene.
// Build first (`npm run build`), then: node tests/verify-neighbourhood.mjs
import { start, json, check, finish, account } from "./mockdash.mjs";

const key = (id, label, hint, enabled = true) => ({ id, label, hint, enabled, last_test_ok: true });
const accounts = [
  { ...account, id: 1, name: "Alpha", namespace: "alpha", keys: [key(1, "busy", "sk…aaaa"), key(2, "quiet", "sk…bbbb"), key(3, "retired", "sk…cccc", false)] },
  { ...account, id: 2, name: "Beta", namespace: "beta", keys: [key(4, "flaky", "sk…dddd")] },
];
const row = (k, calls, tin, tout, errors) => ({ k, calls, tin, tout, errors, cread: 0, cwrite: 0, reasoning: 0, saved: 0, context_pre: 0, context_saved: 0, p50_ttft_ms: 0, applied_saved: 0 });
const usage = { groups: [row("sk…aaaa", 40, 90000, 30000, 0), row("sk…dddd", 8, 4000, 1000, 4), row("sk…cccc", 3, 999, 1, 0)], totals: {} };

const handler = async (route, url) => {
  if (url.pathname === "/admin/accounts") { await json(route, accounts); return true; }
  if (url.pathname === "/admin/usage") { await json(route, usage); return true; }
  return false;
};

const t = await start();
for (const reduced of [false, true]) {
  const page = await t.page(handler);
  if (reduced) await page.emulateMedia({ reducedMotion: "reduce" });
  await page.goto(`${t.base}/neighbourhood`);
  await page.waitForSelector("[data-house]");
  const tag = reduced ? " (reduced motion)" : "";
  const houses = await page.$$eval("[data-house]", (els) => els.map((e) => ({ id: e.dataset.house, state: e.dataset.state, level: Number(e.dataset.level), storm: e.dataset.storm })));
  const by = (id) => houses.find((h) => h.id === id);
  check("one house per key" + tag, houses.length === 4, houses.length);
  check("busy key is lit and brightest" + tag, by("1:1")?.state === "live" && by("1:1").level > by("2:4").level && by("1:1").level > 0.9, by("1:1"));
  check("idle key is quiet, level 0" + tag, by("1:2")?.state === "idle" && by("1:2").level === 0, by("1:2"));
  check("disabled key is dark despite ledger rows" + tag, by("1:3")?.state === "off" && by("1:3").level === 0, by("1:3"));
  check("failing key draws a storm" + tag, by("2:4")?.storm === "1" && by("1:1")?.storm === "0", by("2:4"));
  const tableText = await page.locator("table").innerText();
  check("table prints exact figures, unabbreviated" + tag, tableText.includes("24,000") && !/\d(k|K)\b/.test(tableText), tableText.slice(0, 200));
  check("nav entry present" + tag, (await page.getByRole("link", { name: /Neighbourhood/ }).count()) > 0);
  if (!reduced) { await page.waitForTimeout(1800); await page.screenshot({ path: process.env.SHOT ?? "/tmp/neighbourhood.png" }); }
  await page.context().close();
}
await t.close();
finish();
