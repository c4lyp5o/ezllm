// Neighbourhood scene (docs/design/neighbourhood-design.md): failing providers
// stand in the front row, heights follow traffic linearly, every building has a
// sign that nothing covers, red appears on the canvas only when something is
// failing, both modes and both scales work, a still scene stops drawing, and
// the phone layout fits. Runs against the built web/dist (see mockdash.mjs).
import { start, json, check, finish, account } from "./mockdash.mjs";

const key = (id, hint, enabled = true) => ({ id, label: `key ${id}`, hint, enabled, last_test_ok: true });
const acct = (id, name, ns, o = {}) => ({ ...account, id, name, namespace: ns, keys: [key(id, `h${id}`)], ...o });
const accounts = [
  acct(1, "OpenRouter", "openrouter"), acct(2, "DeepSeek", "deepseek"), acct(3, "Claude Pro/Max", "claude-sub"),
  acct(4, "Groq", "groq"), acct(5, "OpenAI Platform", "openai"), acct(6, "OpenCode Go", "opencode-go"),
  acct(7, "Ollama (local)", "ollama", { enabled: false }),
];
// tin is tokens over five minutes, so tokens/min = tin / 5.
const row = (k, tpm, calls, errors) => ({ k, calls, tin: tpm * 5, tout: 0, errors, cread: 0, cwrite: 0, reasoning: 0, saved: 0, context_pre: 0, context_saved: 0, p50_ttft_ms: 0, applied_saved: 0 });
const BUSY = [row("openrouter", 198000, 120, 2), row("deepseek", 64000, 40, 0), row("claude-sub", 46000, 12, 0), row("groq", 9600, 30, 12), row("openai", 220, 4, 4)];
const CALM = BUSY.map((g) => ({ ...g, errors: 0 }));

let groups = BUSY, usageFails = false;
const handler = async (route, url) => {
  if (url.pathname === "/admin/accounts") { await json(route, accounts); return true; }
  if (url.pathname === "/admin/usage") {
    if (usageFails) { await json(route, { error: { message: "down" } }, 503); return true; }
    await json(route, { groups: url.searchParams.get("group_by") === "key" ? [] : groups, totals: {} }); return true;
  }
  return false;
};
const sign = (page, id) => page.locator(`[data-building="${id}"]`);
const attr = async (page, id, a) => sign(page, id).getAttribute(a);
// Red pixels on the canvas: the failing red (#ef4f63) and its dim flicker (#7a2a35).
const redPixels = (page) => page.locator("canvas").evaluate((c) => {
  const d = c.getContext("2d").getImageData(0, 0, c.width, c.height).data;
  let n = 0;
  for (let i = 0; i < d.length; i += 4) {
    const [r, g, b] = [d[i], d[i + 1], d[i + 2]];
    if ((Math.abs(r - 239) < 14 && Math.abs(g - 79) < 14 && Math.abs(b - 99) < 14) || (Math.abs(r - 122) < 10 && Math.abs(g - 42) < 10 && Math.abs(b - 53) < 10)) n++;
  }
  return n;
});
const errors = [];
const t = await start();
const open = async (opts = {}, setup) => {
  const page = await t.page(handler, opts);
  page.on("pageerror", (e) => errors.push(e.message));
  if (setup) await page.addInitScript(setup);
  await page.goto(`${t.base}/neighbourhood`);
  await page.locator("[data-building]").first().waitFor();
  await page.waitForTimeout(600);
  return page;
};

