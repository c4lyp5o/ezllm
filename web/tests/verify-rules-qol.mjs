// Rules QoL: the readable table (provider names, window phrases, day strip,
// collapsed model chips), search and filters, stale-rule flags, the sectioned
// editor (guards, validation, presets, live readouts, sentence), duplicate,
// the large fan-out confirm, notices and deep links. Runs against the built
// web/dist with fixtures (see mockdash.mjs).
import { start, json, check, finish } from './mockdash.mjs';

const t = await start();
const acct = (id, name, ns, o = {}) => ({ id, name, namespace: ns, kind: 'openai-compatible', base_url: `https://${ns}.test/v1`, enabled: true, keys: [], quota: null, models_count: 3, ...o });
const accounts = [acct(1, 'Groq', 'groq'), acct(2, 'OpenRouter', 'openrouter', { enabled: false })];
const rule = (id, a, m, o = {}) => ({ id, account_id: a, model_id: m, cap_tokens: 0, cap_window: 'monthly', win_start: '', win_end: '', win_days: '', win_tz: 'instance', enabled: true, note: '', ...o });
const many = ['m1', 'm2', 'm3', 'm4', 'm5', 'm6', 'm7'];
const bulk = Array.from({ length: 25 }, (_, i) => `bulk-${i}`);
const rules = [
  ...many.map((m, i) => rule(10 + i, 1, m, { cap_tokens: 5000000, win_start: '09:00', win_end: '18:00', win_days: '1,2,3,4,5', win_tz: 'Asia/Kuala_Lumpur', note: 'day models' })),
  rule(30, 1, 'gone-model', { cap_tokens: 100000, cap_window: 'daily' }),
  rule(31, 2, 'x', { win_start: '22:00', win_end: '06:00', win_days: '0,6' }),
  rule(32, 1, 'a', { enabled: false }),
];
const calls = [];
const page = await t.page(async (route, url) => {
  const req = route.request();
  const p = url.pathname;
  const m = req.method();
  if (m !== 'GET') calls.push(`${m} ${p} ${req.postData() ?? ''}`);
  if (p === '/admin/model-rules' && m === 'GET') { await json(route, rules); return true; }
  if (p === '/admin/accounts') { await json(route, accounts); return true; }
  if (/accounts\/\d+\/models/.test(p)) { await json(route, [...many, 'a', 'x', 'b', ...bulk].map((id) => ({ id }))); return true; }
  if (m !== 'GET' && p.startsWith('/admin/')) { await json(route, {}); return true; }
  return false;
});
const dialog = () => page.getByRole('dialog');
const last = (re) => calls.filter((c) => re.test(c));
const row = (model) => page.locator('div.border-t', { has: page.getByRole('switch', { name: new RegExp(`^enable .*\\b${model}\\b`) }) });

