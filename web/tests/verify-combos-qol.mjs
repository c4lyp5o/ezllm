// Combos QoL: health and traffic on the cards, live block cursor, search and
// filters, single-hop and whole-chain tests, duplicate, editor guards (unsaved
// changes, validation, Enter), presets, hop reorder/duplicate, plan preview,
// deep links, and the Requests page's combo filter. Runs against the built
// web/dist with fixtures (see mockdash.mjs).
import { start, json, check, finish } from './mockdash.mjs';

const t = await start();
const acct = (id, name, host, o = {}) => ({
  id, name, namespace: name.toLowerCase(), kind: 'openai-compatible', base_url: `https://${host}/v1`, enabled: true,
  keys: [{ id, label: 'k', hint: 'x', enabled: true, last_test_ok: true }], quota: null, models_count: 3,
  rate_limit: 0, rate_window: '', protocol_support: { chat: 2, anthropic: 0, responses: 0, untested: 0 }, ...o,
});
const accounts = [acct(1, 'Groq-1', 'api.groq.com'), acct(2, 'Groq-2', 'api.groq.com', { enabled: false }), acct(3, 'OpenRouter', 'openrouter.ai')];
const hop = (a, m, o = {}) => ({ account_id: a, model_id: m, weight: 1, enabled: true, ...o });
const combo = (id, name, o = {}) => ({ id, name, strategy: 'failover', sticky_idle_s: null, enabled: true, block_size: 1, context_size: 200000, hops: [], ...o });
const combos = [
  combo(1, 'daily', { hops: [hop(1, 'a'), hop(2, 'b'), hop(3, 'c')] }),
  combo(2, 'cache-warm', { strategy: 'block_round_robin', block_size: 5, hops: [hop(1, 'a', { hold_until_failure: true }), hop(3, 'b')] }),
  combo(3, 'broken', { hops: [hop(2, 'b')] }),
  combo(4, 'quiet', { enabled: false, hops: [hop(3, 'a')] }),
];

const calls = [];
const usageFroms = [];
const reqQueries = [];
const page = await t.page(async (route, url) => {
  const req = route.request();
  const p = url.pathname;
  const m = req.method();
  if (m !== 'GET') calls.push(`${m} ${p} ${req.postData() ?? ''}`);
  if (p === '/admin/combos' && m === 'GET') { await json(route, combos); return true; }
  if (p === '/admin/accounts') { await json(route, accounts); return true; }
  if (p === '/admin/compression-profiles') { await json(route, [{ id: 1, name: 'lean' }]); return true; }
  if (p === '/admin/usage') {
    usageFroms.push(url.searchParams.get('from'));
    await json(route, { groups: [{ k: 'daily', calls: 1204, errors: 36 }, { k: 'cache-warm', calls: 80, errors: 0 }], totals: {} });
    return true;
  }
  if (p === '/admin/overview') { await json(route, { block_cursors: [{ combo: 'cache-warm', hop: 'groq-1/a', remaining: 2, block_size: 5, reason: 'cold start' }] }); return true; }
  if (/\/admin\/accounts\/\d+\/models$/.test(p)) { await json(route, ['a', 'b', 'c', 'llama'].map((id) => ({ id }))); return true; }
  if (p === '/admin/requests') { reqQueries.push(url.search); return false; }
  if (/\/admin\/combos\/\d+\/test$/.test(p)) {
    const body = JSON.parse(req.postData() || '{}');
    const c = combos.find((x) => p.includes(`/${x.id}/`));
    const hops = c.hops.map((h, i) => ({ index: i, account_id: h.account_id, namespace: accounts.find((a) => a.id === h.account_id).namespace, model: h.model_id, enabled: h.enabled, status: i === 1 ? 429 : 200, ms: 120, cause: i === 1 ? 'quota_exhausted' : undefined, err: i === 1 ? 'quota exhausted' : '' }));
    await json(route, { combo: c.name, surface: 'chat', hops: body.hop == null ? hops : hops.filter((h) => h.index === body.hop) });
    return true;
  }
  if (m !== 'GET' && p.startsWith('/admin/')) { await json(route, {}); return true; }
  return false;
});
const dialog = () => page.getByRole('dialog');
const last = (re) => calls.filter((c) => re.test(c));
const card = (name) => page.locator('div.rounded-xl', { has: page.getByRole('button', { name: `Copy name ${name}` }) });

