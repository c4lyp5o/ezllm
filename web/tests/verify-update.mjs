// The release check in the dashboard: the sidebar version pill and Settings → System.
import { start, json, check, finish } from './mockdash.mjs';

const REL = 'https://github.com/c4lyp5o/ezllm/releases/tag/v9.9.9';
const states = {
  available: { enabled: true, current: '2.2.0', latest: '9.9.9', available: true, url: REL, checked_at: '2026-11-24T08:00:00Z' },
  current: { enabled: true, current: '2.2.0', latest: '2.2.0', available: false, url: REL.replace('9.9.9', '2.2.0'), checked_at: '2026-11-24T08:00:00Z' },
  off: { enabled: false, current: '2.2.0', latest: '', available: false, url: '', checked_at: '' },
};
const want = { available: 'v9.9.9 available', current: 'Up to date', off: 'Releases' };
const diagnostics = { version: '2.2.0', uptime_seconds: 60, database_bytes: 1048576, bridges: [], jobs: {} };
const t = await start();
try {
  for (const [name, body] of Object.entries(states)) {
    const page = await t.page((route, url) => {
      if (url.pathname === '/admin/update') return json(route, body), true;
      if (url.pathname === '/healthz') return json(route, { status: 'ok', version: '2.2.0' }), true;
      if (url.pathname === '/admin/settings/diagnostics') return json(route, diagnostics), true;
      if (url.pathname === '/admin/settings') return json(route, { revision: 0, fields: {} }), true;
      return false;
    });
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto(`${t.base}/settings?tab=system`);
    const side = page.locator('aside a', { hasText: 'v2.2.0' });
    await side.waitFor();
    const pill = page.getByRole('link', { name: want[name], exact: true });
    await pill.waitFor();
    const lit = (await side.getAttribute('class')).includes('bg-warn-dim');
    check(`${name}: sidebar version is ${name === 'available' ? 'highlighted' : 'plain'}`, lit === (name === 'available'));
    check(`${name}: sidebar link`, (await side.getAttribute('href')) === (name === 'available' ? REL : 'https://github.com/c4lyp5o/ezllm'));
    check(`${name}: new version is named for screen readers`, (await side.getAttribute('aria-label')).includes('v9.9.9 available') === (name === 'available'));
    check(`${name}: System says "${want[name]}"`, await pill.count() === 1);
    await page.close();
  }
} finally {
  await finish(t);
}
