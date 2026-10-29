// Chat QoL: fixed-height workspace with auto-follow, stop, per-answer actions
// (regenerate, edit and resend, delete), request parameters, the ledger
// lookup that shows who served an answer, per-column include, saved system
// prompts, export, persistence across navigation and clear. Runs against the
// built web/dist with fixtures (see mockdash.mjs).
import { start, json, account, check, finish } from './mockdash.mjs';

const t = await start();
const models = [{ id: 'model-a', display: 'Model A' }, { id: 'model-b', display: 'Model B' }];
const sse = (text, done = true) => [
  'data: ' + JSON.stringify({ choices: [{ delta: { content: text } }] }), '',
  ...(done ? ['data: ' + JSON.stringify({ choices: [{ delta: {}, finish_reason: 'stop' }] }), '', 'data: [DONE]', ''] : []), '',
].join('\n');
const long = Array.from({ length: 30 }, (_, i) => `Line ${i + 1} of a long answer.`).join('\n\n');

const posts = [];            // every /admin/chat request: { body, headers }
const lookups = [];          // every /admin/requests query string
let mode = 'ok';             // ok | long | incomplete | slow
const page = await t.page(async (route, url) => {
  const req = route.request(); const p = url.pathname;
  if (p === '/admin/accounts') { await json(route, [account]); return true; }
  if (/\/models$/.test(p)) { await json(route, models); return true; }
  if (p === '/admin/requests') {
    lookups.push(url.search);
    await json(route, { rows: [{ ts: new Date().toISOString(), client: 'dashboard', surface: 'chat', alias: 'main', account: 'Mock provider', model: 'model-a', status: 200, ttft_ms: 300, total_ms: 900, tokens_in: 21, tokens_out: 312 }], summary: { count: 1 }, filter: {} });
    return true;
  }
  if (p === '/admin/chat') {
    const body = JSON.parse(req.postData() ?? '{}');
    posts.push({ body, headers: req.headers() });
    if (mode === 'slow') await new Promise((r) => setTimeout(r, 4000));
    const reply = mode === 'long' ? long : `Answer ${posts.length}`;
    await route.fulfill({ status: 200, contentType: 'text/event-stream', body: sse(reply, mode !== 'incomplete') });
    return true;
  }
  return false;
});
const composer = () => page.getByLabel('Message', { exact: true });
const send = async (text) => { await composer().fill(text); await composer().press('Enter'); };
const colText = (n) => page.getByRole('log', { name: `Conversation ${n}` }).innerText();

