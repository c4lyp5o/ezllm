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
    export { buildBuildings } from ${JSON.stringify(join(src, 'utils/city.ts'))};
    export { looksBinary, fenceFor } from ${JSON.stringify(join(src, 'utils/chatlog.ts'))};
    export { effectiveText } from ${JSON.stringify(join(src, 'utils/settings.ts'))};
    export { parkedFor, secondsLeft } from ${JSON.stringify(join(src, 'utils/cooldowns.ts'))};
  `);
  const out = join(dir, 'bundle.mjs');
  await build({ entryPoints: [entry], bundle: true, format: 'esm', platform: 'node', outfile: out, logLevel: 'error' });
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

  console.log('Dashboard logic: links, globs, key state, buildings, attachments, settings text and countdowns hold.');
} finally { await rm(dir, { recursive: true, force: true }); }