try {
  await page.goto(`${t.base}/rules`);
  await page.getByRole('button', { name: 'Edit', exact: true }).first().waitFor();
  await page.getByText('Groq no longer lists gone-model').waitFor();
  check('summary counts rules, disabled and attention', (await page.getByText(/10 rules on 2 providers · 1 disabled · 2 need attention/).count()) === 1);
  check('the provider shows by name, not namespace', (await page.getByText('OpenRouter', { exact: true }).count()) >= 1 && (await page.getByText('openrouter', { exact: true }).count()) === 0);
  check('window reads as a phrase', (await page.getByText('Weekdays 09:00–18:00').count()) === 1);
  check('timezone shown beside the window', (await page.getByText('Asia/Kuala_Lumpur', { exact: true }).count()) === 1);
  check('day strip names the allowed days', (await page.getByRole('img', { name: 'Allowed days: Mon, Tue, Wed, Thu, Fri' }).count()) === 1);
  check('a group of 7 models shows 4 chips and a count', (await page.getByText('m4', { exact: true }).count()) === 1 && (await page.getByText('m5', { exact: true }).count()) === 0 && (await page.getByText('+3 more').count()) === 1);
  await page.getByText('+3 more').click();
  check('+N more expands in place', (await page.getByText('m7', { exact: true }).count()) === 1);
  await page.getByText('Show fewer').click();
  check('Show fewer collapses again', (await page.getByText('m7', { exact: true }).count()) === 0);
  check('a model the provider dropped is flagged', (await page.getByText('Groq no longer lists gone-model.').count()) === 1);
  check('a rule on a disabled provider is flagged', (await page.getByText('OpenRouter is disabled, so this rule has no effect for now.').count()) === 1);
  check('a disabled rule reports no issue', (await row('a').getByText(/no longer lists|disabled, so/).count()) === 0);
  await page.getByText('Groq no longer lists gone-model.').locator('..').getByRole('button', { name: 'Fix' }).click();
  await dialog().getByText('Edit rule').waitFor();
  check('Fix opens the rule for editing', (await dialog().locator('#rl-model-gone-model').isChecked()));
  check('a model missing from the catalog is still listed, marked', (await dialog().getByText('not in catalog').count()) === 1);
  await page.keyboard.press('Escape');
  await dialog().waitFor({ state: 'detached' });

  // search + filters
  await page.getByLabel('Search rules').fill('day models');
  check('search matches notes', (await page.getByText('gone-model', { exact: true }).count()) === 0 && (await page.getByText('m1', { exact: true }).count()) === 1);
  await page.getByLabel('Search rules').fill('zzz');
  await page.getByText('No rule matches').waitFor();
  await page.getByRole('button', { name: 'Clear search and filters' }).click();
  await page.getByLabel('Provider', { exact: true }).selectOption('2');
  check('provider filter narrows the list', (await page.getByText('gone-model', { exact: true }).count()) === 0 && (await page.getByText('x', { exact: true }).count()) === 1);
  await page.getByLabel('Provider', { exact: true }).selectOption('0');
  await page.getByRole('group', { name: 'Show' }).getByRole('button', { name: 'Disabled' }).click();
  check('Disabled filter', (await page.getByRole('switch', { name: 'enable a' }).count()) === 1 && (await page.getByText('gone-model', { exact: true }).count()) === 0);
  await page.getByRole('group', { name: 'Show' }).getByRole('button', { name: /^Needs attention/ }).click();
  check('Needs attention filter', (await page.getByText('gone-model', { exact: true }).count()) === 1 && (await page.getByText('m1', { exact: true }).count()) === 0);
  await page.getByRole('group', { name: 'Show' }).getByRole('button', { name: 'All' }).click();

  // toggle notice
  calls.length = 0;
  await page.getByRole('switch', { name: 'enable gone-model' }).click();
  await page.getByText('Disabled the rule for gone-model.').waitFor();
  check('a toggle PATCHes the full row and says so', last(/^PATCH \/admin\/model-rules\/30 (?=.*"enabled":false)(?=.*"cap_tokens":100000)/).length === 1, calls);

  // editor guards + readouts
  await page.getByRole('button', { name: 'Edit', exact: true }).first().click();
  const save = dialog().getByRole('button', { name: 'Save rule' });
  check('Save is disabled until something changes', await save.isDisabled());
  check('sentence describes the rule', (await dialog().getByText(/7 models on Groq are capped at 5,000,000 tokens per month and may only run 09:00–18:00 on weekdays \(Asia\/Kuala_Lumpur\)/).count()) === 1);
  check('the business-hours preset shows as active', (await dialog().getByRole('button', { name: 'Business hours' }).getAttribute('aria-pressed')) === 'true');
  await dialog().getByRole('button', { name: '1,000,000', exact: true }).click();
  check('a quick value sets the cap', (await dialog().getByLabel('Cap tokens').inputValue()) === '1000000' && (await dialog().getByText('1,000,000 tokens per month').count()) >= 1);
  await dialog().getByRole('button', { name: 'Day', exact: true }).click();
  check('the period is chosen with a segmented control', (await dialog().getByText('1,000,000 tokens per day').count()) >= 1);
  await dialog().getByLabel('Cap tokens').fill('0');
  check('no cap hides the period choice', (await dialog().getByRole('group', { name: 'Cap period' }).count()) === 0);
  await dialog().getByLabel('Cap tokens').fill('2000000');
  await page.keyboard.press('Escape');
  await dialog().getByText('Discard unsaved changes?').waitFor();
  check('Escape with unsaved edits asks first', true);
  await dialog().getByRole('button', { name: 'Keep editing' }).click();
  calls.length = 0;
  await dialog().getByLabel('Cap tokens').press('Enter');
  await page.waitForTimeout(400);
  check('Enter saves every rule in the group', last(/^PATCH \/admin\/model-rules\/1\d .*"cap_tokens":2000000/).length === 7, calls.length);
  await page.getByText('Saved 7 rules.').waitFor();
  check('save shows a notice', true);

  // validation
  await page.getByRole('button', { name: '+ New rule' }).click();
  calls.length = 0;
  await dialog().getByLabel('Cap tokens').press('Enter');
  await page.waitForTimeout(250);
  check('Enter with no model saves nothing', last(/^POST/).length === 0, calls);
  check('the missing model is explained', (await dialog().getByText('Select at least one model.').count()) >= 1);
  await dialog().getByLabel('Window start').fill('10:00');
  check('a half window is refused at once', (await dialog().getByText('A window needs both a start and an end, or neither.').count()) === 1);
  await dialog().getByRole('button', { name: 'Nights' }).click();
  check('a preset fills the times', (await dialog().getByLabel('Window start').inputValue()) === '22:00' && (await dialog().getByLabel('Window end').inputValue()) === '06:00');
  await dialog().getByRole('button', { name: 'Weekend', exact: true }).click();
  check('the weekday shortcut sets days', (await dialog().getByRole('button', { name: 'Sat' }).getAttribute('aria-pressed')) === 'true' && (await dialog().getByRole('button', { name: 'Mon' }).getAttribute('aria-pressed')) === 'false');
  await dialog().getByLabel('Search models').fill('bulk-');
  await dialog().getByRole('button', { name: /Select all 25 matches/ }).click();
  await dialog().getByRole('button', { name: 'Save rule' }).click();
  await page.getByText('Change 25 rules?').waitFor();
  check('a big fan-out asks in a styled confirm', true);
  calls.length = 0;
  await page.getByRole('dialog').last().getByRole('button', { name: 'Cancel' }).click();
  await page.waitForTimeout(200);
  check('declining the fan-out writes nothing', calls.length === 0, calls);
  await dialog().getByRole('button', { name: 'Save rule' }).click();
  await page.getByRole('dialog').last().getByRole('button', { name: 'Continue' }).click();
  await page.getByText('Saved 25 rules.').waitFor();
  check('accepting the fan-out writes one rule per model', last(/^POST \/admin\/model-rules .*bulk-/).length === 25, calls.length);

  // duplicate
  await row('m1').getByRole('button', { name: 'Duplicate' }).click();
  await dialog().getByText('Duplicate rule').waitFor();
  check('duplicate copies the settings but no models', (await dialog().getByLabel('Cap tokens').inputValue()) === '2000000' || (await dialog().getByLabel('Cap tokens').inputValue()) === '5000000');
  check('duplicate starts with nothing selected', (await dialog().getByText('0 selected').count()) === 1);
  await dialog().locator('#rl-model-b').check();
  await dialog().locator('#rl-model-a').check();
  check('a model that already has a rule says so', (await dialog().getByText('has a rule').count()) >= 1);
  calls.length = 0;
  await dialog().getByRole('button', { name: 'Save rule' }).click();
  await page.waitForTimeout(400);
  check('duplicate creates new rules and updates existing ones', last(/^POST .*"model_id":"b"/).length === 1 && last(/^PATCH \/admin\/model-rules\/32 /).length === 1, calls);

  // delete confirm
  await row('m1').getByRole('button', { name: 'Delete' }).click();
  check('delete names every model it removes', (await page.getByRole('dialog').getByText(/m1, m2, m3, m4, m5, m6, m7/).count()) === 1);
  calls.length = 0;
  await page.getByRole('dialog').getByRole('button', { name: 'Delete', exact: true }).click();
  await page.getByText('Deleted 7 rules.').waitFor();
  check('delete removes each rule and says so', last(/^DELETE/).length === 7, calls.length);

  // deep links
  await page.goto(`${t.base}/rules?edit=31`);
  await dialog().getByText('Edit rule').waitFor();
  check('?edit= opens that rule', (await dialog().locator('#rl-model-x').isChecked()));
  await page.keyboard.press('Escape');
  await page.goto(`${t.base}/rules?new=1&account=2`);
  await dialog().getByText('New rule').waitFor();
  check('?new=1&account= starts on that provider', (await dialog().getByLabel('Provider').inputValue()) === '2');
} catch (e) {
  check('driver ran to completion', false, String(e));
} finally {
  await t.close();
}
finish();
