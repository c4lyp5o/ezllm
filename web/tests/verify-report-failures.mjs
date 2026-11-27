// Failed usage reports must be visibly unknown and recover on retry.
import { start, json, check, finish } from './mockdash.mjs';
const t = await start();
let fail = true;
const page = await t.page(async (route, url) => {
  if (url.pathname === '/admin/overview') {
    await json(route, { health: { status: 'ok', uptime_s: 10, dropped_rows: 0 }, usage: [], usage_by_surface: [], accounts: [], recent_calls: [] });
    return true;
  }
  if (url.pathname === '/admin/usage' && fail) {
    await json(route, { error: { message: 'report unavailable' } }, 503);
    return true;
  }
  return false;
});
await page.goto(t.base);
await page.getByText(/Some usage reports didn.t refresh/).waitFor();
check('dashboard shows unavailable reports', await page.getByText(/Some usage reports didn.t refresh/).count() === 1);
fail = false;
await page.getByRole('button', { name: 'Retry' }).click();
await page.getByText(/Some usage reports didn.t refresh/).waitFor({ state: 'detached' });
check('dashboard clears warning on recovery', await page.getByText(/Some usage reports didn.t refresh/).count() === 0);
fail = true;
await page.goto(`${t.base}/connect`);
await page.getByText(/Some usage numbers didn.t load/).waitFor();
check('connect shows unavailable usage', await page.getByText(/Some usage numbers didn.t load/).count() === 1);
fail = false;
await page.getByRole('button', { name: 'Retry' }).click();
await page.getByText(/Some usage numbers didn.t load/).waitFor({ state: 'detached' });
check('connect clears warning on recovery', await page.getByText(/Some usage numbers didn.t load/).count() === 0);
await t.close();
finish();
