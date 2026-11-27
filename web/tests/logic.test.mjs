// Pure dashboard logic, checked without a browser: esbuild bundles the
// modules for node, then plain asserts run against them.
import { build } from 'esbuild';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { strict as assert } from 'node:assert';
import { fileURLToPath, pathToFileURL } from 'node:url';

const src = fileURLToPath(new URL('../src/', import.meta.url));
const dir = await mkdtemp(join(tmpdir(), 'ezllm-logic-'));
try {
  const entry = join(dir, 'entry.ts');
  await writeFile(entry, `
    export { safeHref } from ${JSON.stringify(join(src, 'utils/lib.ts'))};
    export { globMatch, keyState } from ${JSON.stringify(join(src, 'utils/connect.ts'))};
    export { buildBuildings, niceCeil, scaleCeiling, rulerTicks, share, floorsOf, windowMix, planRows, placeSigns, signLine, MAX_FLOORS } from ${JSON.stringify(join(src, 'utils/city.ts'))};
    export { looksBinary, fenceFor } from ${JSON.stringify(join(src, 'utils/chatlog.ts'))};
    export { effectiveText, settingError } from ${JSON.stringify(join(src, 'utils/settings.ts'))};
    export { summarizeImport, importWrites } from ${JSON.stringify(join(src, 'utils/import.ts'))};
    export { startProgress, defaultTestModel, freeNamespace, isAppKey, START_PRESETS, SUB_PRESETS, bridgeState } from ${JSON.stringify(join(src, 'utils/start.ts'))};
    export { PROVIDER_PRESETS, modelStatus } from ${JSON.stringify(join(src, 'utils/providers.ts'))};
    export { plainReason, apiLabel } from ${JSON.stringify(join(src, 'utils/words.ts'))};
    export { extraStates, EXTRAS } from ${JSON.stringify(join(src, 'utils/extras.ts'))};
    export { parkedFor, secondsLeft, splitParks } from ${JSON.stringify(join(src, 'utils/cooldowns.ts'))};
    export { csvField, toCSV } from ${JSON.stringify(join(src, 'utils/export.ts'))};
    export { filterPalette, PALETTE_ITEMS } from ${JSON.stringify(join(src, 'components/CommandPalette.tsx'))};
  `);
  const out = join(dir, 'bundle.mjs');
  await build({ entryPoints: [entry], bundle: true, format: 'esm', platform: 'node', outfile: out, logLevel: 'error', jsx: 'automatic' });
  globalThis.window = { location: { href: 'http://localhost/' } };
  const m = await import(pathToFileURL(out).href);

  // Markdown links: only http(s) and mailto survive.
  assert.equal(m.safeHref('https://example.com/a'), 'https://example.com/a');
  assert.equal(m.safeHref('mailto:you@example.com'), 'mailto:you@example.com');
  assert.equal(m.safeHref('javascript:alert(1)'), '#');
  assert.equal(m.safeHref('JaVaScRiPt:alert(1)'), '#');
  assert.equal(m.safeHref('data:text/html,<script>x</script>'), '#');

  // Allowlist globs follow the server: a trailing * crosses the slash.
  assert.ok(m.globMatch('openrouter/*', 'openrouter/google/gemini'));
  assert.ok(m.globMatch('gpt-5*', 'gpt-5-mini'));
  assert.ok(!m.globMatch('deepseek/*', 'openrouter/deepseek'));

  // A key at its spend cap reads as spent.
  const key = { enabled: true, expires_at: '', cap_tokens: 0, cap_usd: 10, spent_usd: 10 };
  assert.equal(m.keyState(key, 0).key, 'spent');
  assert.equal(m.keyState({ ...key, spent_usd: 2 }, 0).key, 'active');

  // Buildings: totals from the account row; a shared key hint never doubles.
  const acct = (id, ns, hint) => ({ id, name: ns, namespace: ns, kind: 'openai-compatible', enabled: true,
    keys: [{ id: id * 10, label: '', hint, enabled: true }, { id: id * 10 + 1, label: '', hint: `${hint}-${id}`, enabled: true }] });
  const usage = { groups: [{ k: 'acme', tin: 600, tout: 0, calls: 6, errors: 0 }, { k: 'globex', tin: 0, tout: 0, calls: 0, errors: 0 }] };
  const keyUsage = { groups: [{ k: '••••', tin: 600, tout: 0, calls: 6, errors: 0 }] };
  const [a, b] = m.buildBuildings([acct(1, 'acme', '••••'), acct(2, 'globex', '••••')], usage, 10, keyUsage);
  assert.equal(a.calls, 6, 'acme keeps its own calls');
  assert.equal(b.calls, 0, 'globex is not credited with acme\'s calls through a shared hint');
  assert.equal(b.keys[0].calls, 0, 'a shared hint is not attributed to either key');

  // Neighbourhood: one encoding per question (docs/design/neighbourhood-design.md).
  {
    const acc = (id, name, ns, o = {}) => ({ id, name, namespace: ns, kind: 'openai-compatible', enabled: true, keys: [{ id, label: '', hint: `h${id}`, enabled: true }], ...o });
    const row = (k, tpmTimes5, calls, errors) => ({ k, tin: tpmTimes5, tout: 0, calls, errors });
    const city = m.buildBuildings([
      acc(1, 'OpenRouter', 'openrouter'), acc(2, 'Groq', 'groq'), acc(3, 'OpenAI', 'openai'),
      acc(4, 'Ollama', 'ollama', { enabled: false }), acc(5, 'Idle Co', 'idle'), acc(6, 'Two Fails', 'twofail'),
    ], { groups: [row('openrouter', 990000, 120, 2), row('groq', 48000, 30, 12), row('openai', 1100, 4, 4), row('twofail', 500, 2, 2)] }, 5);
    const by = Object.fromEntries(city.map((b) => [b.namespace, b]));
    assert.equal(by.openrouter.health, 'serving');
    assert.equal(by.groq.health, 'failing', '40% of 30 calls failed');
    assert.equal(by.openai.health, 'failing', '100% of 4 calls failed');
    assert.equal(by.twofail.health, 'serving', 'two calls are too few to call it failing');
    assert.equal(by.ollama.health, 'off');
    assert.equal(by.idle.health, 'quiet');
    assert.equal(m.buildBuildings([acc(1, 'A', 'a')], null)[0].health, 'unknown', 'no usage is unknown, never quiet');

    // Height is linear: 220 tok/min is ~0.1% of 198,000, not 44% as the old log scale drew it.
    const ceiling = m.scaleCeiling(city);
    assert.equal(ceiling, 200000, 'busiest 198,000 rounds up to 200k');
    assert.ok(Math.abs(m.share(by.openai.tpm, ceiling) - 220 / 200000) < 1e-9);
    assert.equal(m.share(300000, 100000), 1, 'over a fixed ceiling is full height');
    assert.equal(m.floorsOf(by.openrouter, ceiling), Math.round(198000 / 200000 * m.MAX_FLOORS));
    assert.equal(m.floorsOf(by.openai, ceiling), 1, 'any traffic is at least one storey');
    assert.equal(m.floorsOf(by.ollama, ceiling), 1);
    assert.equal(m.scaleCeiling(city, 100000), 100000, 'a fixed ceiling wins');
    assert.equal(m.scaleCeiling([]), 1000, 'never below 1,000');
    assert.deepEqual([1, 1001, 1999, 2001, 2600, 5001, 99999].map(m.niceCeil), [1, 2000, 2000, 2500, 5000, 10000, 100000]);
    assert.deepEqual(m.rulerTicks(200000), [0, 50000, 100000, 150000, 200000]);
    assert.deepEqual(m.rulerTicks(250000), [0, 50000, 100000, 150000, 200000, 250000]);
    assert.deepEqual(m.rulerTicks(5000), [0, 1000, 2000, 3000, 4000, 5000]);

    // Windows: lit share = calls that worked, red share = calls that failed.
    assert.deepEqual(m.windowMix(by.groq), { ok: 0.6, failed: 0.4 });
    assert.deepEqual(m.windowMix(by.idle), { ok: 0, failed: 0 }, 'no calls, dark windows');
    assert.deepEqual(m.windowMix(by.ollama), { ok: 0, failed: 0 });

    // Rows: failing in front, then serving, quiet, switched off; by name within a row.
    const names = (rows) => rows.map((r) => r.map((b) => b.name));
    assert.deepEqual(names(m.planRows(city)), [['Groq', 'OpenAI'], ['OpenRouter', 'Two Fails'], ['Idle Co'], ['Ollama']]);
    assert.deepEqual(names(m.planRows(city, 1)).slice(0, 2), [['Groq'], ['OpenAI']], 'rows wrap at perRow');
    const busier = city.map((b) => (b.namespace === 'twofail' ? { ...b, tpm: 999999 } : b));
    assert.deepEqual(names(m.planRows(busier)), names(m.planRows(city)), 'traffic alone never moves a building');

    // Signs: placed front first, later ones nudged up until nothing overlaps.
    const tops = m.placeSigns([{ id: 'a', x: 0, y: 100, w: 80, h: 30 }, { id: 'b', x: 40, y: 100, w: 80, h: 30 }, { id: 'c', x: 300, y: 100, w: 80, h: 30 }]);
    assert.equal(tops.get('a'), 100, 'the front sign stays');
    assert.ok(tops.get('b') <= 100 - 30 - 4, 'an overlapping sign moves clear above');
    assert.equal(tops.get('c'), 100, 'a sign with room stays');

    const short = (n) => String(n);
    assert.equal(m.signLine(by.groq, short), '40% failing');
    assert.equal(m.signLine(by.openrouter, short), '198000 tok/min');
    assert.equal(m.signLine(by.ollama, short), 'switched off');
  }

  // Chat attachments: binary rejected, fences outgrow the file's own.
  assert.ok(m.looksBinary('%PDF\u0000\u0001'));
  assert.ok(!m.looksBinary('plain text, café'));
  assert.equal(m.fenceFor('no fences'), '```');
  assert.equal(m.fenceFor('has ```js\ncode\n``` inside'), '````');

  // Effective settings say what a value means.
  assert.equal(m.effectiveText('retries', 0), '0 (off)');
  assert.equal(m.effectiveText('attempt_budget', 0), '0 (no limit)');
  assert.equal(m.effectiveText('dispatch_deadline', ''), 'No limit');
  assert.equal(m.effectiveText('translation_enabled', false), 'Off');
  assert.equal(m.effectiveText('cooldown_429', '30s'), '30s');

  // Parked countdowns.
  assert.equal(m.parkedFor(42), '42s');
  assert.equal(m.parkedFor(190), '3m 10s');
  assert.equal(m.parkedFor(3720), '1h 2m');
  assert.equal(m.secondsLeft({ until: new Date(Date.now() + 30_500).toISOString() }), 31);
  assert.equal(m.secondsLeft({ until: new Date(Date.now() - 5_000).toISOString() }), 0);
  // Get started: progress from the server, a sensible test model, free namespaces.
  const app = { roles: ['infer'], enabled: true };
  assert.deepEqual(m.startProgress([], []), { provider: false, key: false, request: false, done: 0, next: 1 });
  assert.equal(m.startProgress([{ id: 1, namespace: 'x', enabled: true }], [{ roles: ['admin', 'infer'], enabled: true }]).next, 2);
  assert.equal(m.startProgress([{ id: 1, namespace: 'x', enabled: true }], [app]).next, 3);
  assert.equal(m.startProgress([{ id: 1, namespace: 'x', enabled: true }], [{ ...app, last_used_at: '2026-11-27' }]).done, 3);
  assert.equal(m.isAppKey({ roles: ['infer'], enabled: false }), false);
  assert.equal(m.defaultTestModel(['acme/big', 'openai/gpt-x', 'openai/gpt-y:free'], 'openai'), 'openai/gpt-y:free');
  assert.equal(m.defaultTestModel(['acme/big', 'openai/gpt-x'], 'openai'), 'openai/gpt-x');
  assert.equal(m.defaultTestModel(['daily', 'acme/big'], 'nope'), 'acme/big');
  assert.equal(m.defaultTestModel([], 'openai'), '');
  assert.equal(m.freeNamespace('openai', ['openai', 'openai-2']), 'openai-3');
  assert.equal(m.freeNamespace('openai', []), 'openai');
  for (const id of m.START_PRESETS) assert.ok(m.PROVIDER_PRESETS[id], `preset ${id} exists`);
  // Import plan in one line; only creates and updates write anything.
  assert.equal(m.summarizeImport({ create: 2, update: 1, unchanged: 5 }), '2 to create, 1 to update, 5 unchanged');
  assert.equal(m.summarizeImport({ skip: 1 }), '1 skipped');
  assert.equal(m.summarizeImport({}), 'Nothing to import');
  assert.equal(m.importWrites({ unchanged: 4, skip: 1 }), false);
  assert.equal(m.importWrites({ update: 1 }), true);
  // Ledger retention: 0 or at least 32 days.
  assert.equal(m.settingError('ledger_retention_days', 0), '');
  assert.equal(m.settingError('ledger_retention_days', 90), '');
  assert.notEqual(m.settingError('ledger_retention_days', 7), '');
  assert.equal(m.effectiveText('ledger_retention_days', 0), '0 (keep every call)');
  // Alert thresholds.
  assert.equal(m.settingError('alert_quota_percent', 80), '');
  assert.notEqual(m.settingError('alert_quota_percent', 0), '');
  assert.equal(m.settingError('alert_balance_below', 2.5), '');
  assert.notEqual(m.settingError('alert_balance_below', 0), '');
  assert.notEqual(m.settingError('alert_cookie_hours', 721), '');
  // A model is parked only when every key of its provider is.
  const park = (model, key_id) => ({ namespace: 'acme', model, key_id, until: '', strikes: 0, probe_active: false, reason: '429' });
  const split = m.splitParks([park('a', 1), park('b', 1), park('b', 2)], 2);
  assert.deepEqual(split.full.map((c) => c.model), ['b', 'b']);
  assert.deepEqual(split.partial.map((c) => c.model), ['a']);
  assert.equal(m.splitParks([park('a', 1)], 1).full.length, 1);
  assert.equal(m.splitParks([park('a', 1)], 0).full.length, 1);

  // CSV: quoting, and no formula injection from upstream text.
  assert.equal(m.csvField('plain'), 'plain');
  assert.equal(m.csvField('a,b'), '"a,b"');
  assert.equal(m.csvField('say "hi"'), '"say ""hi"""');
  assert.equal(m.csvField('=HYPERLINK("x")'), `"'=HYPERLINK(""x"")"`);
  assert.equal(m.csvField(-5), '-5', 'a negative number is not a formula');
  assert.equal(m.csvField(null), '');
  assert.equal(m.toCSV([{ a: 1, b: 'x\ny' }], [{ key: 'a', get: (r) => r.a }, { key: 'b', get: (r) => r.b }]), 'a,b\r\n1,"x\ny"\r\n');

  // Palette: every word must match, in label or hint.
  assert.deepEqual(m.filterPalette(m.PALETTE_ITEMS, 'new combo').map((i) => i.to), ['/combos?new=1']);
  assert.ok(m.filterPalette(m.PALETTE_ITEMS, 'pauses').some((i) => i.to === '/settings?tab=gateway'));
  assert.equal(m.filterPalette(m.PALETTE_ITEMS, '').length, m.PALETTE_ITEMS.length);

  // One status per model: works if any API passed, untested if none ran.
  assert.equal(m.modelStatus({ chat: true, anthropic: false, responses: null }), 'works');
  assert.equal(m.modelStatus({ chat: null, anthropic: null, responses: null }), 'untested');
  assert.equal(m.modelStatus({}), 'untested');
  assert.equal(m.modelStatus({ chat: false, anthropic: 404, responses: null }), 'fails');

  // Router messages read in the dashboard's words; ids and numbers stay.
  assert.equal(m.plainReason('hop acme/gpt-x: not in that account\'s catalog'), "acme/gpt-x: not in that provider's catalog");
  assert.equal(m.plainReason('probed unable to serve the anthropic surface and not proven on another wire'),
    "tested: doesn't work on Anthropic Messages, and no other API is proven");
  assert.equal(m.plainReason('model is pinned to a wire the probe disproved'), "forced to an API that testing showed doesn't work");
  assert.equal(m.plainReason('router: combo "daily" has no usable hops'), 'combo "daily" has no usable models');
  assert.equal(m.plainReason('"acme/m1" cannot serve the responses wire'), '"acme/m1" doesn\'t work on Responses');
  assert.equal(m.plainReason('openrouter/meta/llama:free is parked'), 'openrouter/meta/llama:free is paused');
  assert.equal(m.plainReason('shopping hopper'), 'shopping hopper');
  assert.equal(m.plainReason(undefined), '');
  assert.equal(m.apiLabel('chat'), 'Chat Completions');

  // An extra is in use only when it would change a request or serve one.
  const none = { profiles: [], combos: [], tokens: [], rules: [], services: [] };
  assert.deepEqual(m.extraStates(none), { compression: false, rules: false, services: false, chat: null, neighbourhood: null });
  const on = { id: 1, enabled: true }, off = { id: 2, enabled: false };
  assert.equal(m.extraStates({ ...none, profiles: [on] }).compression, false, 'a profile no combo or key uses is not in use');
  assert.equal(m.extraStates({ ...none, profiles: [on], combos: [{ compression_profile_id: 1 }] }).compression, true);
  assert.equal(m.extraStates({ ...none, profiles: [on], tokens: [{ compression_profile_id: 1 }] }).compression, true);
  assert.equal(m.extraStates({ ...none, profiles: [off], combos: [{ compression_profile_id: 2 }] }).compression, false, 'a profile that is off is not in use');
  assert.equal(m.extraStates({ ...none, profiles: null }).compression, null, 'unknown when it did not load');
  assert.equal(m.extraStates({ ...none, rules: [{ enabled: false }] }).rules, false);
  assert.equal(m.extraStates({ ...none, rules: [{ enabled: true }] }).rules, true);
  assert.equal(m.extraStates({ ...none, services: [{ mode: 'auto', resolved_target: '' }] }).services, false);
  assert.equal(m.extraStates({ ...none, services: [{ mode: 'auto', resolved_target: 'acme/embed' }] }).services, true);
  assert.equal(m.extraStates({ ...none, services: [{ mode: 'off', resolved_target: 'acme/embed' }] }).services, false);
  assert.deepEqual(m.EXTRAS.map((x) => x.key), ['compression', 'rules', 'services', 'chat', 'neighbourhood']);

  // A subscription bridge reads as ready, needing sign-in, off or unknown.
  assert.equal(m.bridgeState(undefined), 'unknown');
  assert.equal(m.bridgeState({ enabled: false }), 'off');
  assert.equal(m.bridgeState({ enabled: true, status: { login: 'logged in (Pro)' } }), 'ready');
  assert.equal(m.bridgeState({ enabled: true, status: { login: 'Logged-in' } }), 'ready');
  assert.equal(m.bridgeState({ enabled: true, status: { login: 'not logged in' } }), 'signed-out');
  assert.equal(m.bridgeState({ enabled: true }), 'ready', 'no login report: let the connection test decide');
  for (const id of m.SUB_PRESETS) assert.ok(m.PROVIDER_PRESETS[id]?.bridge === id, `subscription ${id} has a bridge preset`);

  console.log('Dashboard logic: links, globs, key state, buildings, attachments, settings text, countdowns, CSV, the palette, Get started, the plain words and the extras hold.');
} finally { await rm(dir, { recursive: true, force: true }); }
