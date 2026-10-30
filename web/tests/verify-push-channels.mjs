// Exercises Settings → Notifications → Push channels against a mocked API.
import { start, json, check, finish } from './mockdash.mjs';

const shots = process.env.SHOTS || '/tmp';
const kinds = {
  ntfy: [{ name: 'server', secret: false, required: false }, { name: 'topic', secret: false, required: true }, { name: 'token', secret: true, required: false }],
  discord: [{ name: 'webhook_url', secret: true, required: true }],
  telegram: [{ name: 'chat_id', secret: false, required: true }, { name: 'bot_token', secret: true, required: true }],
  webhook: [{ name: 'url', secret: true, required: true }, { name: 'bearer', secret: true, required: false }],
};
let channels = [{ id: 7, kind: 'webhook', name: 'Ops hook', enabled: true, min_severity: 'warn', config: {}, secrets_set: ['bearer', 'url'],
  target: 'https://hooks.example.com', last_status: 'ok', last_sent_at: new Date(Date.now() - 300000).toISOString(), created_at: '', updated_at: '' }];
let nextID = 8;
const sent = { create: [], patch: [], test: [], del: [] };
let testFails = false;
const target = (c) => c.kind === 'ntfy' ? `${c.config.server || 'https://ntfy.sh'}/${c.config.topic}` : c.kind === 'telegram' ? `Telegram chat ${c.config.chat_id}` : c.target;

