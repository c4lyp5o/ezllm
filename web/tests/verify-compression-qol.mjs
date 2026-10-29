// Compression QoL: profile cards in words (stages, settings, which combos use
// them), enable switch, delete guard, the savings range / delta / breakdown,
// the editor modal (presets, guards, validation, advice, stage actions), "try
// it on a sample" from a card and from an unsaved draft, and deep links. Runs
// against the built web/dist with fixtures (see mockdash.mjs).
import { start, json, check, finish } from './mockdash.mjs';

const t = await start();
const prof = (id, name, o = {}) => ({ id, name, enabled: true, stages: [{ engine: 'lite' }], exempt_last_turn: true, min_compress_ratio: 0.05, fail_open: true, auto_trigger_tokens: 0, fresh_tool_output: 'keep', notes: '', ...o });
const profiles = [
  prof(1, 'agent-output', { stages: [{ engine: 'rtk', options: { keep_lines: 6, tool_classes: { terminal: 'log' } } }, { engine: 'tool_ref' }], auto_trigger_tokens: 2000, fresh_tool_output: 'compress', notes: 'for the coding agent' }),
  prof(2, 'idle', { enabled: false }),
];
const combos = [{ id: 5, name: 'daily', strategy: 'failover', enabled: true, block_size: 1, context_size: 200000, compression_profile_id: 1, hops: [] }];
const summary = (saved) => ({ count: 9, tin: 1, tout: 1, cread: 0, cwrite: 0, max_tin: 1, max_tout: 1, context_pre: 0, context_saved: 0, compression_saved: saved, applied_calls: 40, applied_saved: saved, declined_calls: 10, held_notional: 3000 });

const calls = [];
const reqHours = [];
let previewFail = false;
const page = await t.page(async (route, url) => {
  const req = route.request();
  const p = url.pathname;
  const m = req.method();
  if (m !== 'GET') calls.push(`${m} ${p} ${req.postData() ?? ''}`);
  if (p === '/admin/compression-profiles' && m === 'GET') { await json(route, profiles); return true; }
  if (p === '/admin/combos') { await json(route, combos); return true; }
  if (p === '/admin/usage') {
    const g = url.searchParams.get('group_by');
    await json(route, { groups: g === 'alias' ? [{ k: 'daily', calls: 40, saved: 12000 }, { k: '(direct)', calls: 2, saved: 100 }] : [{ k: 'Groq', calls: 40, saved: 12000 }], totals: {} });
    return true;
  }
  if (p === '/admin/requests') {
    const hours = Math.round((Date.now() - Date.parse(url.searchParams.get('from'))) / 3600e3);
    reqHours.push(hours);
    const base = hours > 36 && hours < 60 ? 20000 : hours > 200 && hours < 400 ? 30000 : hours > 400 ? 50000 : 12000;
    await json(route, { rows: [], summary: summary(base), filter: { from: '', to: '', limit: 1, sort: 'ts_desc' } });
    return true;
  }
  if (p === '/admin/compression-preview') {
    if (previewFail) { await json(route, { error: { message: 'stage 0: unknown engine "x"', type: 'invalid_request_error' } }, 400); return true; }
    const body = JSON.parse(req.postData() || '{}');
    await json(route, { kind: body.kind, tool: body.tool ?? '', applied: true, tokens_before: 1000, tokens_after: 400, saved_notional: 0, note: '', after: 'FOLDED OUTPUT x80', stages: [{ engine: 'fold', pre: 1000, post: 400, saved: 600, fired: 3, cpu_ms: 1 }] });
    return true;
  }
  if (m !== 'GET' && p.startsWith('/admin/')) { await json(route, {}); return true; }
  return false;
});
const dialog = () => page.getByRole('dialog');
const last = (re) => calls.filter((c) => re.test(c));
const card = (name) => page.locator('article', { has: page.getByText(name, { exact: true }) });

