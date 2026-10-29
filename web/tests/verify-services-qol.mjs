// Services QoL: summary + filters, failing-first order, verified models in the
// picker, Auto "test the pick", unsaved/discard, styled confirms (no native
// dialogs), bulk "test all", deep link, and parallel catalog loading with a
// broken provider. Runs against the built web/dist with fixtures (mockdash.mjs).
import { start, json, account, check, finish } from './mockdash.mjs';

const t = await start();
const now = Date.now();
const iso = (msAgo) => new Date(now - msAgo).toISOString();
const card = (surface, endpoint, label, o = {}) => ({
  surface, endpoint, label, description: `${label} service.`, mode: 'off', model_target: '', resolved_target: '',
  target_exists: true, verified_models: 0, accounts: 2, verdict: null, probes: [], ...o,
});
const cards = [
  card('embeddings', '/v1/embeddings', 'Embeddings', {
    mode: 'manual', model_target: 'mock/model-a', resolved_target: 'mock/model-a',
    verdict: { model: 'mock/model-a', ok: true, detail: '', tested_at: iso(5400e3) },
    probes: [{ model: 'mock/model-a', ok: true, detail: '', tested_at: iso(5400e3) }, { model: 'mock/model-b', ok: false, detail: 'HTTP 500', tested_at: iso(9000e3) }],
  }),
  card('images_gen', '/v1/images/generations', 'Image generation', {
    mode: 'auto', verified_models: 1, resolved_target: 'mock/model-b',
    verdict: { model: 'mock/model-b', ok: false, detail: 'HTTP 500: busy', tested_at: iso(600e3) },
    probes: [{ model: 'mock/model-b', ok: true, detail: '', tested_at: iso(7200e3) }],
  }),
  card('audio_tts', '/v1/audio/speech', 'Text to speech'),
  card('rerank', '/v1/rerank', 'Rerank', { mode: 'manual', model_target: 'mock/model-b', resolved_target: 'mock/model-b' }),
  card('audio_stt', '/v1/audio/transcriptions', 'Speech to text', { accounts: 0 }),
];
const models = [{ id: 'model-a', display: 'Model A', chat: true }, { id: 'model-b', display: 'Model B', chat: true }];
const second = { ...account, id: 2, name: 'Broken', namespace: 'broken' };
const calls = [];
const page = await t.page(async (route, url) => {
  const req = route.request(); const p = url.pathname; const m = req.method();
  if (m !== 'GET') calls.push(`${m} ${p} ${req.postData() ?? ''}`);
  if (p === '/admin/services' && m === 'GET') { await json(route, { services: cards }); return true; }
  if (p === '/admin/accounts') { await json(route, [account, second]); return true; }
  if (p === '/admin/accounts/1/models') { await json(route, models); return true; }
  if (p === '/admin/accounts/2/models') { await json(route, { error: { message: 'down' } }, 500); return true; }
  if (/\/admin\/services\/[a-z_]+\/test$/.test(p)) {
    const body = JSON.parse(req.postData() ?? '{}');
    await json(route, { surface: p.split('/')[3], model: body.model, ok: true, dispatched: true, status: 200, ms: 12, detail: '' });
    return true;
  }
  if (/\/admin\/services\/[a-z_]+$/.test(p) && m === 'PUT') { await json(route, { ...cards[2], mode: 'manual' }); return true; }
  return false;
});
page.on('dialog', async (d) => { check('no native browser dialog is used', false, d.message()); await d.dismiss(); });
const section = (label) => page.locator('section', { has: page.getByRole('heading', { name: label }) });

