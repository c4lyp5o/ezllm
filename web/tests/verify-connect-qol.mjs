// Connect QoL: key list with status, relative use, search/filter and view-
// requests links; create-key modal (validation, role, allowlist readout,
// presets, budget); the show-once panel with real-key snippets and a key
// test; policy modal guards; client recipes; the models browser; deep links;
// and the Requests page's API-key filter. Runs against the built web/dist.
import { start, json, check, finish } from './mockdash.mjs';

const t = await start();
const day = 86_400_000;
const iso = (ms) => new Date(Date.now() + ms).toISOString();
const tok = (id, name, o = {}) => ({ id, name, hint: `ez-…${id}${id}${id}${id}`, roles: ['infer'], enabled: true, cap_window: 'monthly', cap_tokens: 0, allowed_models: '', expires_at: '', last_used_at: iso(-2 * 3600e3), created_at: iso(-30 * day), ...o });
const tokens = [
  tok(1, 'hermes', { cap_tokens: 10_000_000, allowed_models: 'deepseek/*', expires_at: iso(200 * day) }),
  tok(2, 'old', { expires_at: iso(-2 * day) }),
  tok(3, 'soon', { expires_at: iso(3 * day) }),
  tok(4, 'spent', { cap_tokens: 1000 }),
  tok(5, 'off', { enabled: false, last_used_at: null }),
  tok(6, 'boss', { roles: ['admin'] }),
];
const routable = [{ id: 'deepseek/chat' }, { id: 'deepseek/coder' }, { id: 'openai/gpt-5' }, { id: 'daily' }];

const calls = [];
const v1Auth = [];
const reqQueries = [];
let failToggle = true;
const page = await t.page(async (route, url) => {
  const req = route.request();
  const p = url.pathname;
  const m = req.method();
  if (m !== 'GET' && p.startsWith('/admin/')) calls.push(`${m} ${p} ${req.postData() ?? ''}`);
  if (p === '/admin/tokens' && m === 'GET') { await json(route, tokens); return true; }
  if (p === '/admin/tokens' && m === 'POST') {
    const b = JSON.parse(req.postData());
    await json(route, { token: tok(9, b.name, { roles: b.roles, allowed_models: b.allowed_models, expires_at: b.expires_at }), plaintext: 'ez-secret-123' }, 201);
    return true;
  }
  if (/^\/admin\/tokens\/5$/.test(p) && m === 'PATCH' && failToggle) { await json(route, { error: { message: 'could not change the key', type: 'server_error' } }, 500); return true; }
  if (/^\/admin\/tokens\/\d+$/.test(p) && m === 'PATCH') { await json(route, { ...tokens.find((x) => x.id === Number(p.split('/').pop())), ...JSON.parse(req.postData()) }); return true; }
  if (/^\/admin\/tokens\/\d+$/.test(p) && m === 'DELETE') { await json(route, {}); return true; }
  if (p === '/admin/usage') { await json(route, { groups: [{ k: 'hermes', calls: 1204, tin: 2000000, tout: 500000 }, { k: 'spent', calls: 3, tin: 1500, tout: 600 }], totals: {} }); return true; }
  if (p === '/admin/routable-models') { await json(route, { data: routable }); return true; }
  if (p === '/v1/models') { v1Auth.push(req.headers()['authorization'] ?? ''); await json(route, { data: [{ id: 'a' }, { id: 'b' }] }); return true; }
  if (p === '/admin/requests') { reqQueries.push(url.search); return false; }
  return false;
});
const dialog = () => page.getByRole('dialog');
const last = (re) => calls.filter((c) => re.test(c));
const card = (name) => page.locator('div.rounded-xl', { has: page.getByRole('switch', { name: new RegExp(`key ${name}$`) }) }).last();