try {
  await page.goto(`${t.base}/compression`);
  await page.locator('article').first().waitFor();
  await page.getByText('12,000 rewritten').waitFor();
  check('summary counts profiles and unattached ones', (await page.getByText(/2 profiles · 1 enabled · 1 not used by any combo/).count()) === 1);
  check('stages read in words, not JSON', (await card('agent-output').getByText('RTK · keeps 6 lines, 1 tool class').count()) === 1 && (await card('agent-output').getByText('Tool reference · from 200 bytes').count()) === 1);
  check('settings read as a sentence', (await card('agent-output').getByText(/Keeps a rewrite only if it saves at least 5% · leaves the final turn alone · trims a fresh tool result on its first send · starts above 2,000 tokens\./).count()) === 1);
  check('the combos using a profile are linked', (await card('agent-output').getByRole('link', { name: 'daily' }).getAttribute('href')) === '/combos?edit=5');
  check('an unused profile says it does nothing', (await card('idle').getByText(/Not used by any combo/).count()) === 1);
  check('the old JSON dump is gone', (await page.getByText('"keep_lines"').count()) === 0);

  // savings
  check('rewritten figure shows the change against the previous period', (await page.getByText('▲ 50% vs previous 24h').count()) === 1, reqHours);
  check('the previous period was fetched as a 2x window', reqHours.some((h) => h >= 46 && h <= 50), reqHours);
  check('breakdown by provider shows providers', (await page.getByText('Groq', { exact: true }).count()) >= 1);
  await page.getByRole('group', { name: 'Savings breakdown' }).getByRole('button', { name: 'Combo' }).click();
  check('breakdown by combo links to the combo’s requests', (await page.getByRole('link', { name: 'daily' }).filter({ hasText: 'daily' }).last().getAttribute('href')).includes('alias=daily'));
  check('direct calls are not a combo row', (await page.getByText('(direct)').count()) === 0);
  const before = reqHours.length;
  await page.getByRole('group', { name: 'Savings range' }).getByRole('button', { name: '7d' }).click();
  await page.getByText('Actual savings · last 7d').waitFor();
  await page.waitForTimeout(400);
  check('changing the range refetches savings', reqHours.length > before && reqHours.slice(before).some((h) => h > 150 && h < 180), reqHours.slice(before));
  await page.getByRole('group', { name: 'Savings range' }).getByRole('button', { name: '24h' }).click();

  // switch + notices
  calls.length = 0;
  await page.getByRole('switch', { name: 'Pause profile agent-output' }).click();
  await page.getByText('Paused agent-output.').waitFor();
  check('the switch PATCHes one field and says so', last(/^PATCH \/admin\/compression-profiles\/1 \{"enabled":false\}/).length === 1, calls);

  // delete guard
  calls.length = 0;
  await card('agent-output').getByRole('button', { name: 'Delete' }).click();
  await page.getByText(/agent-output is still used by/).waitFor();
  check('deleting a used profile explains and links the combos', (await page.getByRole('alert').getByRole('link', { name: 'daily' }).count()) === 1 && (await page.getByRole('dialog').count()) === 0);
  check('nothing was sent', calls.length === 0, calls);
  await card('idle').getByRole('button', { name: 'Delete' }).click();
  await dialog().getByText(/No combo uses it/).waitFor();
  await dialog().getByRole('button', { name: 'Delete', exact: true }).click();
  await page.getByText('Deleted idle.').waitFor();
  check('an unused profile deletes after a confirm', last(/^DELETE \/admin\/compression-profiles\/2/).length === 1, calls);

  // try it (saved profile)
  calls.length = 0;
  await card('agent-output').getByRole('button', { name: 'Try it' }).click();
  await dialog().getByText('Try agent-output').waitFor();
  const run = dialog().getByRole('button', { name: 'Run on sample' });
  check('Run needs a sample', await run.isDisabled());
  await dialog().getByLabel('Sample', { exact: true }).fill('line\n'.repeat(80));
  await run.click();
  await dialog().getByText('Would ship shorter').waitFor();
  check('a saved profile previews by id', last(/^POST \/admin\/compression-preview /).length === 1 && /"profile_id":1/.test(calls[0]) && /"tool":"terminal"/.test(calls[0]), calls);
  check('the result shows before and after tokens', (await dialog().getByText(/1,000.*400 tokens/).count()) === 1 && (await dialog().getByText(/saves 600 \(60%\)/).count()) === 1);
  check('per-stage savings are tabulated', (await dialog().getByRole('cell', { name: 'Fold repeats' }).count()) === 1);
  check('the shortened text is shown', (await dialog().getByText('FOLDED OUTPUT x80').count()) === 1);
  await dialog().getByRole('button', { name: 'Prompt' }).click();
  await dialog().getByLabel('Ignore the minimum saving').check();
  calls.length = 0;
  await run.click();
  await page.waitForTimeout(300);
  check('kind and ignore-floors travel in the request', /"kind":"prompt"/.test(calls[0] ?? '') && /"ignore_floors":true/.test(calls[0] ?? '') && !/"tool"/.test(calls[0] ?? ''), calls);
  await page.keyboard.press('Escape');
  await dialog().waitFor({ state: 'detached' });

  // editor: presets, validation, guards
  await page.getByRole('button', { name: /New profile/ }).first().click();
  await dialog().getByRole('button', { name: /Agent tool output/ }).click();
  check('a preset fills the name and the pipeline', (await dialog().getByLabel('Profile name').inputValue()) === 'agent-output' && (await dialog().getByLabel('Engine').count()) === 6);
  check('the taken name is refused with the reason', (await dialog().getByText(/already exists/).count()) === 1);
  await dialog().getByLabel('Profile name').fill('off');
  check('the reserved name is refused', (await dialog().getByText(/"off" is reserved/).count()) === 1);
  await dialog().getByLabel('Profile name').fill('mine');
  check('each stage says what its engine does', (await dialog().getByText(/Collapses runs of exactly identical log lines/).count()) >= 1);
  check('tool reference before other tool-output stages is flagged', (await dialog().getByText(/Tool reference works best last/).count()) === 0, 'preset order is already last');
  await dialog().getByRole('button', { name: 'Move stage 6 up' }).click();
  check('moving tool reference up triggers the advice', (await dialog().getByText(/Tool reference works best last/).count()) === 1);
  await dialog().getByRole('button', { name: 'Move stage 5 down' }).click();
  await dialog().getByRole('button', { name: /Add stage/ }).click();
  await dialog().getByRole('button', { name: 'Duplicate', exact: true }).first().click();
  check('a repeated engine is flagged', (await dialog().getByText(/appears more than once/).count()) >= 1);
  await dialog().getByLabel('Keep lines').fill('abc');
  check('a bad option is refused inline', (await dialog().getByText('Keep lines must be a whole number, 0 or more.').count()) === 1);
  calls.length = 0;
  await dialog().getByLabel('Profile name').press('Enter');
  await page.waitForTimeout(250);
  check('Enter on an invalid draft saves nothing', last(/^POST/).length === 0, calls);
  check('it says what to fix', (await dialog().getByText('Fix the fields marked in red to save.').count()) === 1);
  await dialog().getByLabel('Keep lines').fill('8');

  // try it from the unsaved draft
  await dialog().getByText('Try it on a sample').click();
  await dialog().getByLabel('Sample', { exact: true }).fill('x\n'.repeat(40));
  calls.length = 0;
  await dialog().getByRole('button', { name: 'Run on sample' }).click();
  await dialog().getByText('Would ship shorter').waitFor();
  check('an unsaved draft previews inline, with its own stages', /"profile":\{"stages":\[/.test(calls[0] ?? '') && /"keep_lines":8/.test(calls[0] ?? '') && !/profile_id/.test(calls[0] ?? ''), calls[0]);
  previewFail = true;
  await dialog().getByRole('button', { name: 'Run on sample' }).click();
  await dialog().getByText(/unknown engine/).waitFor();
  check('a preview failure shows the reason and clears the result', (await dialog().getByText('Would ship shorter').count()) === 0);
  previewFail = false;

  await page.keyboard.press('Escape');
  await dialog().getByText('Discard unsaved changes?').waitFor();
  check('Escape with unsaved edits asks first', true);
  await dialog().getByRole('button', { name: 'Keep editing' }).click();
  await dialog().getByLabel('Profile name').fill('mine');
  calls.length = 0;
  await dialog().getByLabel('Profile name').press('Enter');
  await page.getByText('Created mine.').waitFor();
  const posted = last(/^POST \/admin\/compression-profiles /)[0] ?? '';
  check('Enter saves a valid profile', /"name":"mine"/.test(posted) && /"keep_lines":8/.test(posted) && !/uid/.test(posted), posted);

  // duplicate + edit dirty gate
  await card('agent-output').getByRole('button', { name: 'Duplicate' }).click();
  await dialog().getByText('Duplicate agent-output').waitFor();
  check('duplicate pre-fills a non-colliding name', (await dialog().getByLabel('Profile name').inputValue()) === 'agent-output-copy');
  await page.keyboard.press('Escape');
  await dialog().waitFor({ state: 'detached' });
  await card('agent-output').getByRole('button', { name: 'Edit' }).click();
  check('Save is disabled until something changes', await dialog().getByRole('button', { name: 'Save profile' }).isDisabled());
  check('the ratio shows as a percent', (await dialog().getByLabel(/Keep a rewrite only if it saves at least/).inputValue()) === '5');
  await dialog().getByLabel(/Keep a rewrite only if it saves at least/).fill('20');
  calls.length = 0;
  await dialog().getByRole('button', { name: 'Save profile' }).click();
  await page.waitForTimeout(300);
  check('the percent is sent as a ratio', /"min_compress_ratio":0.2/.test(last(/^PATCH \/admin\/compression-profiles\/1 /)[0] ?? ''), calls);

  // deep links
  await page.goto(`${t.base}/compression?edit=1`);
  await dialog().getByText('Edit agent-output').waitFor();
  check('?edit= opens that profile', true);
  await page.keyboard.press('Escape');
  await page.goto(`${t.base}/compression?new=1`);
  await dialog().getByText('New profile').waitFor();
  check('?new=1 opens a blank editor', (await dialog().getByLabel('Profile name').inputValue()) === '');
} catch (e) {
  check('driver ran to completion', false, String(e));
} finally {
  await t.close();
}
finish();
