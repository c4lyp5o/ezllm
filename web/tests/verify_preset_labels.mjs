// Verify the renamed provider presets: the dropdown text, and that the
// auto-seeded account name drops the "(bridge)" suffix so cards stay clean.
import { chromium } from 'playwright';

const BASE = process.env.RIG_BASE ?? 'http://127.0.0.1:20198';
const PASS = process.env.RIG_PASS;
if (!PASS) throw new Error('set RIG_PASS');

const b = await chromium.launch();
const p = await b.newPage({ viewport: { width: 1280, height: 1000 } });
const errs = [];
p.on('console', (m) => { if (m.type() === 'error') errs.push(m.text()); });
p.on('pageerror', (e) => errs.push('pageerror: ' + e.message));

await p.goto(BASE + '/', { waitUntil: 'networkidle' });
await p.fill('input[type=password]', PASS);
await p.keyboard.press('Enter');
await p.waitForTimeout(1500);
await p.goto(BASE + '/providers', { waitUntil: 'networkidle' });
await p.waitForTimeout(700);
await p.getByRole('button', { name: /add provider/i }).first().click();
await p.waitForTimeout(500);

const select = p.locator('select').first();
const options = await select.locator('option').allTextContents();
const wanted = [
  'Claude Pro/Max (bridge)', 'ChatGPT Plus/Pro (bridge)', 'Google Antigravity (bridge)',
  'GitHub Copilot (bridge)', 'Claude.ai (web)', 'Google Gemini (web)',
];
const gone = ['Claude.ai (web session)', 'Google Gemini (web cookies)', 'GitHub Copilot (CLI)'];

// Seeding only fires on a BLANK name, so each preset needs its own fresh modal.
const seeded = {};
for (const label of wanted) {
  if (!options.includes(label)) { seeded[label] = 'OPTION MISSING'; continue; }
  if (await p.locator('[role=dialog]').count()) {
    await p.keyboard.press('Escape');
    await p.waitForTimeout(300);
    if (await p.getByRole('button', { name: /discard|leave/i }).count()) {
      await p.getByRole('button', { name: /discard|leave/i }).first().click();
      await p.waitForTimeout(300);
    }
  }
  await p.getByRole('button', { name: /add provider/i }).first().click();
  await p.waitForTimeout(400);
  const s2 = p.locator('select').first();
  await s2.selectOption({ label });
  await p.waitForTimeout(250);
  seeded[label] = await p.locator('#ap-name').inputValue().catch(() => '(no #ap-name)');
}

// screenshot with the copilot preset selected, dropdown open for the label list
await select.selectOption({ label: 'GitHub Copilot (bridge)' });
await p.waitForTimeout(300);
await p.screenshot({ path: '/tmp/providers-labels.png' });

const report = {
  totalOptions: options.length,
  present: wanted.filter((w) => options.includes(w)),
  missing: wanted.filter((w) => !options.includes(w)),
  staleStillThere: gone.filter((g) => options.includes(g)),
  seededNames: seeded,
  errs,
};
console.log(JSON.stringify(report, null, 1));
await b.close();
