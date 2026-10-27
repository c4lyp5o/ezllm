// Screenshot driver for the Neighbourhood city: the built dashboard against a
// fixture that looks like a real gate (21 providers, one busy, one failing,
// one disabled), in both themes and at phone width, plus a frame-rate read.
// Build first (`npm run build`), then: node tests/shot-city.mjs
import { mkdir } from 'node:fs/promises';
import { start, json, check, finish, account } from './mockdash.mjs';

const out = process.env.OUT ?? '/tmp/ezllm-city'; await mkdir(out, { recursive: true });
const names = [
  'Mimo Token Plan', 'Claude Platform', 'Space Stationine', 'Alibaba Token Plan', 'Tokenrouter',
  'Opencode Go', 'Ollama Cloud', 'Command Code', 'Groq', 'Deepseek',
  'Vercel AI Gateway', 'Openrouter', 'NVIDIA NIM', 'Cloudflare Workers AI', 'Google AI Studio',
  'OpenAI', 'Claude Web', 'Claude Subscription', 'ChatGPT Subscription', 'Github Copilot',
  'Antigravity subscription',
];
const key = (id, hint, enabled = true) => ({ id, label: hint, hint, enabled, last_test_ok: true });
const accounts = names.map((name, i) => ({
  ...account, id: i + 1, name, namespace: name.toLowerCase().replace(/[^a-z]+/g, '-'),
  enabled: i !== 20, keys: [key(i + 1, `sk…${String(i).padStart(2, '0')}`, i !== 20)],
}));
// A realistic spread: a hot provider, a few warm, most idle, one bleeding errors.
const tokens = [186652, 74210, 51200, 38040, 22100, 12400, 8100, 4300, 2100, 900, 400, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0];
const usage = {
  groups: accounts.map((a, i) => ({
    k: a.keys[0].hint, calls: Math.round(tokens[i] / 900) + (i === 5 ? 12 : 0),
    tin: Math.round(tokens[i] * 0.8), tout: Math.round(tokens[i] * 0.2),
    errors: i === 5 ? 6 : 0, cread: 0, cwrite: 0, saved: 0, reasoning: 0,
    context_pre: 0, context_saved: 0, p50_ttft_ms: 0, applied_saved: 0,
  })).filter((g) => g.calls > 0),
  totals: {},
};
const handler = async (route, url) => {
  if (url.pathname === '/admin/accounts') { await json(route, accounts); return true; }
  if (url.pathname === '/admin/usage') { await json(route, usage); return true; }
  return false;
};

const dash = await start();
try {
  for (const [theme, viewport] of [['dark', { width: 1440, height: 1000 }], ['light', { width: 1440, height: 1000 }], ['dark', { width: 390, height: 844 }]]) {
    const page = await dash.page(handler, { viewport, hasTouch: viewport.width < 640, isMobile: viewport.width < 640 });
    await page.addInitScript((t) => { localStorage.setItem('ezllm.theme', t); localStorage.setItem('ezllm.city.seed', '4242'); }, theme);
    await page.goto(`${dash.base}/neighbourhood`);
    await page.locator('[data-building]').first().waitFor();
    await page.waitForTimeout(1200);
    // Frames actually drawn per second, measured in the page.
    const fps = await page.evaluate(() => new Promise((resolve) => {
      let n = 0; const t0 = performance.now();
      const tick = () => { n++; if (performance.now() - t0 < 2000) requestAnimationFrame(tick); else resolve(Math.round(n / ((performance.now() - t0) / 1000))); };
      requestAnimationFrame(tick);
    }));
    const geom = await page.evaluate(() => {
      const c = document.querySelector('canvas');
      const r = c.getBoundingClientRect();
      const hits = [...document.querySelectorAll('[data-building]')].map((e) => e.getBoundingClientRect());
      return {
        canvas: [Math.round(r.width), Math.round(r.height)],
        tallest: Math.round(Math.max(...hits.map((h) => h.height))),
        smallestHit: Math.round(Math.min(...hits.map((h) => h.width))),
      };
    });
    console.log(`${theme} ${viewport.width}: fps=${fps} ${JSON.stringify(geom)}`);
    await page.screenshot({ path: `${out}/city-${theme}-${viewport.width}.png`, fullPage: true });
    await page.locator('.nbhd-city').screenshot({ path: `${out}/stage-${theme}-${viewport.width}.png` });
    check(`${theme} ${viewport.width}: draws at a usable rate`, fps >= 25, fps);
    await page.context().close();
  }

  // The heaviest state has a budget too: a rain shower over a night city runs
  // streaks, wet streets, reflections and umbrellas all at once.
  {
    const page = await dash.page(handler, { viewport: { width: 1440, height: 1000 } });
    await page.addInitScript(() => { localStorage.setItem('ezllm.theme', 'dark'); localStorage.setItem('ezllm.city.seed', '4242'); });
    await page.goto(`${dash.base}/neighbourhood`);
    await page.locator('[data-building]').first().waitFor();
    await page.locator('[data-wx="rain"]').click();
    await page.waitForTimeout(1200);
    const fps = await page.evaluate(() => new Promise((resolve) => {
      let n = 0; const t0 = performance.now();
      const tick = () => { n++; if (performance.now() - t0 < 2000) requestAnimationFrame(tick); else resolve(Math.round(n / ((performance.now() - t0) / 1000))); };
      requestAnimationFrame(tick);
    }));
    console.log(`dark rain 1440: fps=${fps}`);
    await page.locator('.nbhd-city').screenshot({ path: `${out}/stage-rain-1440.png` });
    check('dark 1440 rain: draws at a usable rate', fps >= 25, fps);
    await page.context().close();
  }
} finally { await dash.close() }
finish();
