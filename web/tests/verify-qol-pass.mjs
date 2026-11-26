// QoL pass in the browser: parked pills with Unpark on Providers and Combos,
// and the per-key compression profile picker on Connect.
import { start, json, account, check, finish } from './mockdash.mjs';

const shots = process.env.SHOTS_DIR;
const t = await start();
const until = new Date(Date.now() + 90_000).toISOString();
let parks = [{ namespace: 'mock', model: 'model-a', until, strikes: 0, probe_active: false, reason: '429 rate limited' }];
const deletes = [];
const patches = [];
const explains = [];
const page = await t.page(async (route, url) => {
  const req = route.request();
  const p = url.pathname;
  if (p === '/admin/cooldowns' && req.method() === 'DELETE') {
    deletes.push(url.search);
    const before = parks.length;
    parks = parks.filter((c) => c.namespace !== url.searchParams.get('namespace') || (url.searchParams.get('model') && c.model !== url.searchParams.get('model')));
    await json(route, { cleared: before - parks.length });
    return true;
  }
  if (p === '/admin/cooldowns') { await json(route, { cooldowns: parks, now: new Date().toISOString() }); return true; }
  if (p === '/admin/combos') {
    await json(route, [{ id: 1, name: 'daily', strategy: 'failover', enabled: true, context_size: 200000, compression_profile_id: null,
      hops: [{ account_id: 1, model_id: 'model-a', weight: 1, enabled: true }, { account_id: 1, model_id: 'model-b', weight: 1, enabled: true }] }]);
    return true;
  }
  if (p === '/admin/route-explain') {
    explains.push(req.postDataJSON());
    await json(route, { requested: 'daily', kind: 'combo', strategy: 'failover', surface: 'chat', usable: 1, error: '',
      hops: [
        { account_id: 1, namespace: 'mock', model: 'model-a', status: 'skipped', reason: 'parked: 429 rate limited', parked_until: until },
        { account_id: 1, namespace: 'mock', model: 'model-b', status: 'candidate', wire: 'chat' },
      ] });
    return true;
  }
  if (p === '/admin/compression-profiles') { await json(route, [{ id: 3, name: 'lean', enabled: true }]); return true; }
  if (p === '/admin/tokens' && req.method() === 'GET') {
    await json(route, [{ id: 7, name: 'my-agent', hint: 'ezllm…a91f', roles: ['infer'], enabled: true, cap_window: 'monthly', cap_tokens: 0, cap_usd: 0,
      allowed_models: '', expires_at: '', last_used_at: null, created_at: '2026-11-01T00:00:00Z', compression_profile_id: null }]);
    return true;
  }
  if (p === '/admin/tokens/7' && req.method() === 'PATCH') {
    patches.push(req.postDataJSON());
    await json(route, { id: 7, name: 'my-agent', hint: 'ezllm…a91f', roles: ['infer'], enabled: true, cap_window: 'monthly', cap_tokens: 0, cap_usd: 0,
      allowed_models: '', expires_at: '', created_at: '2026-11-01T00:00:00Z', compression_profile_id: 3 });
    return true;
  }
  if (p === '/admin/accounts') { await json(route, [account]); return true; }
  return false;
});
try {
  await page.goto(`${t.base}/providers`);
  const pill = page.getByText(/parked · 1m \d+s/).first();
  await pill.waitFor();
  check('provider card shows the park with a countdown', await pill.count() === 1);
  if (shots) { await page.waitForTimeout(1200); await page.screenshot({ path: `${shots}/providers-parked.png`, clip: { x: 240, y: 170, width: 1200, height: 130 } }); }
  await page.getByRole('button', { name: 'Unpark' }).first().click();
  await page.waitForTimeout(300);
  check('Unpark clears the account', deletes.at(-1) === '?namespace=mock', deletes);
  check('the pill goes away once nothing is parked', await page.getByText(/parked · /).count() === 0);

  parks = [{ namespace: 'mock', model: 'model-a', until, strikes: 0, probe_active: false, reason: '5xx' }];
  await page.goto(`${t.base}/combos`);
  await page.getByText(/parked · 1m/).first().waitFor();
  check('only the parked hop carries a pill', await page.getByText(/parked · /).count() === 1);
  if (shots) await page.screenshot({ path: `${shots}/combos-parked.png`, fullPage: false });
  await page.getByRole('button', { name: 'Unpark' }).first().click();
  await page.waitForTimeout(300);
  check('Unpark on a hop clears just that model', deletes.at(-1) === '?namespace=mock&model=model-a', deletes);

  await page.getByRole('button', { name: 'Explain' }).click();
  await page.getByText('1 hop would be tried.').waitFor();
  check('explain lists both hops with their verdicts', await page.getByText('would be tried', { exact: true }).count() === 1 && await page.getByText('parked: 429 rate limited').count() === 1);
  check('explain asked about the combo on the chat API', explains.at(-1)?.model === 'daily' && explains.at(-1)?.surface === 'chat', explains);
  if (shots) { await page.waitForTimeout(1000); await page.screenshot({ path: `${shots}/combos-explain.png`, fullPage: false }); }
  await page.getByRole('button', { name: 'Anthropic' }).click();
  await page.waitForTimeout(300);
  check('switching the API asks again', explains.at(-1)?.surface === 'anthropic', explains);
  await page.keyboard.press('Escape');

  await page.goto(`${t.base}/connect?policy=7`);
  const picker = page.getByLabel('Compression profile');
  await picker.waitFor();
  await picker.selectOption('3');
  if (shots) await page.screenshot({ path: `${shots}/connect-policy.png`, fullPage: false });
  await page.getByRole('button', { name: 'Save' }).click();
  await page.waitForTimeout(300);
  check("saving the policy sends the key's profile", patches.at(-1)?.compression_profile_id === 3, patches);
} finally { await t.close(); }
finish();