try {
  await page.setViewportSize({ width: 1400, height: 900 });
  await page.goto(`${t.base}/chat`);
  await page.getByRole('button', { name: 'Send to 1 model' }).waitFor();

  // ── layout: long answer must not push the composer off screen; stream is followed ──
  mode = 'long';
  await send('tell me a lot');
  await page.getByRole('log', { name: 'Conversation 1' }).getByText('Line 30 of a long answer.').waitFor();
  await page.waitForTimeout(500);
  const box = await composer().boundingBox();
  check('composer stays on screen after a long answer', box !== null && box.y + box.height <= 900, box);
  const near = await page.getByRole('log', { name: 'Conversation 1' }).evaluate((el) => el.scrollHeight - el.scrollTop - el.clientHeight < 60);
  check('the column follows the stream to the bottom', near);
  check('no "Jump to latest" while at the bottom', (await page.getByRole('button', { name: /Jump to latest/ }).count()) === 0);
  await page.getByRole('log', { name: 'Conversation 1' }).evaluate((el) => { el.scrollTop = 0; });
  await page.getByRole('button', { name: /Jump to latest/ }).waitFor();
  check('scrolling up offers "Jump to latest"', true);

  // ── served-by + stats from the ledger ──
  check('request is tagged with a session id', /^chat-/.test(posts[0].headers['x-ezllm-session'] ?? ''), posts[0].headers['x-ezllm-session']);
  await page.getByText(/via Mock provider/).waitFor({ timeout: 6000 });
  check('ledger lookup used the same session id', lookups.some((q) => q.includes(`session=${posts[0].headers['x-ezllm-session']}`)), lookups);
  check('served-by shows exact token counts', (await page.getByText(/↑21 ↓312 tokens/).count()) === 1);
  check('answer shows first-token and total time', (await page.getByText(/first token/).count()) === 1 && (await page.getByText(/^total /).count()) === 1);
  const roleSize = await page.getByText('Assistant', { exact: true }).first().evaluate((el) => parseFloat(getComputedStyle(el).fontSize));
  check('role labels meet the 11px minimum', roleSize >= 11, roleSize);

  // ── regenerate / edit / delete ──
  mode = 'ok';
  posts.length = 0;
  await send('second question');
  await page.getByText('Answer 1').waitFor();
  await page.getByRole('log', { name: 'Conversation 1' }).hover();
  await page.getByRole('button', { name: 'Regenerate' }).click();
  await page.getByText('Answer 2').waitFor();
  check('regenerate resends the same last message', posts.length === 2 && JSON.stringify(posts[0].body.messages.at(-1)) === JSON.stringify(posts[1].body.messages.at(-1)), posts.map((p) => p.body.messages.length));
  check('regenerate replaces the answer, not appends', !(await colText(1)).includes('Answer 1'));

  await page.getByRole('button', { name: 'Edit', exact: true }).click();
  await page.getByLabel('Edit message').fill('edited question');
  await page.getByRole('button', { name: 'Resend' }).click();
  await page.getByText('Answer 3').waitFor();
  const edited = posts.at(-1).body.messages;
  check('edit and resend sends the new text, without the old one', edited.at(-1).content === 'edited question' && !JSON.stringify(edited).includes('second question'), edited.map((m) => m.content));

  await page.getByRole('button', { name: 'Delete this message and its answer' }).last().click();
  check('deleting a message removes it with its answer', !(await colText(1)).includes('edited question') && !(await colText(1)).includes('Answer 3'));

  // ── incomplete answers stay out of history ──
  mode = 'incomplete';
  await send('will break');
  await page.getByText(/not sent as history/).waitFor();
  mode = 'ok';
  posts.length = 0;
  await send('after the break');
  await page.getByText(/^Answer \d+$/).last().waitFor();
  check('an incomplete answer is not sent back as history', posts[0].body.messages.filter((m) => m.role === 'assistant').every((m) => !/^Answer \d+$/.test(m.content)), posts[0]?.body.messages.map((m) => m.content.slice(0, 12)));

  // ── stop ──
  mode = 'slow';
  await send('slow one');
  await page.getByRole('button', { name: 'Stop', exact: true }).first().waitFor();
  await page.getByRole('button', { name: 'Stop', exact: true }).first().click();
  await page.getByText('Streaming…').waitFor({ state: 'detached', timeout: 3000 });
  check('Stop ends the stream and frees the composer', (await composer().isEnabled()));
  mode = 'ok';

  // ── parameters ──
  await page.getByRole('button', { name: 'Parameters', exact: true }).first().click();
  await page.getByLabel(/^Temperature/).first().fill('0.2');
  posts.length = 0;
  await send('with params');
  await page.getByText(/^Answer \d+$/).last().waitFor();
  check('a set parameter is sent', posts[0].body.temperature === 0.2 && !('top_p' in posts[0].body), posts[0].body);
  await page.getByLabel(/^Temperature/).first().fill('5');
  await composer().fill('x');
  check('an out-of-range parameter blocks sending and says why', await page.getByRole('button', { name: /^Send to/ }).isDisabled() && (await page.getByText(/must be between 0 and 2/).count()) >= 1);
  await page.getByRole('button', { name: 'Reset parameters' }).click();
  await composer().fill('');

  // ── ArrowUp recall ──
  await composer().press('ArrowUp');
  check('Up recalls the last message', (await composer().inputValue()) === 'with params');
  await composer().fill('');

  // ── columns: include toggle ──
  await page.getByRole('button', { name: /Add column/ }).click();
  await page.getByRole('switch', { name: 'Include column 2 in sends' }).click();
  posts.length = 0;
  await send('only first');
  await page.getByText(/^Answer \d+$/).last().waitFor();
  await page.waitForTimeout(300);
  check('an excluded column is skipped', posts.length === 1 && (await colText(2)) === '' || (await page.getByRole('log', { name: 'Conversation 2' }).getByText('only first').count()) === 0);
  check('button counts only included columns', (await page.getByRole('button', { name: 'Send to 1 model' }).count()) === 1);

  // ── persistence across navigation ──
  await page.getByRole('link', { name: 'Providers' }).first().click();
  await page.getByText(/providers?$/).first().waitFor().catch(() => {});
  await page.getByRole('link', { name: 'Chat' }).first().click();
  await page.getByRole('log', { name: 'Conversation 1' }).getByText('only first').waitFor();
  check('the conversation survives leaving the page', true);

  // ── saved system prompts ──
  await page.getByRole('button', { name: /^System prompt/ }).click();
  await page.getByLabel('System prompt', { exact: true }).fill('Answer like a pirate.');
  await page.getByLabel('Preset name').fill('Pirate');
  await page.getByRole('button', { name: 'Save prompt' }).click();
  await page.reload();
  await page.getByRole('button', { name: /^System prompt/ }).click();
  check('a saved prompt is offered after reload', (await page.getByLabel('Saved prompts').locator('option', { hasText: 'Pirate' }).count()) === 1);

  // ── export ──
  const dl = page.waitForEvent('download');
  await page.getByText('Export ▾').click();
  await page.getByRole('button', { name: 'Download Markdown' }).click();
  const file = await dl;
  const text = await (await import('node:fs/promises')).readFile(await file.path(), 'utf8');
  check('Markdown export has the conversation under its column', text.includes('## Column 1') && text.includes('only first'), text.slice(0, 120));

  // ── clear all (styled confirm) ──
  await page.getByRole('button', { name: 'Clear all' }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Clear', exact: true }).click();
  check('Clear all empties every column', (await page.getByText('No messages yet.').count()) === 2);
} catch (e) {
  check('driver ran to completion', false, String(e));
} finally {
  await t.close();
}
finish();
