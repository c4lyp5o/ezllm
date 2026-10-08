// shot-providers.mjs — verify the Providers dialog: Claude Platform preset present, "(custom URL)" gone,
// anthropic base-URL placeholder is host-only. Screenshots both states.
// Auth: injects the admin token into localStorage before page scripts run (SPA reads TOKEN_KEY 'ezllm.admin.token').
import { chromium } from 'playwright';
import { readFileSync, mkdirSync } from 'node:fs';

const TOKEN = readFileSync('/home/calypso/.hermes/secrets/ezllm-admin.token', 'utf8').trim();
const shots = '/home/calypso/.hermes/cache/scratch/ezllm-verify/shots';
mkdirSync(shots, { recursive: true });

const b = await chromium.launch();
const ctx = await b.newContext({ viewport: { width: 1440, height: 1100 } });
await ctx.addInitScript((t) => {
  try { localStorage.setItem('ezllm.admin.token', t); } catch (e) { /* about:blank has no storage */ }
}, TOKEN);

const pg = await ctx.newPage();
await pg.goto('http://127.0.0.1:20129/providers');
await pg.waitForTimeout(2600);

// open the "Add provider" dialog
const clicked = await pg.evaluate(() => {
  const btn = [...document.querySelectorAll('button')].find((x) => /add provider/i.test(x.textContent || ''));
  if (btn) { btn.click(); return btn.textContent.trim(); }
  return null;
});
console.log('CLICKED=' + JSON.stringify(clicked));
await pg.waitForTimeout(700);

// provider-type options: order + copy
const labels = await pg.evaluate(() => [...document.querySelectorAll('#ap-kind option')].map((o) => o.textContent));
console.log('OPTIONS=' + JSON.stringify(labels));
console.log('HAS_CLAUDE_FIRST=' + (labels[0] === 'Claude Platform'));
console.log('HAS_CUSTOM_URL=' + labels.some((l) => /custom/i.test(l)));

// select "Claude Platform" — base prefilled with the clean host
await pg.selectOption('#ap-kind', 'claude-platform');
await pg.waitForTimeout(250);
const a = await pg.evaluate(() => ({
  sel: document.querySelector('#ap-kind')?.value,
  base: document.querySelector('#ap-url')?.value,
  ph: document.querySelector('#ap-url')?.placeholder,
  helper: document.body.innerText.includes('Enter the host only'),
}));
console.log('CLAUDE=' + JSON.stringify(a));
await pg.screenshot({ path: `${shots}/providers-claude.png` });

// select generic "anthropic-compatible" — empty field shows the host-only placeholder
await pg.selectOption('#ap-kind', 'anthropic-compatible');
await pg.waitForTimeout(250);
const c = await pg.evaluate(() => ({
  base: document.querySelector('#ap-url')?.value,
  ph: document.querySelector('#ap-url')?.placeholder,
  helper: document.body.innerText.includes('Enter the host only'),
}));
console.log('ANTH_PLACEHOLDER=' + JSON.stringify(c));
await pg.screenshot({ path: `${shots}/providers-anthropic-ph.png` });

console.log('SHOTS=' + shots);
await b.close();