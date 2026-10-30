// Neighbourhood proof: serves the BUILT dashboard against fixtures and checks
// that the city tells the truth — height and window light follow tokens per
// minute, a disabled provider is boarded up, a failing one gets weather, the
// table prints exact figures, one live call flares exactly one building, and
// reduced motion still renders the whole city.
// Build first (`npm run build`), then: node tests/verify-neighbourhood.mjs
import { start, json, check, finish, account } from "./mockdash.mjs";

const key = (id, label, hint, enabled = true) => ({ id, label, hint, enabled, last_test_ok: true });
const accounts = [
  { ...account, id: 1, name: "Alpha Gateway", namespace: "alpha", keys: [key(1, "busy", "sk…aaaa")] },
  { ...account, id: 2, name: "Beta Works", namespace: "beta", keys: [key(2, "quiet", "sk…bbbb")] },
  { ...account, id: 3, name: "Gamma Cloud", namespace: "gamma", keys: [key(3, "flaky", "sk…cccc")] },
  { ...account, id: 4, name: "Delta Offline", namespace: "delta", keys: [key(4, "retired", "sk…dddd", false)] },
];
const row = (k, calls, tin, tout, errors) => ({ k, calls, tin, tout, errors, cread: 0, cwrite: 0, reasoning: 0, saved: 0, context_pre: 0, context_saved: 0, p50_ttft_ms: 0, applied_saved: 0 });
// The second and later polls report double the tokens, so the page has to show
// what moved since the last update.
let usagePolls = 0;
const usageFor = () => {
  usagePolls += 1;
  const scale = usagePolls; // each poll moves the figures, so a trend appears
  return { groups: [row("sk…aaaa", 40, 90000 * scale, 30000 * scale, 0), row("sk…cccc", 8, 4000, 1000, 4)], totals: {} };
};

// One committed call for Alpha, delivered on the live feeder.
const streamBody = [
  `event: snapshot\ndata: ${JSON.stringify({ calls: [], ts: new Date().toISOString() })}\n\n`,
  `event: calls\ndata: ${JSON.stringify({ calls: [{ ts: new Date().toISOString(), client: "x", surface: "openai", alias: "a", account: "Alpha Gateway", model: "m", status: 200, stream: false, tin: 4000, tout: 500 }], ts: new Date().toISOString() })}\n\n`,
].join("");

const handler = async (route, url) => {
  if (url.pathname === "/admin/accounts") { await json(route, accounts); return true; }
  if (url.pathname === "/admin/usage") { await json(route, usageFor()); return true; }
  if (url.pathname === "/admin/stream") {
    await route.fulfill({ status: 200, contentType: "text/event-stream", body: streamBody });
    return true;
  }
  return false;
};

