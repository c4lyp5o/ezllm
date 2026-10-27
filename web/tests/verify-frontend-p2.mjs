// Verify split SSE frames, compression failures, model picker semantics and reflow.
import { start, json, check, finish } from './mockdash.mjs';
const t = await start();
let usageFail = true, summaryFail = false;
const page = await t.page(async (route, url) => {
  if (url.pathname === '/admin/usage' && usageFail) { await json(route, { error: { message: 'unavailable' } }, 503); return true; }
  if (url.pathname === '/admin/requests' && summaryFail) { await json(route, { error: { message: 'unavailable' } }, 503); return true; }
  if (url.pathname === '/admin/compression-profiles') { await json(route, []); return true; }
  return false;
});
await page.addInitScript(() => {
  const original = window.fetch;
  window.fetch = (input, init) => {
    if (typeof input === 'string' && input === '/admin/chat') {
      const stream = new ReadableStream({ start(controller) { window.__stream = controller; } });
      return Promise.resolve(new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } }));
    }
    return original(input, init);
  };
});
try {
  await page.goto(`${t.base}/compression`);
  await page.getByText('Could not load compression savings.').waitFor();
  check('account report failure is not reported as zero', await page.getByText('No savings recorded in the last 24h.').count() === 0);
  usageFail = false; summaryFail = true;
  await Promise.all([page.waitForResponse(r => r.url().includes('/admin/requests') && r.status() === 503), page.getByRole('button', { name: 'Retry savings' }).click()]);
  await page.getByText('Could not load compression savings.').waitFor();
  check('summary failure is not reported as zero', await page.getByText('No savings recorded in the last 24h.').count() === 0);
  summaryFail = false;
  await page.getByRole('button', { name: 'Retry savings' }).click();
  await page.getByText('No savings recorded in the last 24h.').waitFor();
  check('savings recovers after retry', await page.getByText('Could not load compression savings.').count() === 0);

  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`${t.base}/chat`);
  const picker = page.getByRole('combobox', { name: 'Model for column 1' });
  await picker.waitFor(); await picker.focus();
  const firstActive = await picker.getAttribute('aria-activedescendant');
  check('active model is exposed to assistive technology', !!firstActive && await page.locator(`[id="${firstActive}"]`).getAttribute('role') === 'option');
  await picker.press('ArrowDown');
  const nextActive = await picker.getAttribute('aria-activedescendant');
  check('arrow navigation updates active option', !!nextActive && nextActive !== firstActive);
  await picker.press('Enter');
  check('enter selects the highlighted model', (await picker.inputValue()).includes('Model B'));
  await page.getByRole('button', { name: '＋ Add column' }).click();
  await page.getByRole('button', { name: '＋ Add column' }).click();
  const rects = await page.locator('section').filter({ has: page.getByRole('combobox') }).evaluateAll(els => els.map(e => ({ x: e.getBoundingClientRect().x, y: e.getBoundingClientRect().y, w: e.getBoundingClientRect().width })));
  check('three chat columns stack at phone width', rects.length === 3 && rects[1].y > rects[0].y && rects[2].y > rects[1].y && rects.every(r => r.w > 250), rects);
  await page.getByRole('button', { name: 'Remove column' }).last().click();
  await page.getByRole('button', { name: 'Remove column' }).last().click();
  const input = page.getByRole('textbox', { name: 'Message' });
  await input.fill('hello'); await input.press('Enter');
  await page.waitForFunction(() => !!window.__stream);
  const frame = JSON.stringify({ choices: [{ delta: { content: 'INCREMENTAL' }, finish_reason: null }] });
  await page.evaluate((payload) => {
    const enc = new TextEncoder();
    for (const chunk of [`data: ${payload}\r`, '\n\r', '\n']) window.__stream.enqueue(enc.encode(chunk));
  }, frame);
  await page.getByText('INCREMENTAL').waitFor({ timeout: 3000 });
  check('split CRLF frame appears before stream ends', await page.getByText('Streaming…').count() > 0);
  await page.evaluate(() => { const e = new TextEncoder(); window.__stream.enqueue(e.encode('data: [DONE]\r\n\r\n')); window.__stream.close(); });
  await page.getByText('Streaming…').waitFor({ state: 'hidden' });
  check('split stream completes without an error', await page.getByText(/ended before/).count() === 0);
} finally { await t.close(); }
finish();