const dash = await start();
try {
  const page = await dash.page(async (route, url) => {
    const method = route.request().method();
    if (url.pathname === '/admin/notification-channels' && method === 'GET') { await json(route, { channels, kinds, max: 10 }); return true; }
    if (url.pathname === '/admin/notification-channels' && method === 'POST') {
      const b = route.request().postDataJSON(); sent.create.push(b);
      const c = { id: nextID++, kind: b.kind, name: b.name, enabled: b.enabled, min_severity: b.min_severity, config: b.config, secrets_set: Object.keys(b.secrets).sort(), last_status: '', last_sent_at: null, created_at: '', updated_at: '' };
      c.target = target(c); channels.push(c); await json(route, c, 201); return true;
    }
    if (url.pathname === '/admin/notification-channels/test') {
      sent.test.push(route.request().postDataJSON());
      await json(route, testFails ? { ok: false, error: 'the channel answered 502' } : { ok: true }); return true;
    }
    const m = url.pathname.match(/^\/admin\/notification-channels\/(\d+)$/);
    if (m) {
      const id = Number(m[1]);
      if (method === 'DELETE') { sent.del.push(id); channels = channels.filter((c) => c.id !== id); await route.fulfill({ status: 204 }); return true; }
      const b = route.request().postDataJSON(); sent.patch.push({ id, ...b });
      const c = channels.find((x) => x.id === id);
      Object.assign(c, { ...b, secrets: undefined });
      if (b.secrets) for (const [k, v] of Object.entries(b.secrets)) c.secrets_set = v ? [...new Set([...c.secrets_set, k])].sort() : c.secrets_set.filter((n) => n !== k);
      c.target = target(c); await json(route, c); return true;
    }
    if (url.pathname === '/admin/settings') { await json(route, { revision: 0, fields: {} }); return true; }
    return false;
  });
  const errors = []; page.on('pageerror', (e) => errors.push(e.message));
  await page.goto(dash.base + '/settings?tab=notifications');
  await page.getByText('Push channels', { exact: true }).waitFor();
  check('saved channel listed with its target and status', await page.getByText('https://hooks.example.com · warnings and errors').count() === 1 && await page.getByText(/^Delivered \d+m ago$/).count() === 1);

  // Add an ntfy channel: a malformed topic is refused at once, the missing name only on submit.
  await page.getByRole('button', { name: 'Add channel', exact: true }).click();
  const dialog = page.getByRole('dialog');
  await dialog.getByLabel('Topic').fill('bad topic!');
  check('malformed topic refused at the field', await dialog.getByText('Use 1 to 64 letters, digits, - or _.').count() === 1);
  check('untouched name not flagged yet', await dialog.getByText('Give the channel a name.').count() === 0);
  await dialog.getByLabel('Topic').press('Enter');
  check('Enter on an invalid form saves nothing', sent.create.length === 0 && await dialog.getByText('Give the channel a name.').count() === 1);
  await dialog.getByLabel('Name').fill('My phone');
  await dialog.getByLabel('Topic').fill('ezllm-alerts');
  await page.screenshot({ path: `${shots}/push-1-editor.png` });
  await dialog.getByRole('button', { name: 'Send test', exact: true }).click();
  await dialog.getByText(/Test sent\. Check ntfy/).waitFor();
  check('draft test sends the draft, unsaved', sent.test.length === 1 && sent.test[0].id === undefined && sent.test[0].config.topic === 'ezllm-alerts' && sent.create.length === 0);
  await dialog.getByRole('button', { name: 'Add channel', exact: true }).click();
  await page.getByText('Added My phone. Send a test to check it reaches you.').waitFor();
  await dialog.waitFor({ state: 'detached' });
  const created = sent.create[0];
  check('create body', created.kind === 'ntfy' && created.name === 'My phone' && created.min_severity === 'warn' && created.enabled === true &&
    JSON.stringify(created.config) === '{"topic":"ezllm-alerts"}' && JSON.stringify(created.secrets) === '{}', created);
  check('new channel listed', await page.getByText('https://ntfy.sh/ezllm-alerts · warnings and errors').count() === 1 && await page.getByText('Nothing sent yet').count() === 1);

  // Telegram: a bad bot token is named at the field, never echoed elsewhere.
  await page.getByRole('button', { name: 'Add channel', exact: true }).click();
  await dialog.getByRole('button', { name: 'Telegram', exact: true }).click();
  await dialog.getByLabel('Bot token').fill('nope');
  check('bad bot token refused at the field', await dialog.getByText('That is not a BotFather token (digits:letters).').count() === 1);
  await dialog.getByRole('button', { name: 'Cancel', exact: true }).click();
  const discard = page.getByRole('button', { name: /^Discard/ });
  if (await discard.count()) await discard.first().click();
  await dialog.waitFor({ state: 'detached' });

  // Edit the stored webhook: a blank secret keeps it; only changes are sent.
  const hookRow = page.locator('li', { hasText: 'Ops hook' });
  await hookRow.getByRole('button', { name: 'Edit', exact: true }).click();
  await dialog.getByPlaceholder('Stored — leave blank to keep it').first().waitFor();
  check('stored secret shown as kept', await dialog.getByPlaceholder('Stored — leave blank to keep it').count() === 2);
  await dialog.getByRole('button', { name: 'Errors only', exact: true }).click();
  check('severity readout follows the choice', await dialog.getByText('Errors only (a rollback, a failed backup).').count() === 1);
  await dialog.getByLabel(/Remove the stored bearer token/).check();
  await dialog.getByRole('button', { name: 'Save', exact: true }).click();
  await page.getByText('Saved Ops hook.').waitFor();
  await dialog.waitFor({ state: 'detached' });
  check('edit sends only what changed', JSON.stringify(sent.patch.at(-1)) === '{"id":7,"min_severity":"error","secrets":{"bearer":""}}', sent.patch.at(-1));

  // Row actions: a failing test says why; the toggle and delete do what they say.
  testFails = true;
  await hookRow.getByRole('button', { name: 'Send test', exact: true }).click();
  await page.getByText('The test to Ops hook did not arrive: the channel answered 502').waitFor();
  check('row test names the saved channel only', JSON.stringify(sent.test.at(-1)) === '{"id":7}');
  await hookRow.getByRole('switch').click();
  await page.getByText('Ops hook is off.').waitFor();
  check('toggle sends enabled only', JSON.stringify(sent.patch.at(-1)) === '{"id":7,"enabled":false}' && await hookRow.getByText('Off', { exact: true }).count() === 1);
  check('row switch reads off', await hookRow.getByRole('switch').getAttribute('aria-checked') === 'false');
  await page.waitForTimeout(800);
  await page.screenshot({ path: `${shots}/push-2-list.png`, fullPage: true });
  await hookRow.getByRole('button', { name: 'Delete', exact: true }).click();
  await page.getByRole('dialog').getByRole('button', { name: 'Delete', exact: true }).click();
  await page.getByText('Deleted Ops hook.').waitFor();
  check('delete removes the row', sent.del[0] === 7 && await page.locator('li', { hasText: 'Ops hook' }).count() === 0);

  await page.setViewportSize({ width: 390, height: 844 });
  check('mobile layout stays within viewport', await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
  const targetWidth = await page.locator('[data-target]').first().evaluate((el) => el.getBoundingClientRect().width);
  check('mobile rows stack: the target line keeps its width', targetWidth >= 250, targetWidth);
  await page.screenshot({ path: `${shots}/push-3-mobile.png`, fullPage: true });
  check('no browser exceptions', errors.length === 0, errors);
} finally { await dash.close(); }
finish();
