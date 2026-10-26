// Shared harness for the dashboard regression drivers: serves the built
// web/dist (SPA fallback) on a free loopback port and answers every API call
// from in-process fixtures, so a driver needs no gateway, credentials or
// provider. Build first (`npm run build`); the drivers test what ships.
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { extname, join, normalize } from "node:path";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright";

const dist = fileURLToPath(new URL("../dist/", import.meta.url));
const TYPES = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".woff2": "font/woff2" };

export const account = {
  id: 1, name: "Mock provider", namespace: "mock", kind: "openai-compatible", base_url: "http://mock.invalid",
  enabled: true, requires_session_header: false, quota_mode: "none", cap_window: "monthly", cap_tokens: 0,
  usage_convention: "auto", probe_delay_ms: 400, rate_limit: 0, rate_window: "",
  keys: [{ id: 1, label: "mock", hint: "dummy", enabled: true, last_test_ok: true }], quota: null, models_count: 2,
  protocol_support: { chat: 2, anthropic: 0, responses: 0, untested: 0 },
};
export const models = [
  { id: "model-a", display: "Model A", chat: true, anthropic: false, responses: false, pin: "" },
  { id: "model-b", display: "Model B", chat: true, anthropic: false, responses: false, pin: "" },
];
export const summary = { count: 1, tin: 10, tout: 5, cread: 0, cwrite: 0, reasoning: 0, max_tin: 10, max_tout: 5, context_pre: 0, context_saved: 0, compression_saved: 0, applied_calls: 0, applied_saved: 0, declined_calls: 0, held_notional: 0 };
export const requestsBody = (marker) => ({
  rows: [{ ts: "2026-10-23T00:00:00Z", client: "mock", surface: "openai", alias: marker, account: "mock", model: marker, status: 200, stream: false, tokens_in: 10, tokens_out: 5 }],
  summary, filter: { from: "2026-10-01T00:00:00Z", to: "2026-10-24T00:00:00Z", limit: 100, sort: "ts_desc" },
});

export const json = (route, body, status = 200) =>
  route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });

function defaults(path) {
  switch (path) {
    case "/admin/setup": return { first_boot: false };
    case "/healthz": return { status: "ok", version: "test" };
    case "/admin/accounts": return [account];
    case "/admin/accounts/1/models": return models;
    case "/admin/notifications": return { items: [], unread: 0 };
    case "/admin/tokens": return [];
    case "/admin/usage": return { groups: [], totals: {} };
    case "/admin/requests": return requestsBody("BASELINE");
    case "/admin/cost": return { rows: [], totals: { usd: 0, calls: 0, unpriced_calls: 0, pricing_complete: true } };
    case "/v1/models": return { data: models.map((m) => ({ id: "mock/" + m.id })) };
    default: return [];
  }
}

// start launches the static server and browser. page(handler) opens an
// unlocked dashboard tab; handler(route, url) returns true when it answered.
export async function start() {
  const server = createServer(async (req, res) => {
    const path = normalize(decodeURIComponent(new URL(req.url, "http://x").pathname)).replace(/^(\.\.[/\\])+/, "");
    let file = join(dist, path);
    let body = await readFile(file).catch(() => null);
    if (!body) { file = join(dist, "index.html"); body = await readFile(file); }
    res.writeHead(200, { "content-type": TYPES[extname(file)] ?? "application/octet-stream" });
    res.end(body);
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  const base = `http://127.0.0.1:${server.address().port}`;
  const browser = await chromium.launch();
  return {
    base,
    async page(handler, { unlocked = true, ...contextOptions } = {}) {
      const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, ...contextOptions });
      // Sessions are an httpOnly cookie; the tab only keeps this unlock marker.
      if (unlocked) await context.addInitScript(() => sessionStorage.setItem("ezllm.admin.unlocked", "1"));
      const page = await context.newPage();
      page.on("pageerror", (e) => console.error("PAGEERROR", e.message));
      await page.route("**/*", async (route) => {
        const url = new URL(route.request().url());
        if (url.origin !== base) return route.abort();
        const p = url.pathname;
        if (!p.startsWith("/admin/") && !p.startsWith("/v1/") && p !== "/healthz") return route.continue();
        if (handler && (await handler(route, url))) return;
        return json(route, defaults(p));
      });
      return page;
    },
    async close() { await browser.close(); server.close(); },
  };
}

// check records one assertion; finish exits non-zero if any failed.
const failures = [];
export function check(name, ok, detail) {
  console.log(`${ok ? "PASS" : "FAIL"} ${name}${detail === undefined ? "" : " " + JSON.stringify(detail)}`);
  if (!ok) failures.push(name);
}
export function finish() { process.exit(failures.length ? 1 : 0); }
