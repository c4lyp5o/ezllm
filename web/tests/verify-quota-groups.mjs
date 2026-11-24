// A provider that limits its models in groups (Antigravity): the card, the Quota
// dialog and the warnings show the groups, not only their total.
import { mkdirSync } from 'node:fs';
import { start, json, check, finish, account } from './mockdash.mjs';

const shots = process.env.SHOTS_DIR;
if (shots) mkdirSync(shots, { recursive: true });
const soon = new Date(Date.now() + 6.2 * 86_400_000).toISOString();
const later = new Date(Date.now() + 7 * 86_400_000).toISOString();
const quota = (gemini, other) => ({
  kind: 'percent', percent_rolling: null, percent_weekly: Math.max(gemini, other), percent_monthly: null, balance: null, unit: null,
  resets_at: soon, read_at: new Date().toISOString(),
  groups: [{ name: 'Gemini Models', percent: gemini, resets_at: soon }, { name: 'Claude and GPT models', percent: other, resets_at: later }],
});
const t = await start();
try {
  for (const [name, gemini, other] of [['usual', 13.5, 0], ['full', 100, 4]]) {
    const acct = { ...account, name: 'Antigravity subscription', namespace: 'antigravity', base_url: 'http://127.0.0.1:8792/v1', quota: quota(gemini, other) };
    const page = await t.page((route, url) => {
      if (url.pathname === '/admin/accounts') return json(route, [acct]), true;
      if (url.pathname === '/admin/accounts/1/quota') return json(route, { kind: 'percent', raw: '{}', read_at: acct.quota.read_at }), true;
      if (url.pathname === '/admin/bridges') return json(route, []), true;
      return false;
    });
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.goto(`${t.base}/providers?open=1`);
    const gem = page.getByText('Gemini Models', { exact: true });
    await gem.first().waitFor();
    check(`${name}: both groups have a bar`, await page.getByText('Claude and GPT models', { exact: true }).count() >= 1);
    check(`${name}: the weekly total is not a third bar`, await page.getByText('weekly', { exact: true }).count() === 0);
    check(`${name}: the summary names both groups`, await page.getByTitle(`Gemini Models ${Math.round(gemini)}% · Claude and GPT models ${other}%`).count() === 1);
    check(`${name}: warning names the full group`, (await page.getByTitle('Antigravity subscription: Gemini Models quota at 100%').count() > 0) === (name === 'full'));
    check(`${name}: reset times are in the future`, /resets in \d+d/.test(await page.locator('body').innerText()) && !/resets -/.test(await page.locator('body').innerText()));
    await page.waitForTimeout(900);
    if (shots) await page.screenshot({ path: `${shots}/card-${name}.png` });
    await page.getByRole('button', { name: 'Quota', exact: true }).first().click();
    await page.getByRole('dialog').getByText('Claude and GPT models', { exact: true }).waitFor();
    check(`${name}: dialog has both groups`, await page.getByRole('dialog').getByText('Gemini Models', { exact: true }).count() === 1);
    await page.waitForTimeout(900);
    if (shots) await page.screenshot({ path: `${shots}/dialog-${name}.png` });
    await page.close();
  }
} finally {
  await finish(t);
}
