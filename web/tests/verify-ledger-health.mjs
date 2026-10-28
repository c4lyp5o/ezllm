// Verify all accounting-loss counters are visible on the built dashboard.
import { start, json, check, finish } from './mockdash.mjs';

const t = await start();
const page = await t.page(async (route, url) => {
  if (url.pathname === '/admin/overview') {
    await json(route, {
      health: { status: 'ok', uptime_s: 10, dropped_rows: 2, lost_rows: 3, unmetered_rows: 4 },
      usage: [], usage_by_surface: [], accounts: [], recent_calls: [],
    });
    return true;
  }
  return false;
});
await page.goto(t.base);
await page.getByText('3 ledger rows lost').waitFor();
check('dropped rows visible', await page.getByText('2 ledger rows dropped').count() === 1);
check('lost rows visible', await page.getByText('3 ledger rows lost').count() === 1);
check('unmetered rows visible', await page.getByText('4 rows unmetered').count() === 1);
await t.close();
finish();