const t = await start();
// Point at a building's published roof: the least-occluded part of its
// silhouette. Hovering the polygon's centre can land on a nearer tower's hit
// target — a real visitor avoids that by clicking what they can see.
const hoverBuilding = async (page, id) => {
  const [rx, ry] = await page.locator(`[data-building="${id}"]`).evaluate((el) => el.dataset.roof.split(",").map(Number));
  const canvas = page.locator("canvas");
  await canvas.scrollIntoViewIfNeeded();
  const box = await canvas.boundingBox();
  await page.mouse.move(box.x + rx, box.y + ry);
};
for (const reduced of [false, true]) {
  usagePolls = 0;
  const page = await t.page(handler);
  if (reduced) await page.emulateMedia({ reducedMotion: "reduce" });
  await page.goto(`${t.base}/neighbourhood`);
  await page.waitForSelector("[data-building]");
  const tag = reduced ? " (reduced motion)" : "";
  const buildings = await page.$$eval("[data-building]", (els) => els.map((e) => ({
    id: e.dataset.building, state: e.dataset.state, level: Number(e.dataset.level),
    tpm: Number(e.dataset.tpm), storm: e.dataset.storm,
  })));
  const by = (id) => buildings.find((b) => b.id === id);
  check("one building per provider" + tag, buildings.length === 4, buildings.length);
  check("busiest provider is lit and brightest" + tag,
    by("1")?.state === "live" && by("1").level > 0.9 && by("1").level > by("3").level, by("1"));
  check("idle provider is quiet, level 0" + tag, by("2")?.state === "idle" && by("2").level === 0, by("2"));
  check("disabled provider is dark despite ledger rows" + tag, by("4")?.state === "off" && by("4").level === 0, by("4"));
  check("failing provider gets weather" + tag, by("3")?.storm === "1" && by("1")?.storm === "0", by("3"));
  check("every building gets a hit target inside the stage" + tag, await page.evaluate(() => {
    const stage = document.querySelector(".nbhd-city")?.getBoundingClientRect();
    if (!stage) return false;
    return [...document.querySelectorAll("[data-building]")].every((el) => {
      const r = el.getBoundingClientRect();
      return r.width > 0 && r.height > 0 && r.left >= stage.left - 1 && r.right <= stage.right + 1
        && r.top >= stage.top - 1 && r.bottom <= stage.bottom + 1;
    });
  }));

  // The canvas is the city: it must have painted something, and the light on it
  // must be data (a serving provider leaves warm pixels; a dark one does not).
  const painted = await page.evaluate(() => {
    const c = document.querySelector("canvas");
    const ctx = c.getContext("2d");
    const { data } = ctx.getImageData(0, 0, c.width, c.height);
    let solid = 0, warm = 0;
    for (let i = 0; i < data.length; i += 4) {
      if (data[i + 3] > 8) solid++;
      // warm = clearly more red than blue: window light, not sky or asphalt
      if (data[i] > 150 && data[i] > data[i + 2] + 40) warm++;
    }
    return { solid, warm, total: data.length / 4 };
  });
  check("city is painted on the canvas" + tag, painted.solid > painted.total * 0.15, painted);
  check("serving provider puts warm light on the canvas" + tag, painted.warm > 40, painted.warm);

  // Every tower is painted where the plan says it is: sample the canvas at the
  // roof point the scene publishes for each building.
  const roofs = await page.evaluate(() => {
    const c = document.querySelector("canvas");
    const ctx = c.getContext("2d");
    const sx = c.width / c.clientWidth, sy = c.height / c.clientHeight;
    const at = (x, y) => { const d = ctx.getImageData(Math.round(x * sx), Math.round(y * sy), 1, 1).data; return d[3]; };
    const rows = [...document.querySelectorAll("[data-building]")].map((el) => {
      const [x, y] = el.dataset.roof.split(",").map(Number);
      return { id: el.dataset.building, roof: at(x, y) };
    });
    return { rows, sky: at(4, 4) };
  });
  check("every roof is painted at its published point" + tag, roofs.rows.every((r) => r.roof > 40), roofs.rows);
  check("the canvas leaves the sky transparent" + tag, roofs.sky === 0, roofs.sky);

  const tableText = await page.locator("table").innerText();
  check("table prints exact figures, unabbreviated" + tag,
    tableText.includes("24,000") && !/\d(k|K)\b/.test(tableText), tableText.slice(0, 160));
  check("table mirrors the city" + tag, (await page.locator("tbody tr").count()) === 4);

  // Point at a building the scene really exposes. A taller tower in front can
  // cover the one behind it, and the front building rightly gets the click —
  // keyboard focus and the table still reach every building.
  await page.locator("canvas").scrollIntoViewIfNeeded();
  const hoverable = await page.evaluate(() => {
    const c = document.querySelector("canvas").getBoundingClientRect();
    for (const el of document.querySelectorAll("[data-building]")) {
      const [x, y] = el.dataset.roof.split(",").map(Number);
      const hit = document.elementFromPoint(c.left + x, c.top + y);
      if (hit && hit.closest("[data-building]") === el) return el.dataset.building;
    }
    return null;
  });
  check("the scene exposes a building to the pointer" + tag, hoverable !== null, hoverable);
  if (hoverable) {
    const name = accounts.find((a) => String(a.id) === hoverable)?.name ?? "";
    await hoverBuilding(page, hoverable);
    check("hovering a building highlights its table row" + tag,
      await page.locator(`tr[data-row="${hoverable}"]`).evaluate((e) => e.className.includes("bg-hover")));
    check("inspector names the hovered provider" + tag,
      (await page.getByLabel("Selected building").innerText()).includes(name));
  }
  await page.locator('[data-building="4"]').focus();
  check("disabled provider says so in the inspector" + tag,
    (await page.getByLabel("Selected building").innerText()).includes("disabled"));

  await page.locator('[data-building="2"]').focus();
  await page.keyboard.press("Enter");
  check("keyboard selects a building" + tag,
    (await page.locator('[data-building="2"]').getAttribute("aria-pressed")) === "true");

  if (!reduced) {
    await page.locator('[data-building="1"]').focus();
    const frames = Number(await page.locator("canvas").getAttribute("data-frames"));
    const before = await page.locator("canvas").evaluate((c) => c.toDataURL());
    await page.waitForTimeout(900);
    const after = await page.locator("canvas").evaluate((c) => c.toDataURL());
    check("traffic animates the canvas", before !== after);
    check("the frame loop is running", Number(await page.locator("canvas").getAttribute("data-frames")) > frames);
    check("a live call flares its provider",
      Number(await page.locator("canvas").getAttribute("data-pulses")) >= 1,
      await page.locator("canvas").getAttribute("data-pulses"));
    check("live request rate is shown", /requests \/ min/.test(await page.locator(".nbhd-toolbar").innerText()));

    // The scene has a life of its own: people on the pavements, a flight
    // overhead, weather that is occasional rather than constant. The dataset
    // counters are the contract the drivers read.
    const peds = Number(await page.locator("canvas").getAttribute("data-peds"));
    check("people walk the pavements", peds >= 4, peds);
    await page.waitForFunction(() => Number(document.querySelector("canvas").dataset.planes) >= 1, null, { timeout: 25_000 });
    check("a plane crosses the sky", Number(await page.locator("canvas").getAttribute("data-planes")) >= 1);
    check("the sky starts clear — rain is occasional",
      (await page.locator("canvas").getAttribute("data-weather")) === "clear",
      await page.locator("canvas").getAttribute("data-weather"));
    check("lit windows throw their light down the street at night",
      Number(await page.locator("canvas").getAttribute("data-reflect")) >= 1,
      await page.locator("canvas").getAttribute("data-reflect"));

    await page.locator('[data-wx="rain"]').click();
    await page.waitForFunction(() => document.querySelector("canvas").dataset.weather === "rain", null, { timeout: 4_000 });
    check("rain can be commanded over the city",
      (await page.locator("canvas").getAttribute("data-weather")) === "rain"
      && (await page.locator(".nbhd-city").getAttribute("data-weather")) === "rain");
    check("rain cools the scene", await page.locator(".nbhd-city-canvas").evaluate((e) => getComputedStyle(e).filter.includes("saturate")));
    await page.locator('[data-wx="clear"]').click();
    await page.waitForFunction(() => document.querySelector("canvas").dataset.weather === "clear", null, { timeout: 4_000 });
    check("the sky can be held clear", (await page.locator("canvas").getAttribute("data-weather")) === "clear");
    await page.locator('[data-wx="auto"]').click();

    // The trend column: a figure that moved since the last update leaves a
    // direction beside it, in the table and in the inspector.
    await page.evaluate(() => {
      Object.defineProperty(document, "hidden", { value: false, configurable: true });
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await page.waitForFunction(() => /[▲▼]/.test(document.querySelector("table tbody").innerText), null, { timeout: 6_000 });
    check("the table shows what moved since the last update",
      /[▲▼]/.test(await page.locator("table tbody").innerText()));
    await page.locator('[data-building="1"]').focus();
    check("the inspector names the change",
      /up since the last update/.test(await page.getByLabel("Selected building").innerText()));

    await page.getByRole("button", { name: "Pause traffic", exact: true }).click();
    await page.mouse.move(2, 2);
    await page.waitForTimeout(250);
    const a = await page.locator("canvas").evaluate((c) => c.toDataURL());
    await page.waitForTimeout(600);
    const b = await page.locator("canvas").evaluate((c) => c.toDataURL());
    check("paused scene stops drawing", a === b);
    await page.getByRole("button", { name: "Resume traffic", exact: true }).click();
  }
  check("no page overflow" + tag, await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
  check("legend and refresh time shown" + tag, /updated/.test(await page.locator(".nbhd ul", { hasText: "updated" }).first().innerText()));
  check("nav entry present" + tag, (await page.getByRole("link", { name: /Neighbourhood/ }).count()) > 0);
  if (!reduced) {
    await page.waitForTimeout(400);
    await page.screenshot({ path: process.env.SHOT ?? "/tmp/neighbourhood.png" });
  }
  await page.context().close();
}
// Daylight: the window reflections on the street are a night effect. A wet day
// keeps its own sheen, but clear daylight carries no light pools.
{
  usagePolls = 0;
  const page = await t.page(handler);
  await page.addInitScript(() => localStorage.setItem("ezllm.theme", "light"));
  await page.goto(`${t.base}/neighbourhood`);
  await page.waitForSelector("[data-building]");
  await page.waitForTimeout(1_300);
  check("daylight streets carry no window reflections",
    Number(await page.locator("canvas").getAttribute("data-reflect")) === 0,
    await page.locator("canvas").getAttribute("data-reflect"));
  await page.context().close();
}

await t.close();
finish();
