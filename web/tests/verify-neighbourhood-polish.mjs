// Verify the data-backed city, its nonvisual controls, and degraded states.
import { mkdir, writeFile } from 'node:fs/promises';
import { start, json, check, finish, account } from './mockdash.mjs';
const artifacts = '/tmp/ezllm-scene-evidence'; await mkdir(artifacts, { recursive: true });
const key = (id, label, hint, enabled = true) => ({ id, label, hint, enabled, last_test_ok: true });
const accounts = [
  { ...account, id: 1, name: 'Lantern district', keys: [key(1, 'Night shift', 'a'), key(2, 'Weekend worker', 'b'), key(3, 'Retired', 'c', false)] },
  { ...account, id: 2, name: 'Harbour street', keys: [key(4, 'Busy corner', 'd'), key(5, 'Spare room', 'e')] },
];
const row = (k, calls, tin, errors = 0) => ({ k, calls, tin, tout: 0, errors, cread: 0, cwrite: 0, saved: 0, reasoning: 0 });
const usage = { groups: [row('a', 40, 120000), row('d', 8, 5000, 4)], totals: {} };
const dash = await start(); let pages = 0; const errors = [];
async function pageFor(mode = 'success', options = {}) {
  const page = await dash.page(async (route, url) => {
    if (url.pathname === '/admin/accounts') { await json(route, mode === 'empty' ? [] : accounts, mode === 'error' ? 503 : 200); return true }
    if (url.pathname === '/admin/usage') { await json(route, usage, mode === 'partial' || mode === 'error' ? 503 : 200); return true }
    return false;
  }, options); page.on('pageerror', (e) => errors.push(e.message)); pages++; return page;
}
const frames = (page) => page.locator('canvas').getAttribute('data-frames').then((v) => Number(v ?? 0));
const shot = (page) => page.locator('canvas').evaluate((c) => c.toDataURL());
try {
  for (const [theme, viewport] of [['dark', { width: 1440, height: 900 }], ['light', { width: 1440, height: 900 }], ['dark', { width: 390, height: 844 }], ['light', { width: 390, height: 844 }]]) {
    const page = await pageFor('success', { viewport, hasTouch: viewport.width < 640, isMobile: viewport.width < 640 });
    await page.addInitScript((t) => localStorage.setItem('ezllm.theme', t), theme);
    await page.goto(dash.base + '/neighbourhood'); await page.locator('[data-building="1"]').waitFor();
    await page.locator('[data-building="1"]').focus(); await page.keyboard.press('Enter');
    check(`${theme} ${viewport.width}: keyboard selects a building`, await page.locator('[data-building="1"]').getAttribute('aria-pressed') === 'true');
    check(`${theme} ${viewport.width}: inspector shows exact provider data`, (await page.getByLabel('Selected building').innerText()).includes('24,000'));
    if (viewport.width < 640) {
      // Tap the published roof point of a building the scene really exposes:
      // a nearer tower can cover the one behind it and rightly keeps the tap.
      await page.locator('canvas').scrollIntoViewIfNeeded();
      const tapId = await page.evaluate(() => {
        const c = document.querySelector('canvas').getBoundingClientRect();
        for (const el of document.querySelectorAll('[data-building]')) {
          const [x, y] = el.dataset.roof.split(',').map(Number);
          const hit = document.elementFromPoint(c.left + x, c.top + y);
          // Skip the already-selected building: a tap toggles, so tapping it
          // again would deselect — the check wants a building that selects.
          if (hit && hit.closest('[data-building]') === el && el.getAttribute('aria-pressed') !== 'true') {
            return el.dataset.building;
          }
        }
        return null;
      });
      const [rx, ry] = await page.locator(`[data-building="${tapId}"]`).evaluate((el) => el.dataset.roof.split(',').map(Number));
      const canvas = page.locator('canvas');
      await canvas.scrollIntoViewIfNeeded();
      const box = await canvas.boundingBox();
      await page.touchscreen.tap(box.x + rx, box.y + ry);
      check(`${theme} ${viewport.width}: touch selects a building`, tapId !== null
        && await page.locator(`[data-building="${tapId}"]`).getAttribute('aria-pressed') === 'true');
    }
    // The city is contained by its stage; it must never widen the page.
    check(`${theme} ${viewport.width}: no page overflow`, await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    const first = await frames(page); await page.waitForTimeout(1100);
    check(`${theme} ${viewport.width}: traffic animates`, await frames(page) > first, { first, now: await frames(page) });
    await page.getByRole('button', { name: 'Pause traffic', exact: true }).click();
    await page.mouse.move(0, 0); await page.waitForTimeout(300);
    const held = await shot(page); const heldFrames = await frames(page);
    await page.waitForTimeout(500);
    check(`${theme} ${viewport.width}: pause stops drawing`, (await shot(page)) === held && (await frames(page)) === heldFrames);
    await page.evaluate(() => scrollTo(0, 0));
    await page.screenshot({ path: `${artifacts}/neighbourhood-${theme}-${viewport.width}.png`, fullPage: true });
    await page.getByRole('button', { name: 'Resume traffic', exact: true }).click();
    await page.emulateMedia({ reducedMotion: 'reduce' });
    await page.getByRole('button', { name: 'Reduced motion', exact: true }).waitFor();
    const rFirst = await shot(page); await page.waitForTimeout(500);
    check(`${theme} ${viewport.width}: reduced motion keeps the city and stops the loop`,
      (await page.locator('[data-building]').count()) === 2 && (await shot(page)) === rFirst);
    await page.context().close();
  }
  const empty = await pageFor('empty'); await empty.goto(dash.base + '/neighbourhood'); await empty.getByText('No providers yet.', { exact: true }).waitFor();
  check('empty: actionable provider link', await empty.getByRole('link', { name: 'Add a provider', exact: true }).getAttribute('href') === '/providers');
  await empty.screenshot({ path: `${artifacts}/neighbourhood-empty.png` }); await empty.context().close();
  const partial = await pageFor('partial'); await partial.goto(dash.base + '/neighbourhood'); await partial.locator('[data-state="unknown"]').first().waitFor();
  check('partial: city remains, usage is unavailable', await partial.locator('[data-building]').count() === 2 && await partial.locator('[data-state="unknown"]').count() === 2);
  check('partial: missing usage is never shown as zero', (await partial.locator('table').innerText()).includes('—'));
  await partial.screenshot({ path: `${artifacts}/neighbourhood-partial.png` }); await partial.context().close();
  const failure = await pageFor('error'); await failure.goto(dash.base + '/neighbourhood'); await failure.getByRole('alert').waitFor();
  check('error: retry action available', await failure.getByRole('button', { name: 'Try again', exact: true }).isEnabled());
  await failure.screenshot({ path: `${artifacts}/neighbourhood-error.png` });
  await failure.route('**/admin/accounts', (route) => json(route, accounts)); await failure.route('**/admin/usage?*', (route) => json(route, usage));
  await failure.getByRole('button', { name: 'Try again', exact: true }).click(); await failure.locator('[data-building]').first().waitFor();
  check('error: retry restores the city', await failure.locator('[data-building]').count() === 2); await failure.context().close();
  let release; const held = new Promise((r) => { release = r }); const loading = await dash.page(async (route, url) => {
    if (url.pathname === '/admin/accounts') { await held; await json(route, accounts); return true }
    if (url.pathname === '/admin/usage') { await json(route, usage); return true } return false;
  });
  // The stage's own loader can come and go within a second: an observer
  // records it the moment it appears.
  await loading.addInitScript(() => {
    window.__stageLoader = null;
    new MutationObserver(() => {
      const el = document.querySelector('.nbhd-city-loading');
      if (el && !window.__stageLoader) window.__stageLoader = { label: el.querySelector('[role="progressbar"]')?.getAttribute('aria-label'), pointer: getComputedStyle(el).pointerEvents, drawnYet: !!document.querySelector('canvas.nbhd-city-canvas')?.dataset.frames };
    }).observe(document, { childList: true, subtree: true });
  });
  await loading.goto(dash.base + '/neighbourhood'); await loading.getByText('Laying out the streets…', { exact: true }).waitFor();
  await loading.screenshot({ path: `${artifacts}/neighbourhood-loading.png` }); check('loading: a progress bar while the data loads', await loading.getByRole('progressbar', { name: 'Laying out the streets…' }).count() === 1);
  release(); await loading.locator('[data-building]').first().waitFor();
  await loading.waitForFunction(() => document.querySelector('canvas.nbhd-city-canvas')?.dataset.frames);
  const stage = await loading.evaluate(() => window.__stageLoader);
  check('loading: the stage shows a loader until the first frame', !!stage && stage.label === 'Drawing the city…' && stage.drawnYet === false, stage);
  check('loading: the stage loader never takes a click', stage?.pointer === 'none', stage);
  await loading.waitForFunction(() => !document.querySelector('.nbhd-city-loading'), null, { timeout: 5000 });
  check('loading: the stage loader is gone once the city is drawn', await loading.locator('.nbhd-city-loading').count() === 0);
  await loading.context().close();
  let reads = 0; const visibility = await dash.page(async (route, url) => {
    if (url.pathname === '/admin/accounts') { reads++; await json(route, accounts); return true }
    if (url.pathname === '/admin/usage') { await json(route, usage); return true } return false
  });
  await visibility.clock.install(); await visibility.goto(dash.base + '/neighbourhood'); await visibility.locator('[data-building]').first().waitFor(); const initial = reads;
  await visibility.evaluate(() => { Object.defineProperty(document, 'hidden', { configurable: true, get: () => true }); document.dispatchEvent(new Event('visibilitychange')) });
  await visibility.clock.fastForward(11000);
  check('hidden tab: no polling', reads === initial);
  check('hidden tab: scene paused', await visibility.locator('.nbhd-city').getAttribute('data-still') === 'true');
  const resumed = visibility.waitForResponse((r) => new URL(r.url()).pathname === '/admin/accounts');
  await visibility.evaluate(() => { Object.defineProperty(document, 'hidden', { configurable: true, get: () => false }); document.dispatchEvent(new Event('visibilitychange')) }); await resumed;
  await visibility.waitForFunction(() => document.querySelector('.nbhd-city')?.getAttribute('data-still') === 'false');
  check('visible tab: polling resumes', reads > initial); await visibility.context().close();
  const long = await pageFor('success', { viewport: { width: 390, height: 844 } });
  await long.route('**/admin/accounts', (r) => json(r, [{ ...accounts[0], name: 'A provider account with a very long descriptive name', keys: [{ ...accounts[0].keys[0], label: 'a'.repeat(120) }] }]));
  await long.goto(dash.base + '/neighbourhood'); await long.locator('[data-building]').click();
  check('long labels: inspector keeps its width', await long.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
  await long.screenshot({ path: `${artifacts}/neighbourhood-long-label.png`, fullPage: true }); await long.context().close();
  const denied = await dash.page(async (route, url) => {
    if (url.pathname === '/admin/accounts' || url.pathname === '/admin/usage') { await json(route, { error: { message: 'unauthorized', code: 401, type: 'invalid_request_error' } }, 401); return true } return false
  }, { unlocked: false });
  await denied.goto(dash.base + '/'); await denied.getByRole('heading', { name: 'Sign in to ezllm' }).waitFor();
  await denied.evaluate(() => sessionStorage.setItem('ezllm.admin.unlocked', '1'));
  await denied.goto(dash.base + '/neighbourhood'); await denied.getByRole('heading', { name: 'Sign in to ezllm' }).waitFor();
  check('permission: expired session returns to login', await denied.locator('[data-building]').count() === 0); await denied.context().close();
  check('no browser exceptions', errors.length === 0, errors);
  await writeFile(`${artifacts}/checks.json`, JSON.stringify({ pages: pages + 4, errors }, null, 2));
} finally { await dash.close() }
finish();
