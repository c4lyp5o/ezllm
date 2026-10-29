// Screenshot helper for the Compression page (list, editor, try-it; desktop and phone).
import { start, json } from './mockdash.mjs';
const out = process.argv[2] ?? '/tmp';
const t = await start();
const prof = (id, name, o = {}) => ({ id, name, enabled: true, stages: [{ engine: 'lite' }], exempt_last_turn: true, min_compress_ratio: 0.05, fail_open: true, auto_trigger_tokens: 0, fresh_tool_output: 'keep', notes: '', ...o });
const profiles = [
  prof(1, 'agent-output', { stages: [{ engine: 'ansi' }, { engine: 'fold' }, { engine: 'rtk', options: { keep_lines: 6, tool_classes: { terminal: 'log' } } }, { engine: 'tool_ref' }], auto_trigger_tokens: 2000, fresh_tool_output: 'compress', notes: 'for the coding agent' }),
  prof(2, 'idle', { enabled: false }),
];
const combos = [{ id: 5, name: 'daily', strategy: 'failover', enabled: true, block_size: 1, context_size: 200000, compression_profile_id: 1, hops: [] }];
const page = await t.page(async (route, url) => {
  const p = url.pathname;
  if (p === '/admin/compression-profiles') { await json(route, profiles); return true; }
  if (p === '/admin/combos') { await json(route, combos); return true; }
  if (p === '/admin/usage') { await json(route, { groups: [{ k: url.searchParams.get('group_by') === 'alias' ? 'daily' : 'Groq', calls: 40, saved: 12000 }], totals: {} }); return true; }
  if (p === '/admin/requests') { await json(route, { rows: [], summary: { count: 9, tin: 1, tout: 1, cread: 0, cwrite: 0, max_tin: 1, max_tout: 1, context_pre: 0, context_saved: 0, compression_saved: 12000, applied_calls: 40, applied_saved: 12000, declined_calls: 10, held_notional: 3000 }, filter: {} }); return true; }
  if (p === '/admin/compression-preview') { await json(route, { kind: 'tool_output', tool: 'terminal', applied: true, tokens_before: 1000, tokens_after: 400, saved_notional: 0, note: '', after: 'compiling module alpha ... ok (x80)\ndone', stages: [{ engine: 'fold', pre: 1000, post: 400, saved: 600, fired: 3, cpu_ms: 1 }] }); return true; }
  return false;
});
await page.goto(`${t.base}/compression`);
await page.locator('article').first().waitFor();
await page.waitForTimeout(800);
await page.screenshot({ path: `${out}/comp-list.png`, fullPage: true });
await page.getByRole('button', { name: 'Edit' }).first().click();
await page.getByRole('dialog').waitFor();
await page.getByText('Try it on a sample').click();
await page.getByLabel('Sample', { exact: true }).fill('compiling module alpha ... ok\n'.repeat(80));
await page.getByRole('button', { name: 'Run on sample' }).click();
await page.getByText('Would ship shorter').waitFor();
await page.waitForTimeout(500);
await page.screenshot({ path: `${out}/comp-edit.png` });
await page.getByRole('dialog').locator('div.overflow-y-auto').first().evaluate((el) => { el.scrollTop = el.scrollHeight; });
await page.waitForTimeout(300);
await page.screenshot({ path: `${out}/comp-edit-bottom.png` });
await page.keyboard.press('Escape');
await page.setViewportSize({ width: 420, height: 900 });
await page.waitForTimeout(300);
await page.screenshot({ path: `${out}/comp-phone.png`, fullPage: true });
await t.close();