try {
  await page.goto(`${t.base}/combos`);
  await page.getByRole('button', { name: 'Copy name daily' }).waitFor();
  await page.getByText('1,204 requests').waitFor();
  check('summary counts combos and the ones that cannot serve', (await page.getByText(/4 combos · 3 enabled/).count()) === 1 && (await page.getByText(/1 cannot serve/).count()) === 1);
  const order = await page.getByRole('button', { name: /^Copy name/ }).allTextContents();
  check('a combo that cannot serve is listed first', /broken/.test(order[0]), order);
  check('cannot-serve chip on the dead combo', (await card('broken').getByText('Cannot serve: no usable hop').count()) === 1);
  check('partial health shows usable of total', (await card('daily').getByText('2 of 3 hops usable').count()) === 1);
  check('a hop problem is spelled out', (await card('daily').getByText(/Hop 2: Groq-2 is disabled/).count()) === 1);
  check('traffic and error rate shown', (await card('daily').getByText('3.0% errors').count()) === 1);
  check('a combo with no traffic says so', (await card('broken').getByText(/No requests in 24h/).count()) === 1);
  check('View requests links to the combo filter', (await card('daily').getByRole('link', { name: 'View requests' }).getAttribute('href')).includes('alias=daily'));
  check('live block cursor marks the serving hop', (await card('cache-warm').getByText('▮ 2 left').count()) === 1);
  check('context size on the card', (await card('daily').getByText('200,000 context').count()) === 1);

  // range
  const before = usageFroms.length;
  await page.getByRole('group', { name: 'Traffic range' }).getByRole('button', { name: '7d' }).click();
  await page.waitForTimeout(400);
  check('changing the range refetches traffic', usageFroms.length > before);
  check('the range is remembered', (await page.evaluate(() => localStorage.getItem('ezllm.range'))).includes('7d'));
  await page.getByText('in 7d').first().waitFor();

  // search + filters
  await page.getByLabel('Search combos').fill('openrouter');
  check('search matches provider names inside hops', (await card('daily').count()) === 1 && (await card('broken').count()) === 0);
  await page.getByLabel('Search combos').fill('zzz');
  await page.getByText('No combo matches').waitFor();
  await page.getByRole('button', { name: 'Clear search and filter' }).click();
  await page.getByRole('group', { name: 'Show' }).getByRole('button', { name: 'Disabled' }).click();
  check('Disabled filter shows only disabled combos', (await card('quiet').count()) === 1 && (await card('daily').count()) === 0);
  await page.getByRole('group', { name: 'Show' }).getByRole('button', { name: /^Needs attention/ }).click();
  check('Needs attention shows combos with hop problems', (await card('daily').count()) === 1 && (await card('broken').count()) === 1 && (await card('quiet').count()) === 0);
  await page.getByRole('group', { name: 'Show' }).getByRole('button', { name: 'All' }).click();

  // testing
  calls.length = 0;
  await card('daily').getByTitle(/^Test hop 1/).click();
  await dialog().getByText(/1 of 1 hop answered/).waitFor();
  check('a hop chip tests only that hop', last(/combos\/1\/test/).length === 1 && /"hop":0/.test(calls[0]), calls);
  await dialog().getByText('Close', { exact: true }).click();
  await card('daily').getByText(/Tested .* 1 of 1 answered/).waitFor();
  check('the card remembers the probe', (await card('daily').getByText('✓').count()) === 1);
  calls.length = 0;
  await card('daily').getByRole('button', { name: 'Test', exact: true }).click();
  await dialog().getByText(/2 of 3 hops answered/).waitFor();
  check('Test probes the whole chain', last(/combos\/1\/test/).length === 1 && !/"hop"/.test(calls[0]), calls);
  check('a failed hop shows its cause', (await dialog().getByText(/429 · quota exhausted/).count()) === 1);
  calls.length = 0;
  await dialog().getByRole('button', { name: 'Test hop 2 again' }).click();
  await dialog().getByRole('button', { name: 'Test hop 2 again' }).waitFor();
  check('Test again re-probes one hop', /"hop":1/.test(calls[0] ?? ''), calls);
  await page.keyboard.press('Escape');
  await card('daily').getByText(/Tested .* 2 of 3 answered/).waitFor();
  check('card summarises the last full test', true);

  // duplicate
  calls.length = 0;
  await card('cache-warm').getByRole('button', { name: 'Duplicate' }).click();
  await dialog().getByText('Duplicate cache-warm').waitFor();
  check('duplicate pre-fills a non-colliding name', (await dialog().getByLabel('Name', { exact: true }).inputValue()) === 'cache-warm-copy');
  await dialog().getByRole('button', { name: 'Create combo' }).click();
  await page.waitForTimeout(300);
  const dup = last(/^POST \/admin\/combos /)[0] ?? '';
  check('duplicate posts the copy with the same hops and strategy', /"name":"cache-warm-copy"/.test(dup) && /block_round_robin/.test(dup) && (dup.match(/"account_id"/g) ?? []).length === 2 && /"hold_until_failure":true/.test(dup), dup);

  // editor guards
  await page.getByRole('button', { name: /New combo/ }).first().click();
  await dialog().getByLabel('Name', { exact: true }).fill('daily');
  await dialog().getByText(/already exists/).waitFor();
  check('a taken name is refused with the reason', true);
  await dialog().getByLabel('Name', { exact: true }).fill('a/b');
  check('a slash in the name is refused', (await dialog().getByText(/No "\/" in the name/).count()) === 1);
  calls.length = 0;
  await dialog().getByLabel('Name', { exact: true }).press('Enter');
  await page.waitForTimeout(250);
  check('Enter on an invalid draft saves nothing', last(/^POST/).length === 0, calls);
  check('invalid draft says what to fix', (await dialog().getByText('Fix the fields marked in red to save.').count()) === 1);
  await dialog().getByRole('button', { name: /Cache-warm rotation/ }).click();
  check('a preset sets the strategy', (await dialog().getByRole('button', { name: 'Block round robin' }).getAttribute('aria-pressed')) === 'true' && (await dialog().getByLabel('Block size').inputValue()) === '5');
  await page.keyboard.press('Escape');
  await dialog().getByText('Discard unsaved changes?').waitFor();
  check('Escape with unsaved edits asks first', true);
  await dialog().getByRole('button', { name: 'Keep editing' }).click();

  // build a valid one by Enter, with hops
  await dialog().getByLabel('Name', { exact: true }).fill('fresh');
  await dialog().getByRole('button', { name: /Add hop/ }).click();
  await dialog().getByLabel('Hop 1 model').waitFor();
  check('an empty hop is flagged', (await dialog().getByText('Pick or type a model.').count()) === 1);
  await dialog().getByLabel('Hop 1 model').click();
  await dialog().getByRole('option', { name: 'b', exact: true }).click();
  await dialog().getByRole('button', { name: 'Duplicate', exact: true }).first().click();
  await page.waitForTimeout(150);
  check('duplicate hop is flagged as a repeat', (await dialog().getByText(/Same provider and model as hop 1/).count()) === 1);
  await dialog().getByRole('button', { name: 'Remove hop 2' }).click();
  check('plan preview describes the strategy', (await dialog().getByText(/serves the next 5 requests/).count()) === 1);
  calls.length = 0;
  await dialog().getByLabel('Name', { exact: true }).press('Enter');
  await page.waitForTimeout(300);
  check('Enter saves a valid draft', last(/^POST \/admin\/combos .*"name":"fresh"/).length === 1, calls);
  await dialog().waitFor({ state: 'detached', timeout: 3000 });
  check('saving shows a notice', (await page.getByText('Created fresh.').count()) === 1);

  // edit: dirty gate, reorder
  await card('daily').getByRole('button', { name: 'Edit', exact: true }).click();
  const save = dialog().getByRole('button', { name: 'Save changes' });
  check('Save is disabled until something changes', await save.isDisabled());
  check('plan preview skips an unusable hop and says why', (await dialog().getByText(/Groq-2 \/ b is skipped: Groq-2 is disabled/).count()) === 1);
  await dialog().getByRole('button', { name: 'Move hop 3 up' }).click();
  await dialog().getByRole('button', { name: 'Move hop 2 up' }).click();
  await page.waitForTimeout(200);
  check('moving a hop reorders the rows', (await dialog().getByLabel('Hop 1 provider').inputValue()) === '3');
  check('Save enables once dirty', await save.isEnabled());
  calls.length = 0;
  await save.click();
  await page.waitForTimeout(300);
  const patched = last(/^PATCH \/admin\/combos\/1 /)[0] ?? '';
  check('PATCH carries the new order', /"hops":\[\{"account_id":3,"model_id":"c"/.test(patched), patched);
  check('the UI-only key never reaches the wire', !/uid|custom/.test(patched), patched);

  // delete confirm
  await card('daily').getByRole('button', { name: 'Delete' }).click();
  await page.getByText(/served 1,204 requests/).waitFor();
  check('delete confirm names the traffic it would cut off', (await page.getByText(/Its 3 hops will be removed/).count()) === 1);
  await page.keyboard.press('Escape');

  // switch
  calls.length = 0;
  await page.getByRole('switch', { name: 'Disable combo daily' }).click();
  await page.waitForTimeout(300);
  check('enable switch PATCHes one field', last(/^PATCH \/admin\/combos\/1 \{"enabled":false\}/).length === 1, calls);

  // deep links
  calls.length = 0;
  await page.goto(`${t.base}/combos?edit=2`);
  await dialog().getByText('Edit cache-warm').waitFor();
  check('?edit= opens that combo', true);
  await page.keyboard.press('Escape');
  await page.goto(`${t.base}/combos?test=3`);
  await dialog().getByText('Test broken').waitFor();
  check('?test= opens and runs the test', last(/combos\/3\/test/).length === 1, calls);

  // requests page
  await page.goto(`${t.base}/requests?alias=daily`);
  const comboSel = page.locator('select', { has: page.locator('option', { hasText: 'All combos' }) });
  await comboSel.waitFor();
  await page.waitForTimeout(400);
  check('Requests sends the combo filter', reqQueries.some((q) => q.includes('alias=daily')), reqQueries);
  check('Requests shows the combo select', (await comboSel.inputValue()) === 'daily');
} catch (e) {
  check('driver ran to completion', false, String(e));
} finally {
  await t.close();
}
finish();
