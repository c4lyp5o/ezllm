import { chromium } from 'playwright';
import { readFileSync } from 'node:fs';
const TOKEN = readFileSync('/home/calypso/.hermes/secrets/ezllm-admin.token', 'utf8').trim();
const shots = '/home/calypso/.hermes/cache/scratch/ezllm-verify/shots';
const b = await chromium.launch();
const pg = await (await b.newContext({ viewport: { width: 1400, height: 1000 } })).newPage();
await pg.goto('http://127.0.0.1:20199/');
await pg.waitForTimeout(600);
const inp = pg.locator('input[type=password], input[placeholder*=token i]');
if (await inp.count()) { await inp.first().fill(TOKEN); await pg.keyboard.press('Enter'); await pg.waitForTimeout(900); }
await pg.goto('http://127.0.0.1:20199/rules');
await pg.waitForTimeout(1200);
await pg.screenshot({ path: shots + '/rules.png', fullPage: true });
// open the editor modal for the second shot (edit the closed day rule)
const edit = pg.locator('button:has-text("edit")').first();
if (await edit.count()) {
  await edit.click();
  await pg.waitForTimeout(700);
  await pg.screenshot({ path: shots + '/rules-modal.png' });
}
await pg.close(); await b.close();
console.log('RULES_SHOTS_OK');
