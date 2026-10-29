// Providers QoL: key add/replace from the Edit modal (replace removes the old
// key only after the new one validates), modal guards (busy, unsaved changes,
// Enter submits), secret fields, pricing, and the page's search, filters,
// switches, test-all and deep links. Runs against the built web/dist with
// fixtures (see mockdash.mjs).
import { start, json, check, finish } from './mockdash.mjs';

const t = await start();
const key = (id, label, o = {}) => ({ id, label, hint: `sk-…${1000 + id}`, enabled: true, last_test_ok: true, ...o });
const acct = (id, name, ns, o = {}) => ({
  id, name, namespace: ns, kind: 'openai-compatible', base_url: `https://${ns}.example.test/v1`, enabled: true,
  probe_delay_ms: 400, notes: null, keys: [key(id * 10, 'primary')], quota: null, models_count: 3,
  protocol_support: { chat: 2, anthropic: 0, responses: 0, untested: 0 }, ...o,
});
const accounts = [
  acct(1, 'Alpha', 'alpha'),
  acct(2, 'Beta', 'beta', { keys: [key(21, 'primary'), key(22, 'backup', { last_test_ok: false, last_test_detail: 'chat: HTTP 401 invalid key' })] }),
  acct(3, 'Gamma', 'gamma', { keys: [], enabled: false }),
];
const models = [{ id: 'm-one', display: 'm-one', chat: true, anthropic: false, responses: false, pin: '' }];
const prices = [{ account_id: 1, model_id: 'm-one', effective_from: '2026-09-01', currency: 'USD', price_in: 1, price_out: 2.5, price_cache_read: 0.075, price_cache_write: 1 }];

const calls = [];            // every mutating request, in order: "METHOD path body"
let keyPost = { status: 200, delay: 0 };
const page = await t.page(async (route, url) => {
  const req = route.request();
  const p = url.pathname;
  const m = req.method();
  if (m !== 'GET') calls.push(`${m} ${p} ${req.postData() ?? ''}`);
  if (p === '/admin/accounts' && m === 'GET') { await json(route, accounts); return true; }
  if (/\/admin\/accounts\/\d+\/models$/.test(p)) { await json(route, models); return true; }
  if (p === '/admin/prices') { await json(route, prices); return true; }
  if (p === '/admin/model-rules') { await json(route, []); return true; }
  if (p === '/admin/bridges') { await json(route, []); return true; }
  if (/\/admin\/accounts\/\d+\/keys$/.test(p) && m === 'POST') {
    if (keyPost.delay) await new Promise((r) => setTimeout(r, keyPost.delay));
    if (keyPost.status !== 200) { await json(route, { error: { message: 'the key was rejected', type: 'invalid_request_error' } }, keyPost.status); return true; }
    await json(route, { key: key(99, 'new'), test: { ok: true, steps: [] } });
    return true;
  }
  if (m !== 'GET' && p.startsWith('/admin/')) { await json(route, {}); return true; }
  return false;
});
const dialog = () => page.getByRole('dialog');
const last = (re) => calls.filter((c) => re.test(c));

