// Exercises the shipped Settings UI without provider credentials.
import { start, json, check, finish } from './mockdash.mjs';
const defaults = { usage_timezone: 'UTC', translation_enabled: true, automatic_probing: true, catalog_interval: '12h', price_refresh: true, retries: 3, attempt_budget: 8, dispatch_deadline: '', cooldown_429: '60s', cooldown_5xx: '60s', cooldown_strikes: 3, backup_enabled: false, backup_hour: 3, backup_keep: 7, notify_free_models_added: true, notify_free_models_removed: true, notify_free_model_broken: true, notify_catalog_refresh_failed: true, notify_client_budget_reached: true, notify_price_refresh_failed: true, notify_backup_failed: true, notify_bridge_updated: true, notify_bridge_update_blocked: true, notify_bridge_rolled_back: true, notify_quota_warning: true, alert_quota_percent: 80, notify_balance_low: true, alert_balance_below: 2, notify_cookie_expiring: true, alert_cookie_hours: 48, ledger_retention_days: 0, gemini_query_key: false };
let revision = 0, fields = Object.fromEntries(Object.entries(defaults).map(([k, value]) => [k, { value, source: k === 'price_refresh' ? 'environment' : 'default', editable: k !== 'price_refresh', saved: false, apply: 'immediate' }]));
let sessions = [{ id: 'session-other', browser: 'Other browser', current: false, created_at: '2026-10-26T00:00:00Z', expires_at: '2026-10-27T00:00:00Z' }];
let backups = { ready: true, running: false, last_outcome: '', backups: [] };
let rejectNext = false;
const imports = [];
const dash = await start();
try {
  const page = await dash.page(async (route, url) => {
    const method = route.request().method();
    if (url.pathname === '/admin/settings') {
      if (method === 'PATCH') {
        if (rejectNext) { rejectNext = false; await json(route, { error: { message: 'settings changed; reload and try again' } }, 409); return true; }
        const body = route.request().postDataJSON();
        check('save uses revision', body.revision === revision);
        for (const [k, v] of Object.entries(body.values)) fields[k] = { ...fields[k], value: v === null ? defaults[k] : v, saved: v !== null, source: v === null ? 'default' : 'settings' };
        revision++;
      }
      await json(route, { revision, fields }); return true;
    }
    if (url.pathname === '/admin/backups') { if (method === 'POST') backups.running = true; await json(route, method === 'POST' ? { status: 'running' } : backups, method === 'POST' ? 202 : 200); return true; }
    if (url.pathname === '/admin/import') {
      const dry = url.searchParams.get('dry_run') === 'true';
      imports.push({ dry, kind: JSON.parse(route.request().postData() || '{}').kind });
      await json(route, { dry_run: dry, summary: { create: 1, update: 1, unchanged: 1 }, items: [
        { section: 'accounts', name: 'acme', action: 'create', note: 'created turned off: add a key, then turn it on' },
        { section: 'combos', name: 'daily', action: 'update', changes: ['hops'] },
        { section: 'settings', name: 'retries', action: 'unchanged' },
      ] });
      return true;
    }
    if (url.pathname === '/admin/settings/sessions') { await json(route, sessions); return true; }
    if (url.pathname === '/admin/settings/sessions/session-other') { sessions = []; await route.fulfill({ status: 204 }); return true; }
    if (url.pathname === '/admin/settings/diagnostics') { await json(route, { version: 'test', uptime_seconds: 120, database_bytes: 1024, bridges: [{ id: 'chatgpt-sub', enabled: false }], jobs: { catalog: { running: false, last_success: '2026-10-26T00:00:00Z' } } }); return true; }
    return false;
  });
  const errors = []; page.on('pageerror', e => errors.push(e.message));
  await page.goto(dash.base + '/settings');
  await page.getByLabel('Theme', { exact: true }).selectOption('light');
  check('theme applies', await page.locator('html').evaluate(e => e.classList.contains('light')));
  await page.getByLabel('Motion', { exact: true }).selectOption('reduce');
  check('reduced motion applies', await page.locator('html').getAttribute('data-reduce-motion') === 'true');
  await page.getByLabel('Default analytics range', { exact: true }).selectOption('7d');
  await page.reload();
  check('browser preferences persist', await page.getByLabel('Default analytics range', { exact: true }).inputValue() === '7d');
  await page.getByLabel('Usage timezone', { exact: true }).fill('Asia/Kuala_Lumpur');
  check('timezone impact acknowledgement required', await page.getByRole('button', { name: 'Save changes', exact: true }).isDisabled());
  await page.getByLabel('I understand this changes where report days', { exact: false }).check();
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  await page.getByRole('status').filter({ hasText: 'Settings saved.' }).waitFor();
  await page.getByRole('button', { name: 'Gateway', exact: true }).click();
  check('environment override locked', await page.getByLabel('Automatic price refresh', { exact: true }).isDisabled());
  await page.getByLabel('Accept keys in the URL (Gemini ?key=)', { exact: true }).check();
  check('turning on URL keys shows the warning', await page.getByText('Warning: keys in the URL leak.').count() === 1);
  check('saving waits for the risk to be accepted', await page.getByRole('button', { name: 'Save changes', exact: true }).isDisabled());
  if (process.env.SHOTS_DIR) await page.getByRole('alert').filter({ hasText: 'keys in the URL leak' }).screenshot({ path: `${process.env.SHOTS_DIR}/gemini-key-warning.png` });
  await page.getByLabel('I understand that keys sent in the URL can be read from logs', { exact: false }).check();
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  await page.getByRole('status').filter({ hasText: 'Settings saved.' }).waitFor();
  check('URL keys save once accepted', fields.gemini_query_key.value === true);
  await page.getByLabel('Retries per hop', { exact: true }).fill('0');
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  await page.getByRole('status').filter({ hasText: 'Settings saved.' }).waitFor();
  check('zero retries saved', fields.retries.value === 0);
  await page.getByRole('button', { name: 'Reset to deployment default', exact: true }).first().click();
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  await page.getByRole('status').filter({ hasText: 'Settings saved.' }).waitFor();
  check('reset restores fallback', fields.retries.value === 3 && !fields.retries.saved);
  rejectNext = true;
  await page.getByLabel('Retries per hop', { exact: true }).fill('2');
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  await page.getByRole('alert').filter({ hasText: 'settings changed' }).waitFor();
  check('conflict keeps draft', await page.getByLabel('Retries per hop', { exact: true }).inputValue() === '2');
  await page.getByRole('button', { name: 'Security', exact: true }).click();
  await page.getByRole('dialog').getByText('Discard unsaved changes?').waitFor();
  check('leaving a tab with unsaved edits asks first', true);
  await page.getByRole('dialog').getByRole('button', { name: 'Discard changes', exact: true }).click();
  await page.getByText('Other browser', { exact: true }).waitFor();
  await page.getByRole('button', { name: 'Sign out', exact: true }).last().click();
  await page.getByRole('dialog').getByRole('button', { name: 'Sign out', exact: true }).click();
  await page.waitForFunction(() => !document.body.textContent.includes('Other browser'));
  check('individual session revoked', sessions.length === 0);
  await page.getByRole('button', { name: 'Backup & Recovery', exact: true }).click();
  check('configuration export exposed', await page.getByRole('link', { name: 'Export configuration', exact: true }).getAttribute('href') === '/admin/export');
  await page.getByRole('button', { name: 'Back up now', exact: true }).click();
  await page.getByText('Backup running…', { exact: true }).waitFor();
  check('backup overlap disabled', await page.getByRole('button', { name: 'Backing up…', exact: true }).isDisabled());
  await page.getByLabel('Keep every call for', { exact: true }).fill('7');
  check('retention under 32 days is refused', await page.getByText('Use 0 to keep every call, or a whole number of days from 32 to 3650.').count() === 1);
  await page.getByLabel('Keep every call for', { exact: true }).fill('90');
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  await page.getByRole('status').filter({ hasText: 'Settings saved.' }).waitFor();
  check('retention saves', fields.ledger_retention_days.value === 90);
  await page.getByLabel('Export file to import').setInputFiles({ name: 'ezllm-export.json', mimeType: 'application/json', buffer: Buffer.from(JSON.stringify({ kind: 'configuration_export', format_version: 1 })) });
  await page.getByText('1 to create, 1 to update, 1 unchanged').waitFor();
  check('import shows the plan first, without unchanged rows', imports.length === 1 && imports[0].dry && await page.getByText('created turned off: add a key, then turn it on').count() === 1 && await page.getByText('retries', { exact: true }).count() === 0, imports);
  if (process.env.SHOTS_DIR) { await page.waitForTimeout(800); await page.getByText('Import configuration', { exact: true }).locator('xpath=ancestor::section').screenshot({ path: `${process.env.SHOTS_DIR}/import-plan.png` }); }
  await page.getByLabel('Show unchanged').check();
  check('unchanged rows on request', await page.getByText('retries', { exact: true }).count() === 1);
  await page.getByRole('button', { name: 'Apply import', exact: true }).click();
  await page.getByText(/Imported ezllm-export.json/).waitFor();
  check('apply sends the same file without dry run', imports.length === 2 && !imports[1].dry && imports[1].kind === 'configuration_export', imports);
  check('the result says new providers are off', await page.getByText(/New providers are turned off/).count() === 1);
  await page.getByRole('navigation', { name: 'Settings sections' }).getByRole('button', { name: 'Notifications', exact: true }).click();
  check('backup notification preference exposed', await page.getByLabel('Backup failures', { exact: true }).count() === 1);
  check('early warnings have their switches', await page.getByLabel('Quota nearly used', { exact: true }).count() === 1 && await page.getByLabel('Balance running low', { exact: true }).count() === 1 && await page.getByLabel('Session cookie expiring', { exact: true }).count() === 1);
  await page.getByLabel('Warn at this much of a quota', { exact: true }).fill('150');
  check('a quota threshold over 100 is refused', await page.getByRole('button', { name: 'Save changes', exact: true }).isDisabled() && await page.getByText('The percent must be a whole number from 1 to 100.').count() === 1);
  await page.getByLabel('Warn at this much of a quota', { exact: true }).fill('90');
  await page.getByLabel('Warn below this balance', { exact: true }).fill('5.5');
  await page.getByRole('button', { name: 'Save changes', exact: true }).click();
  await page.getByRole('status').filter({ hasText: 'Settings saved.' }).waitFor();
  check('thresholds save as numbers', fields.alert_quota_percent.value === 90 && fields.alert_balance_below.value === 5.5, { q: fields.alert_quota_percent.value, b: fields.alert_balance_below.value });
  await page.getByRole('button', { name: 'System', exact: true }).click();
  await page.getByRole('link', { name: 'Download sanitized diagnostics', exact: true }).waitFor();
  check('effective configuration visible', await page.getByText('Effective configuration', { exact: true }).count() === 1);
  await page.setViewportSize({ width: 390, height: 844 });
  check('mobile layout stays within viewport', await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
  await page.screenshot({ path: '/tmp/ezllm-settings-mobile.png', fullPage: true });
  check('no browser exceptions', errors.length === 0, errors);
  await page.goto(dash.base + '/stats');
  check('analytics opens with preferred range', await page.getByRole('button', { name: '7d', exact: true }).first().getAttribute('aria-pressed') === 'true');
} finally { await dash.close(); }
finish();
