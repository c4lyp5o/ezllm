import { chromium } from 'playwright';
const BASE = process.env.RIG_BASE ?? 'http://127.0.0.1:20198';
const b = await chromium.launch();
const p = await b.newPage({ viewport: { width: 1280, height: 900 } });
const errs = [];
p.on('console', (m) => { if (m.type() === 'error') errs.push(m.text()); });
p.on('pageerror', (e) => errs.push('pageerror: ' + e.message));

// unlock the dashboard (no data renders before this, by design)
await p.goto(BASE + '/', { waitUntil: 'networkidle' });
const PASS = process.env.RIG_PASS;
if (!PASS) throw new Error('set RIG_PASS (never hardcode a dashboard password in a test)');
await p.fill('input[type=password]', PASS);
await p.keyboard.press('Enter');
await p.waitForTimeout(1500);

await p.goto(BASE + '/providers', { waitUntil: 'networkidle' });
await p.waitForTimeout(1200);
const cards = await p.locator('body').innerText();
const hasCopilotCLI = cards.includes('GitHub Copilot (bridge)');
const hasOldCopilot = /GitHub Copilot(?! \(CLI\))/.test(cards);
await p.screenshot({ path: '/tmp/providers.png' });

// open the add-provider modal and switch to the CLI preset
const add = p.getByRole('button', { name: /add provider/i }).first();
await add.click();
await p.waitForTimeout(600);
const opts = await p.locator('select').first().locator('option').allTextContents();
const hasCLIOption = opts.some((o) => o.includes('GitHub Copilot (bridge)'));
const hasOldOption = opts.some((o) => o.trim() === 'GitHub Copilot' || o.includes('GitHub Copilot') && !o.includes('(CLI)'));
if (hasCLIOption) {
  await p.locator('select').first().selectOption({ label: opts.find((o) => o.includes('GitHub Copilot (bridge)')) });
  await p.waitForTimeout(500);
}
const modalText = await p.locator('body').innerText();
const deviceFlowGone = !/device flow|Start GitHub login|Waiting for authorization/i.test(modalText);
await p.screenshot({ path: '/tmp/providers-modal.png' });

console.log(JSON.stringify({ hasCopilotCLI, hasOldCopilot, hasCLIOption, hasOldOption, deviceFlowGone, errs }, null, 1));
await b.close();
