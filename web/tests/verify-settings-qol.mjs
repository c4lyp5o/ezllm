// Settings QoL: inline validation that mirrors the server's bounds, discard
// and unsaved-change guards, the tab in the URL, session sign-out in a styled
// confirm, and readable system figures. Runs against the built web/dist.
import { start, json, check, finish } from './mockdash.mjs';

const defaults = { usage_timezone: 'UTC', translation_enabled: true, automatic_probing: true, catalog_interval: '12h', price_refresh: true, retries: 3, attempt_budget: 8, dispatch_deadline: '', cooldown_429: '60s', cooldown_5xx: '30s', cooldown_strikes: 3, backup_enabled: false, backup_hour: 3, backup_keep: 7, notify_backup_failed: true };
const fields = Object.fromEntries(Object.entries(defaults).map(([k, value]) => [k, { value, source: 'default', editable: true, saved: false, apply: 'immediate' }]));
let sessions = [
  { id: 'cur', browser: 'This one', current: true, created_at: new Date(Date.now() - 3600e3).toISOString(), expires_at: new Date(Date.now() + 86400e3).toISOString() },
  { id: 'other', browser: 'Other browser', current: false, created_at: new Date(Date.now() - 2 * 86400e3).toISOString(), expires_at: new Date(Date.now() + 86400e3).toISOString() },
];
const calls = [];
const t = await start();
const page = await t.page(async (route, url) => {
  const m = route.request().method();
  if (m !== 'GET') calls.push(`${m} ${url.pathname}`);
  if (url.pathname === '/admin/settings') { await json(route, { revision: 0, fields }); return true; }
  if (url.pathname === '/admin/settings/sessions') { await json(route, sessions); return true; }
  if (url.pathname === '/admin/settings/sessions/other') { sessions = sessions.filter((s) => s.id !== 'other'); await route.fulfill({ status: 204 }); return true; }
  if (url.pathname === '/admin/settings/diagnostics') { await json(route, { version: 'v1.5.0', uptime_seconds: 93784, database_bytes: 1048576, bridges: [], jobs: {} }); return true; }
  return false;
});
const save = () => page.getByRole('button', { name: 'Save changes', exact: true });

try {
  await page.goto(`${t.base}/settings?tab=gateway`);
  await page.getByLabel('Retries per hop', { exact: true }).waitFor();
  check('?tab= opens that section', (await page.getByRole('button', { name: 'Gateway', exact: true }).getAttribute('aria-current')) === 'page');
  check('Save is disabled until something changes', await save().isDisabled());
  await page.getByLabel('Retries per hop', { exact: true }).fill('12');
  check('an out-of-range number is explained', (await page.getByText('Retries must be a whole number from 0 to 9.').count()) === 1);
  check('and cannot be saved', await save().isDisabled());
  await page.getByLabel('Retries per hop', { exact: true }).fill('4');
  check('a valid number clears the message', (await page.getByText('Retries must be a whole number').count()) === 0 && await save().isEnabled());
  await page.getByLabel('Catalog refresh interval', { exact: true }).fill('30s');
  check('a too-short duration is explained', (await page.getByText('Use a duration of at least 1m, for example 12h.').count()) === 1 && await save().isDisabled());
  await page.getByLabel('Catalog refresh interval', { exact: true }).fill('90m');
  check('a Go duration such as 90m is accepted', (await page.getByText('Use a duration of at least 1m').count()) === 0);
  await page.getByLabel('Dispatch deadline', { exact: true }).fill('abc');
  check('a malformed deadline is explained', (await page.getByText(/Use a duration of at least 1s/).count()) === 1);
  await page.getByLabel('Dispatch deadline', { exact: true }).fill('');
  check('an empty deadline (no limit) is fine', (await page.getByText(/Use a duration of at least 1s/).count()) === 0);
  check('unsaved changes are flagged', (await page.getByText('Unsaved changes').count()) === 1);
  await page.getByRole('button', { name: 'Discard changes', exact: true }).click();
  check('Discard changes puts the saved values back', (await page.getByLabel('Retries per hop', { exact: true }).inputValue()) === '3' && await save().isDisabled());

  // tab guard
  await page.getByLabel('Retries per hop', { exact: true }).fill('5');
  await page.getByRole('navigation', { name: 'Settings sections' }).getByRole('button', { name: 'Notifications', exact: true }).click();
  await page.getByRole('dialog').getByText('Discard unsaved changes?').waitFor();
  await page.keyboard.press('Escape');
  check('declining the guard stays on the tab with the edit', (await page.getByLabel('Retries per hop', { exact: true }).inputValue()) === '5');
  await page.getByRole('navigation', { name: 'Settings sections' }).getByRole('button', { name: 'Notifications', exact: true }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Discard changes', exact: true }).click();
  await page.getByLabel('Backup failures', { exact: true }).waitFor();
  check('accepting it moves on and the URL follows', page.url().includes('tab=notifications'), page.url());

  // timezone
  await page.getByRole('button', { name: 'General', exact: true }).click();
  await page.getByLabel('Usage timezone', { exact: true }).fill('Mars/Olympus');
  check('an unknown timezone is explained', (await page.getByText('Use an IANA timezone name').count()) === 1);
  await page.getByLabel('Usage timezone', { exact: true }).fill('Asia/Kuala_Lumpur');
  check('a real timezone is accepted', (await page.getByText('Use an IANA timezone name').count()) === 0);

  // sessions
  await page.getByRole('button', { name: 'Discard changes', exact: true }).click();
  await page.getByRole('button', { name: 'Security', exact: true }).click();
  await page.getByText('Other browser', { exact: true }).waitFor();
  check('the current session is marked', (await page.getByText('This browser', { exact: true }).count()) === 1);
  check('session age reads relative', (await page.getByText(/Created 2d ago/).count()) === 1);
  calls.length = 0;
  await page.getByRole('button', { name: 'Sign out', exact: true }).last().click();
  await page.getByRole('dialog').getByText('Sign out this session?').waitFor();
  check('sign-out asks in a styled dialog and sends nothing yet', calls.length === 0, calls);
  await page.getByRole('dialog').getByRole('button', { name: 'Sign out', exact: true }).click();
  await page.waitForFunction(() => !document.body.textContent.includes('Other browser'));
  check('confirming signs that session out', calls.includes('DELETE /admin/settings/sessions/other'), calls);

  // system
  await page.getByRole('button', { name: 'System', exact: true }).click();
  await page.getByText('v1.5.0').waitFor();
  check('uptime reads in days and hours', (await page.getByText('1d 2h').count()) === 1);
  check('the version can be copied', (await page.getByRole('button', { name: 'Copy', exact: true }).count()) === 1);
} catch (e) {
  check('driver ran to completion', false, String(e));
} finally {
  await t.close();
}
finish();