try {
  await page.goto(`${t.base}/services`);
  await page.getByText(/endpoints have a default/).waitFor();
  check('summary states how many endpoints have a default', (await page.getByText(/3 of 5 endpoints have a default/).count()) === 1);
  check('summary names failing and untested counts', (await page.getByText(/1 failing/).count()) === 1 && (await page.getByText(/1 untested/).count()) === 1);
  const titles = await page.locator('section h2').allInnerTexts();
  check('cards sort failing, untested, working, off', titles[0] === 'Image generation' && titles[1] === 'Rerank' && titles[2] === 'Embeddings' && titles[titles.length - 1] === 'Speech to text', titles);
  await page.getByText(/1 provider could not list models/).waitFor();
  check('a broken provider does not block the others', true);

  // filters
  await page.getByRole('button', { name: /^Working 1/ }).click();
  check('Working filter shows only working cards', (await page.locator('section h2').count()) === 1);
  await page.getByRole('button', { name: /^Needs attention 2/ }).click();
  check('Needs attention shows failing and untested', (await page.locator('section h2').count()) === 2);
  await page.getByRole('button', { name: /^All 5/ }).click();

  // picker marks verified models and lists them first
  const emb = section('Embeddings');
  await emb.getByLabel(/Default model for/).click();
  const first = await page.getByRole('listbox').getByRole('option').first().innerText();
  check('picker leads with verified models and says so', /Model A.*verified/.test(first) && (await page.getByText(/last test failed/).count()) >= 1, first);
  await page.keyboard.press('Escape');

  // Auto: test the pick (paid -> styled confirm first)
  const img = section('Image generation');
  check('auto lists its verified models', (await img.getByText('Show verified models').count()) === 1);
  calls.length = 0;
  await img.getByRole('button', { name: 'Test the pick' }).click();
  await page.getByRole('dialog').getByText(/Test mock\/model-b\?/).waitFor();
  check('paid test asks in a styled dialog, not window.confirm', calls.length === 0);
  await page.getByRole('dialog').getByRole('button', { name: 'Send test' }).click();
  await img.getByText('Endpoint test succeeded').waitFor();
  check('the pick is the model tested', calls.some((c) => /^POST \/admin\/services\/images_gen\/test .*"model":"mock\/model-b"/.test(c)), calls);
  check('the outcome stays on the card', (await img.getByText('Endpoint test succeeded').count()) === 1);

  // unsaved / discard / save
  const tts = section('Text to speech');
  await tts.getByLabel('Default behavior').selectOption('manual');
  check('a changed form shows Unsaved', (await tts.getByText(/Unsaved/).count()) === 1);
  await tts.getByRole('button', { name: 'discard' }).click();
  check('Discard restores the saved state', (await tts.getByLabel('Default behavior').inputValue()) === 'off' && (await tts.getByText(/Unsaved/).count()) === 0);

  // bulk
  calls.length = 0;
  check('Test all counts only non-paid configured services', (await page.getByRole('button', { name: /Test all configured \(2\)/ }).count()) === 1);
  await page.getByRole('button', { name: /Test all configured/ }).click();
  await page.getByText(/2 passed · 0 failed · 1 paid skipped/).waitFor({ timeout: 8000 });
  const tests = calls.filter((c) => /\/test /.test(c));
  check('bulk tests each configured service in turn, skipping paid', tests.length === 2 && tests.every((c) => !/images_gen/.test(c)), tests);
  await page.getByRole('switch', { name: 'include paid tests' }).click();
  await page.getByRole('button', { name: /Test all configured \(3\)/ }).click();
  await page.getByRole('dialog').getByText(/Image generation will send real generation requests/).waitFor();
  check('including paid tests asks first, naming them', true);
  await page.getByRole('dialog').getByRole('button', { name: 'Cancel' }).click();

  // Try it: styled confirm
  await img.getByRole('button', { name: /Try it/ }).click();
  await img.getByLabel('Prompt').fill('a cat');
  await img.getByRole('button', { name: 'Generate' }).click();
  await page.getByRole('dialog').getByText(/Send a real request to mock\/model-b\?/).waitFor();
  check('Try it confirms in a styled dialog', true);
  await page.getByRole('dialog').getByRole('button', { name: 'Cancel' }).click();

  // deep link
  await page.goto(`${t.base}/services?service=rerank`);
  await page.getByText(/endpoints have a default/).waitFor();
  await page.waitForTimeout(500);
  check('deep link highlights the card', /border-accent/.test((await section('Rerank').getAttribute('class')) ?? ''));
  check('deep link is consumed from the URL', !/service=/.test(page.url()), page.url());
} catch (e) {
  check('driver ran to completion', false, String(e));
} finally {
  await t.close();
}
finish();
