import { start, check, finish } from './mockdash.mjs';
const rig = await start();
try {
  const page = await rig.page();
  await page.goto(rig.base + '/providers');
  await page.getByRole('button', { name: /add provider/i }).first().click();
  const select = page.locator('select').filter({ has: page.locator('option[value="meta-api"]') });
  await select.selectOption('meta-api');
  check('Meta API selected', await select.inputValue() === 'meta-api');
  check('Meta credential guidance', await page.getByText('Meta Model API key (MODEL_API_KEY)', { exact: true }).count() > 0);
  await page.locator('#ap-name').fill('Meta test');
  await page.locator('#ap-key').fill('fixture-key');
  const request = page.waitForRequest(r => r.url().endsWith('/admin/accounts') && r.method() === 'POST');
  await page.getByRole('button', { name: 'Save & validate key', exact: true }).click();
  const payload = (await request).postDataJSON();
  check('Meta endpoint submitted', payload.base_url === 'https://api.meta.ai/v1');
  check('Compatible adapter submitted', payload.kind === 'openai-compatible');
} finally { await rig.close(); }
finish();