try {
  // ── Edit modal: tabs, guards, Enter ──
  await page.goto(`${t.base}/providers?edit=2`);
  await dialog().waitFor();
  const save = dialog().getByRole('button', { name: 'Save', exact: true });
  check('edit: Save is disabled until something changes', await save.isDisabled());
  check('edit: first field is focused, not the close button', (await page.evaluate(() => document.activeElement?.id)) === 'ea-name');
  check('edit: namespace is read-only', await dialog().getByLabel('Namespace', { exact: true }).isDisabled());
  await dialog().getByLabel('Name', { exact: true }).fill('Beta renamed');
  check('edit: Save enables once dirty', await save.isEnabled());
  await page.keyboard.press('Escape');
  await dialog().getByText('Discard unsaved changes?').waitFor();
  check('edit: Escape on unsaved edits asks first', true);
  await page.keyboard.press('Escape');
  check('edit: Escape again keeps editing', (await dialog().getByText('Discard unsaved changes?').count()) === 0 && (await dialog().count()) === 1);
  await page.locator('#ea-name').press('Enter');
  await page.waitForTimeout(300);
  check('edit: Enter saves the settings', last(/^PATCH \/admin\/accounts\/2 .*Beta renamed/).length === 1, calls);
  await dialog().waitFor({ state: 'detached', timeout: 3000 }).catch(() => {});

  await page.goto(`${t.base}/providers?edit=2`);
  await dialog().waitFor();
  await dialog().getByLabel('Name', { exact: true }).fill('Changed');
  await page.keyboard.press('Escape');
  await dialog().getByRole('button', { name: 'Discard' }).click();
  await dialog().waitFor({ state: 'detached', timeout: 3000 });
  check('edit: Discard closes without saving', last(/Changed/).length === 0);

  // ── Keys tab ──
  await page.goto(`${t.base}/providers?keys=2`);
  await dialog().waitFor();
  check('keys: deep link opens the Keys tab', (await dialog().getByRole('button', { name: /^Keys/ }).getAttribute('aria-pressed')) === 'true');
  check('keys: a failed key says so', (await dialog().getByText('last test failed').count()) === 1);
  calls.length = 0;
  await dialog().getByRole('button', { name: 'Replace key backup' }).click();
  const secret = dialog().getByLabel('API key');
  check('keys: replace form is masked', (await secret.getAttribute('type')) === 'password');
  await dialog().getByRole('button', { name: 'Show value' }).click();
  check('keys: Show reveals the value field', (await secret.getAttribute('type')) === 'text');
  await secret.evaluate((el) => {
    const dt = new DataTransfer(); dt.setData('text', '  sk-new-key-123\n');
    el.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }));
  });
  check('keys: pasted whitespace is trimmed', (await secret.inputValue()) === 'sk-new-key-123', await secret.inputValue());
  check('keys: character count shown', (await dialog().getByText('14 characters').count()) === 1);

  // busy guard: Escape must not close while validating
  keyPost = { status: 200, delay: 900 };
  const submitKey = dialog().getByRole('button', { name: 'Validate, save & remove old key' });
  await submitKey.click();
  await dialog().getByRole('button', { name: 'Validating key…' }).waitFor();
  await page.keyboard.press('Escape');
  check('busy: Escape does nothing while a key validates', (await dialog().count()) === 1 && (await dialog().getByText('Discard unsaved changes?').count()) === 0);
  await dialog().getByText(/Key replaced/).waitFor({ timeout: 5000 });
  const posts = calls.findIndex((c) => /^POST \/admin\/accounts\/2\/keys /.test(c));
  const dels = calls.findIndex((c) => /^DELETE \/admin\/accounts\/2\/keys\/22/.test(c));
  check('replace: new key is added, then the old key is deleted', posts >= 0 && dels > posts, calls);
  check('replace: body carries label and key', /"label":"backup"/.test(calls[posts]) && /"api_key":"sk-new-key-123"/.test(calls[posts]), calls[posts]);

  // failed validation must not delete anything
  keyPost = { status: 422, delay: 0 };
  calls.length = 0;
  await dialog().getByRole('button', { name: 'Replace key primary' }).click();
  await dialog().getByLabel('API key').fill('bad-key');
  await dialog().getByRole('button', { name: 'Validate, save & remove old key' }).click();
  await dialog().getByText('the key was rejected').waitFor();
  check('replace: a rejected key shows the reason', true);
  check('replace: a rejected key deletes nothing', last(/^DELETE/).length === 0, calls);

  // add a key
  keyPost = { status: 200, delay: 0 };
  calls.length = 0;
  await dialog().getByRole('button', { name: 'Cancel', exact: true }).click();
  await dialog().getByRole('button', { name: 'Add key' }).click();
  await dialog().getByLabel('API key').fill('sk-added');
  await dialog().getByLabel('API key').press('Enter');
  await dialog().getByText(/Key added/).waitFor({ timeout: 5000 });
  check('add: Enter in the key field validates it, without saving settings', last(/^POST .*keys /).length === 1 && last(/^PATCH/).length === 0, calls);
  await page.keyboard.press('Escape');

  // ── Pricing ──
  await page.goto(`${t.base}/providers?pricing=1`);
  await dialog().waitFor();
  await dialog().getByText('0.075').waitFor();
  check('pricing: rates print with aligned decimals', (await dialog().getByText('0.075').count()) === 1 && (await dialog().getByText('1.00').count()) >= 1);
  calls.length = 0;
  await dialog().getByRole('button', { name: /^Edit rate for m-one/ }).click();
  check('pricing: Edit loads the row into the form', (await dialog().getByLabel(/^Input/).inputValue()) === '1');
  await dialog().getByLabel(/^Output/).fill('3');
  await dialog().getByLabel(/^Output/).press('Enter');
  await page.waitForTimeout(300);
  check('pricing: Enter saves the rate', last(/^PUT \/admin\/accounts\/1\/prices .*"price_out":3/).length === 1, calls);
  await dialog().getByRole('button', { name: /^Delete rate for m-one/ }).click();
  await page.getByRole('button', { name: 'Delete', exact: true }).last().click();
  await page.waitForTimeout(300);
  check('pricing: delete asks first, then deletes', last(/^DELETE \/admin\/accounts\/1\/prices/).length === 1, calls);
  await page.keyboard.press('Escape');

  // ── Page ──
  await page.goto(`${t.base}/providers`);
  await page.getByText('Alpha').first().waitFor();
  check('page: summary counts providers needing attention', (await page.getByText(/1 needs attention/).count()) === 1);
  await page.getByLabel('Search providers').fill('gam');
  check('page: search filters the list', (await page.getByText('Alpha').count()) === 0 && (await page.getByText('Gamma').count()) > 0);
  await page.getByLabel('Search providers').fill('');
  await page.getByRole('button', { name: /^Needs attention/ }).click();
  check('page: needs-attention filter shows only unhealthy providers', (await page.getByText('Alpha').count()) === 0 && (await page.getByText('Beta').count()) > 0);
  await page.getByRole('button', { name: /^Needs attention/ }).click();
  await page.getByRole('button', { name: 'Expand all' }).click();
  await page.getByRole('button', { name: 'Collapse all' }).waitFor();
  check('page: expand all', true);
  check('page: a failed key shows its reason inline', (await page.getByText('chat: HTTP 401 invalid key').count()) >= 1);

  calls.length = 0;
  await page.getByRole('switch', { name: 'Alpha enabled' }).click();
  await page.waitForTimeout(300);
  check('page: provider switch PATCHes enabled', last(/^PATCH \/admin\/accounts\/1 \{"enabled":false\}/).length === 1, calls);
  calls.length = 0;
  await page.getByRole('switch', { name: 'Key backup enabled' }).click();
  await page.waitForTimeout(300);
  check('page: key switch PATCHes enabled', last(/^PATCH \/admin\/accounts\/2\/keys\/22 \{"enabled":false\}/).length === 1, calls);
  calls.length = 0;
  await page.getByRole('button', { name: 'Test all keys' }).click();
  await page.waitForTimeout(600);
  check('page: Test all keys retests every key', last(/retest/).length === 2 && /keys\/21\/retest/.test(last(/retest/)[0]) && /keys\/22\/retest/.test(last(/retest/)[1]), calls);

  // ── Add modal guards ──
  await page.getByRole('button', { name: /Add provider/ }).first().click();
  await dialog().getByLabel('Name', { exact: true }).fill('Half done');
  await page.keyboard.press('Escape');
  await dialog().getByText('Discard unsaved changes?').waitFor();
  check('add: unsaved wizard asks before closing', true);
  await dialog().getByRole('button', { name: 'Keep editing' }).click();
  calls.length = 0;
  await dialog().getByLabel('Name', { exact: true }).press('Enter');
  await page.waitForTimeout(300);
  check('add: Enter does not submit an incomplete wizard', calls.length === 0, calls);
  check('modal: page behind does not scroll', (await page.evaluate(() => document.body.style.overflow)) === 'hidden');
} catch (e) {
  check('driver ran to completion', false, String(e));
} finally {
  await t.close();
}
finish();