try {
  await page.goto(`${t.base}/connect`);
  await page.getByRole('switch', { name: 'Disable key hermes' }).waitFor();
  check('summary counts keys and what needs attention', (await page.getByText(/6 keys · 3 active/).count()) === 1 && (await page.getByText('1 expiring soon').count()) === 1 && (await page.getByText('1 expired').count()) === 1 && (await page.getByText('1 over budget').count()) === 1);
  check('status chips: active / expired / expiring / spent / disabled',
    (await card('hermes').getByText('Active', { exact: true }).count()) === 1 && (await card('old').getByText('Expired', { exact: true }).count()) === 1
    && (await card('soon').getByText(/Expires in 3 days/).count()) === 1 && (await card('spent').getByText('Budget spent').count()) === 1 && (await card('off').getByText('Disabled', { exact: true }).count()) === 1);
  check('last use is relative', (await card('hermes').getByText('used 2h ago').count()) === 1 && (await card('off').getByText('never used').count()) === 1);
  check('usage reads in plain numbers', (await card('hermes').getByText(/1,204 calls · 2,000,000 in · 500,000 out/).count()) === 1);
  check('the budget meter shows spend against the cap', (await card('spent').getByText('2,100 / 1,000').count()) === 1);
  check('each key links to its requests', (await card('hermes').getByRole('link', { name: 'View requests' }).getAttribute('href')).includes('client=hermes'));

  // filter + search
  await page.getByRole('group', { name: 'Show' }).getByRole('button', { name: /^Needs attention/ }).click();
  check('Needs attention lists expired, expiring and spent keys', (await page.getByRole('switch', { name: /key (old|soon|spent)$/ }).count()) === 3 && (await page.getByRole('switch', { name: /key hermes$/ }).count()) === 0);
  await page.getByRole('group', { name: 'Show' }).getByRole('button', { name: 'Disabled' }).click();
  check('Disabled lists disabled keys', (await page.getByRole('switch', { name: /key off$/ }).count()) === 1 && (await page.getByRole('switch', { name: /key hermes$/ }).count()) === 0);
  await page.getByRole('group', { name: 'Show' }).getByRole('button', { name: 'All' }).click();
  await page.getByLabel('Search keys').fill('deepseek');
  check('search matches allowed models', (await page.getByRole('switch', { name: /key hermes$/ }).count()) === 1 && (await page.getByRole('switch', { name: /key old$/ }).count()) === 0);
  await page.getByLabel('Search keys').fill('');

  // switch error + success
  await page.getByRole('switch', { name: 'Enable key off' }).click();
  await page.getByRole('alert').getByText('could not change the key').waitFor();
  check('a failed toggle is reported', true);
  failToggle = false;
  calls.length = 0;
  await page.getByRole('switch', { name: 'Disable key soon' }).click();
  await page.getByText('Disabled soon.').waitFor();
  check('toggle PATCHes one field and says so', last(/^PATCH \/admin\/tokens\/3 \{"enabled":false\}/).length === 1, calls);

  // revoke
  calls.length = 0;
  await card('hermes').getByRole('button', { name: 'Revoke' }).click();
  await dialog().getByText(/made 1,204 requests in the last 30 days/).waitFor();
  check('revoke warns when the key is in use', (await dialog().getByText(/disable it instead/).count()) === 1);
  await dialog().getByRole('button', { name: 'Revoke', exact: true }).click();
  await page.getByText('Revoked hermes.').waitFor();
  check('revoke deletes and says so', last(/^DELETE \/admin\/tokens\/1/).length === 1, calls);

  // create
  await page.getByRole('button', { name: /Create key/ }).first().click();
  await dialog().getByLabel('Name', { exact: true }).fill('Hermes');
  check('a taken name is refused, case-insensitively, with the consequence', (await dialog().getByText(/already exists. Creating it again would replace that key/).count()) === 1);
  calls.length = 0;
  await dialog().getByLabel('Name', { exact: true }).press('Enter');
  await page.waitForTimeout(250);
  check('Enter on an invalid form creates nothing', last(/^POST/).length === 0, calls);
  await dialog().getByLabel('Name', { exact: true }).fill('laptop');
  await dialog().getByRole('button', { name: 'Full admin' }).click();
  check('admin role shows its warning', (await dialog().getByText(/Keep it for yourself, never for a tool/).count()) === 1);
  await dialog().getByRole('button', { name: 'Inference only' }).click();
  await dialog().getByLabel('Allowed models', { exact: false }).first().fill('deep*');
  check('a glob that cannot cross / says it matches nothing', (await dialog().getByText(/Matches none of the 4 models routable right now/).count()) === 1);
  await dialog().locator('#tk-models').fill('deepseek/*');
  check('the allowlist readout counts matches', (await dialog().getByText(/Matches 2 of 4 routable models/).count()) === 1);
  await dialog().getByRole('button', { name: '90 days', exact: true }).click();
  check('an expiry preset fills the date and reads it back', (await dialog().getByLabel('Expiry date').inputValue()).length === 10 && (await dialog().getByText(/Expires in 90 days/).count()) === 1);
  await dialog().getByRole('button', { name: '1,000,000', exact: true }).click();
  check('a budget quick value sets the cap', (await dialog().getByLabel('Token cap').inputValue()) === '1000000');
  calls.length = 0;
  await dialog().getByLabel('Name', { exact: true }).press('Enter');
  await page.getByRole('dialog', { name: 'New API key' }).waitFor();
  const posted = last(/^POST \/admin\/tokens /)[0] ?? '';
  check('create sends name, role, allowlist and expiry', /"name":"laptop"/.test(posted) && /"roles":\["infer"\]/.test(posted) && /"allowed_models":"deepseek\/\*"/.test(posted) && /"expires_at":"\d{4}-\d\d-\d\d"/.test(posted), posted);
  check('the budget is saved with a second call', last(/^PATCH \/admin\/tokens\/9 .*"cap_tokens":1000000.*"cap_window":"monthly"|^PATCH \/admin\/tokens\/9 .*"cap_window":"monthly".*"cap_tokens":1000000/).length === 1, calls);
  const panel = page.getByRole('dialog', { name: 'New API key' });
  check('the plaintext is shown', (await panel.getByText('ez-secret-123', { exact: true }).count()) === 1);
  check('the snippet already has the real key in it', (await panel.getByText(/OPENAI_API_KEY=ez-secret-123/).count()) === 1);
  await panel.getByRole('button', { name: 'curl' }).click();
  check('the curl tab carries the key too', (await panel.getByText(/Authorization: Bearer ez-secret-123/).count()) === 1);
  await panel.getByRole('button', { name: 'Test this key' }).click();
  await panel.getByText('The key works and can see 2 models.').waitFor();
  check('Test this key calls /v1/models with only that key', v1Auth.at(-1) === 'Bearer ez-secret-123', v1Auth);
  await page.keyboard.press('Escape');
  check('Escape does not dismiss the one-time secret', (await panel.count()) === 1);
  await panel.getByRole('button', { name: 'I saved it' }).click();
  await page.getByText('Created laptop.').waitFor();
  check('closing the panel says what was created', true);

  // policy
  await card('spent').getByRole('button', { name: 'Policy' }).click();
  const save = dialog().getByRole('button', { name: 'Save', exact: true });
  check('Save is disabled until something changes', await save.isDisabled());
  await dialog().getByRole('button', { name: '10,000,000', exact: true }).click();
  check('the cap reads back in words', (await dialog().getByText('10,000,000 tokens per month').count()) === 1);
  await page.keyboard.press('Escape');
  await dialog().getByText('Discard unsaved changes?').waitFor();
  check('Escape with unsaved edits asks first', true);
  calls.length = 0;
  await dialog().getByRole('button', { name: 'Keep editing' }).click();
  await page.waitForTimeout(250);
  check('"Keep editing" does not submit the form', calls.length === 0 && (await dialog().count()) === 1, calls);
  await dialog().getByLabel('Token cap').press('Enter');
  await page.getByText('Saved the policy for spent.').waitFor();
  check('Enter saves the policy', /"cap_tokens":10000000/.test(last(/^PATCH \/admin\/tokens\/4 /)[0] ?? '') && !/expires_at/.test(calls[0] ?? ''), calls);

  // recipes + models browser
  await page.getByRole('group', { name: 'Client' }).getByRole('button', { name: 'Claude Code' }).click();
  check('the Claude Code recipe uses its own variables', (await page.getByText(/ANTHROPIC_AUTH_TOKEN=<your-key>/).count()) >= 1);
  await page.getByRole('group', { name: 'Client' }).getByRole('button', { name: 'Python' }).click();
  check('the Python recipe uses the openai package and the /v1 base', (await page.getByText(/from openai import OpenAI/).count()) === 1 && (await page.getByText(/\/v1", api_key=/).count()) === 1);
  await page.getByLabel('Sample model').click();
  await page.getByRole('option', { name: 'openai/gpt-5', exact: true }).click();
  check('the sample model follows the picker', (await page.getByText(/model="openai\/gpt-5"/).count()) === 1);
  await page.getByText('Models you can call').click();
  await page.getByLabel('Search models').fill('gpt');
  check('the models browser filters and tags ids', (await page.locator('li', { hasText: 'openai/gpt-5' }).count()) === 1 && (await page.locator('li', { hasText: 'deepseek/chat' }).count()) === 0);
  await page.getByLabel('Search models').fill('daily');
  check('a bare id is tagged as a combo', (await page.locator('li', { hasText: 'daily' }).getByText('combo').count()) === 1);

  // deep links
  await page.goto(`${t.base}/connect?new=1`);
  await dialog().getByText('Create an API key').waitFor();
  check('?new=1 opens the create form', true);
  await page.keyboard.press('Escape');
  await page.goto(`${t.base}/connect?policy=4`);
  await dialog().getByText('Key policy — spent').waitFor();
  check('?policy= opens that key’s policy', true);
  await page.keyboard.press('Escape');

  // requests page
  await page.goto(`${t.base}/requests?client=hermes`);
  const keySel = page.locator('select', { has: page.locator('option', { hasText: 'All keys' }) });
  await keySel.waitFor();
  await page.waitForTimeout(400);
  check('Requests sends the API-key filter', reqQueries.some((q) => q.includes('client=hermes')), reqQueries);
  check('Requests shows the key select', (await keySel.inputValue()) === 'hermes');
} catch (e) {
  check('driver ran to completion', false, String(e));
} finally {
  await t.close();
}
finish();
