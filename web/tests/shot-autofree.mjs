// Drives the auto-free combo editor against a throwaway instance (BASE) with
// the factory dashboard password. Not wired into CI; a manual proof like its
// shot-* siblings.
import { chromium } from 'playwright';
const BASE = process.env.BASE ?? 'http://127.0.0.1:20311';
const shots = process.env.SHOTS ?? '.';
const b = await chromium.launch();
const ctx = await b.newContext({ viewport: { width: 1280, height: 1000 } });
const login = await ctx.request.post(`${BASE}/admin/login`, { data: { password: 'STRONGPASSWORD' } });
if (!login.ok()) throw new Error(`login ${login.status()}`);
const { token } = await login.json();
await ctx.addInitScript((t) => {
  localStorage.setItem('ezllm.admin.token', t);
  sessionStorage.setItem('ezllm.admin.unlocked', '1');
}, token);
const pg = await ctx.newPage();
const errs = [];
pg.on('pageerror', (e) => errs.push(String(e)));
await pg.goto(`${BASE}/combos`);
await pg.waitForSelector('text=ezllm-auto-free');
await pg.screenshot({ path: `${shots}/af-1-seeded-empty.png` });
await pg.locator('text=No free models yet').first().waitFor();

await pg.getByRole('button', { name: 'Edit' }).first().click();
await pg.getByRole('switch', { name: 'fill with free models' }).waitFor();
const on = await pg.getByRole('switch', { name: 'fill with free models' }).getAttribute('aria-checked');
if (on !== 'true') throw new Error('switch should be on for the seeded combo');
await pg.getByRole('button', { name: 'Openrouter' }).click();
await pg.getByLabel('Provider', { exact: true }).selectOption({ label: 'Ollama Cloud' });
await pg.getByLabel('Model', { exact: true }).selectOption('beam');
await pg.getByRole('button', { name: 'Add a model' }).click();
await pg.screenshot({ path: `${shots}/af-2-editor.png` });
await pg.getByRole('button', { name: 'Save changes' }).click();
await pg.waitForSelector('text=ezllm-auto-free');
await pg.locator('text=free models from').first().waitFor();
await pg.screenshot({ path: `${shots}/af-3-filled.png` });
console.log(await pg.locator('text=free model').first().innerText());
if (errs.length) throw new Error(errs.join('\n'));
await b.close();
console.log('AUTOFREE_OK');