try {
  groups = BUSY;
  const page = await open({}, () => { try { if (!sessionStorage.getItem("cleared")) { localStorage.clear(); sessionStorage.setItem("cleared", "1"); } } catch {} });

  // Rows follow state: failing in front, switched off at the back.
  check("failing providers stand in the front row", (await attr(page, 4, "data-row")) === "0" && (await attr(page, 5, "data-row")) === "0"
    && (await attr(page, 4, "data-health")) === "failing" && (await attr(page, 5, "data-health")) === "failing");
  check("serving providers stand behind them", (await attr(page, 1, "data-row")) === "1" && (await attr(page, 1, "data-health")) === "serving");
  check("a switched-off provider stands at the back", Number(await attr(page, 7, "data-row")) === Math.max(...(await page.locator("[data-building]").evaluateAll((els) => els.map((e) => Number(e.dataset.row))))));

  // Linear height: the busiest sets 200k; 220 tok/min is one storey, not 44% of the tower.
  check("the busiest provider sets the scale, rounded up", (await page.locator(".nbhd-scene").getAttribute("data-ceiling")) === "200000");
  check("heights are linear in tokens per minute", (await attr(page, 1, "data-floors")) === "24" && (await attr(page, 2, "data-floors")) === "8" && (await attr(page, 3, "data-floors")) === "6");
  check("a small provider is one storey, still visible", (await attr(page, 5, "data-floors")) === "1");

  // Signs: one per building, the right words, none overlapping, all inside the scene.
  check("every building has a sign", (await page.locator("[data-building]").count()) === accounts.length);
  check("a failing sign says how much is failing", (await sign(page, 4).innerText()).includes("40% failing") && (await sign(page, 5).innerText()).includes("100% failing"));
  check("a serving sign gives tokens per minute", (await sign(page, 1).innerText()).includes("198,000"));
  check("a switched-off sign says so", (await sign(page, 7).innerText()).includes("switched off") && (await sign(page, 6).innerText()).includes("quiet"));
  const layout = async (p) => p.evaluate(() => {
    const scene = document.querySelector(".nbhd-scene").getBoundingClientRect();
    const r = [...document.querySelectorAll("[data-building]")].map((e) => e.getBoundingClientRect());
    let overlaps = 0;
    for (let i = 0; i < r.length; i++) for (let j = i + 1; j < r.length; j++) {
      if (r[i].left < r[j].right && r[j].left < r[i].right && r[i].top < r[j].bottom && r[j].top < r[i].bottom) overlaps++;
    }
    const outside = r.filter((b) => b.top < scene.top - 1 || b.left < scene.left - 1 || b.right > scene.right + 1 || b.bottom > scene.bottom + 1).length;
    return { overlaps, outside };
  });
  const l1 = await layout(page);
  check("no two signs overlap", l1.overlaps === 0, l1);
  check("every sign is inside the scene", l1.outside === 0, l1);

  // Red means failing: red pixels on the canvas with failures, none without.
  await page.getByRole("group", { name: "City mode" }).getByRole("button", { name: "Status" }).click();
  check("red is drawn while providers fail", (await redPixels(page)) > 50);
  await page.context().close();

  groups = CALM;
  const calm = await open({}, () => { try { localStorage.clear(); localStorage.setItem("ezllm.city.paused", "1"); } catch {} });
  check("nothing failing: no failing signs", (await calm.locator('[data-health="failing"]').count()) === 0);
  check("nothing failing: no red anywhere on the canvas", (await redPixels(calm)) === 0, await redPixels(calm));
  await calm.context().close();

  // Modes and scale.
  groups = BUSY;
  const m = await open({}, () => { try { if (!sessionStorage.getItem("cleared")) { localStorage.clear(); sessionStorage.setItem("cleared", "1"); } } catch {} });
  await m.getByRole("group", { name: "City mode" }).getByRole("button", { name: "Show" }).click();
  await m.waitForTimeout(400);
  check("Show mode is set on the scene", (await m.locator(".nbhd-scene").getAttribute("data-mode")) === "show");
  check("Show mode keeps failing signs full size and shrinks the rest",
    (await attr(m, 4, "data-big")) === "1" && (await attr(m, 2, "data-big")) === "0");
  check("Show mode keeps the same numbers", (await attr(m, 1, "data-floors")) === "24" && (await attr(m, 4, "data-health")) === "failing");
  const scale = m.getByRole("group", { name: "Height scale" });
  await scale.getByLabel("Fixed full height, tokens per minute").fill("100000");
  await scale.getByLabel("Fixed full height, tokens per minute").press("Enter");
  await m.waitForTimeout(300);
  check("a fixed ceiling sets the scale", (await m.locator(".nbhd-scene").getAttribute("data-ceiling")) === "100000");
  check("over a fixed ceiling is full height; the rest scale to it", (await attr(m, 1, "data-floors")) === "24" && (await attr(m, 2, "data-floors")) === "15");
  await m.reload(); await m.locator("[data-building]").first().waitFor();
  check("the fixed ceiling and the mode are remembered", (await m.locator(".nbhd-scene").getAttribute("data-ceiling")) === "100000" && (await m.locator(".nbhd-scene").getAttribute("data-mode")) === "show");
  await m.getByRole("group", { name: "Height scale" }).getByRole("button", { name: "busiest" }).click();
  await m.waitForTimeout(300);
  check("busiest brings the scale back", (await m.locator(".nbhd-scene").getAttribute("data-ceiling")) === "200000");

  // Selecting: a sign selects its building, again deselects.
  await sign(m, 2).click();
  check("clicking a sign selects the building", (await attr(m, 2, "aria-pressed")) === "true" && (await m.getByLabel("Selected building").innerText()).includes("64,000"));
  await sign(m, 2).click();
  check("clicking it again closes the details", (await m.getByLabel("Selected building").count()) === 0);
  await sign(m, 3).focus(); await m.keyboard.press("Enter");
  check("the keyboard selects a building", (await attr(m, 3, "aria-pressed")) === "true");

  // A still scene stops drawing; a moving one keeps drawing.
  const frame = (p) => p.locator("canvas").evaluate((c) => c.toDataURL());
  const a1 = await frame(m); await m.waitForTimeout(400); const a2 = await frame(m);
  check("a moving scene keeps drawing", a1 !== a2);
  await m.getByRole("button", { name: "Pause" }).click();
  await m.waitForTimeout(300);
  const p1 = await frame(m); await m.waitForTimeout(600); const p2 = await frame(m);
  check("a paused scene stops drawing", p1 === p2 && (await m.locator(".nbhd-scene").getAttribute("data-still")) === "1");
  await m.context().close();

  const rm = await open({ reducedMotion: "reduce" }, () => { try { if (!sessionStorage.getItem("cleared")) { localStorage.clear(); sessionStorage.setItem("cleared", "1"); } } catch {} });
  const r1 = await frame(rm); await rm.waitForTimeout(600); const r2 = await frame(rm);
  check("reduced motion draws one still frame", r1 === r2 && (await rm.locator(".nbhd-scene").getAttribute("data-still")) === "1");
  check("reduced motion still shows failure in red", (await redPixels(rm)) > 50);
  await rm.context().close();

  // Light theme: the scene is the same night scene, signs still readable.
  const lt = await open({ colorScheme: "light" }, () => { try { localStorage.clear(); localStorage.setItem("ezllm.theme", "light"); } catch {} });
  const l2 = await layout(lt);
  check("light theme: signs fit and do not overlap", l2.overlaps === 0 && l2.outside === 0, l2);
  await lt.context().close();

  // Phone: two per row, every sign inside, no sideways page scroll.
  const ph = await open({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true }, () => { try { if (!sessionStorage.getItem("cleared")) { localStorage.clear(); sessionStorage.setItem("cleared", "1"); } } catch {} });
  const l3 = await layout(ph);
  check("phone: signs fit and do not overlap", l3.overlaps === 0 && l3.outside === 0, l3);
  check("phone: no sideways scroll", await ph.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth));
  const phoneRows = await ph.locator("[data-building]").evaluateAll((els) => els.map((e) => e.dataset.row));
  check("phone: rows hold at most two buildings", Object.values(phoneRows.reduce((m, r) => ({ ...m, [r]: (m[r] ?? 0) + 1 }), {})).every((n) => n <= 2) && phoneRows.filter((r) => r === "0").length === 2, phoneRows);
  check("phone: failing signs keep their number", (await sign(ph, 4).innerText()).includes("40% failing") && (await attr(ph, 2, "data-big")) === "0");
  await ph.context().close();

  // A crowded city: eight long names at the same height would overlap without nudging.
  const crowdNames = ["Alibaba Cloud Model Studio", "Xiaomi MiMo token plan", "Cloudflare Workers AI", "Vercel AI Gateway", "NVIDIA NIM endpoints", "Google AI Studio keys", "Ollama Cloud tier", "TokenRouter backup"];
  const savedAccounts = accounts.splice(0, accounts.length, ...crowdNames.map((n, i) => acct(20 + i, n, `crowd${i}`)));
  groups = crowdNames.map((_, i) => row(`crowd${i}`, 50000, 10, 0));
  for (const vp of [{ width: 1280, height: 900 }, { width: 390, height: 844 }]) {
    const cr = await open({ viewport: vp }, () => { try { if (!sessionStorage.getItem("cleared")) { localStorage.clear(); sessionStorage.setItem("cleared", "1"); } } catch {} });
    const lc = await layout(cr);
    check(`crowded city at ${vp.width}px: no two signs overlap, all inside`, lc.overlaps === 0 && lc.outside === 0, lc);
    await cr.context().close();
  }
  // 21 providers on a high-density phone (failing and switched off among them):
  // the sign fit settles instead of looping (React "maximum update depth").
  const bigNames = ["Alibaba Token Plan", "Antigravity", "ChatGPT Subscription", "Claude Platform", "Claude Subscription", "Claude Web", "Cloudflare Workers AI", "Command Code", "DeepSeek", "GitHub Copilot", "Globex", "Google AI Studio", "Groq", "Mimo Token Plan", "NVIDIA NIM", "Ollama Cloud", "OpenAI", "OpenCode Go", "OpenRouter", "TokenRouter", "Vercel AI Gateway"];
  const bigTpm = [0, 0, 30000, 12000, 80000, 0, 2000, 45000, 64000, 0, 0, 9000, 9600, 0, 3000, 0, 220, 15000, 198000, 0, 1200];
  accounts.splice(0, accounts.length, ...bigNames.map((n, i) => acct(40 + i, n, `big${i}`, i === 1 ? { enabled: false } : {})));
  groups = bigNames.map((_, i) => row(`big${i}`, bigTpm[i], i === 12 ? 30 : i === 16 ? 4 : Math.round(bigTpm[i] / 2000), i === 12 ? 12 : i === 16 ? 4 : 0));
  const before = errors.length;
  const big = await t.page(handler, { viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true });
  const crash = [];
  big.on("pageerror", (e) => crash.push(e.message));
  big.on("console", (msg) => { if (msg.type() === "error") crash.push(msg.text()); });
  await big.goto(`${t.base}/neighbourhood`);
  await big.waitForTimeout(2500);
  const bigLayout = await big.evaluate(() => ({ scene: !!document.querySelector(".nbhd-scene"), hidden: [...document.querySelectorAll("[data-building]")].filter((e) => e.style.visibility !== "visible").length, signs: document.querySelectorAll("[data-building]").length }));
  check("21 providers on a phone: the scene settles and shows every sign", bigLayout.scene && bigLayout.signs === 21 && bigLayout.hidden === 0 && crash.length === 0, { ...bigLayout, crash: crash.slice(0, 2) });
  await big.context().close();
  errors.length = before;
  accounts.splice(0, accounts.length, ...savedAccounts);
  groups = BUSY;

  // Usage unavailable: buildings are unknown, never quiet.
  usageFails = true;
  const un = await open({}, () => { try { if (!sessionStorage.getItem("cleared")) { localStorage.clear(); sessionStorage.setItem("cleared", "1"); } } catch {} });
  check("no usage: buildings read as unavailable, not quiet", (await attr(un, 1, "data-health")) === "unknown" && (await sign(un, 1).innerText()).includes("usage unavailable"));
  usageFails = false;
  await un.context().close();

  check("no page errors", errors.length === 0, errors);
} catch (e) {
  check("driver ran to completion", false, String(e));
} finally {
  await t.close();
}
finish();
