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
await pg.goto('http://127.0.0.1:20199/stats');
await pg.waitForTimeout(1400);
const feed = pg.locator('text=RECENT REQUESTS').first();
const section = feed.locator('xpath=ancestor::section[1]');
if (await section.count()) {
  await section.screenshot({ path: shots + '/feed-table.png' });
  console.log('SECTION_OK');
} else {
  await pg.screenshot({ path: shots + '/feed-table.png', fullPage: true });
  console.log('FALLBACK_FULLPAGE');
}
await pg.close(); await b.close();
