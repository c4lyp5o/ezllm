import { mkdtemp, mkdir, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import { strict as assert } from 'node:assert';
import { fileURLToPath } from 'node:url';

const root = await mkdtemp(join(tmpdir(), 'ezllm-dist-check-'));
const script = fileURLToPath(new URL('../scripts/check-dist.mjs', import.meta.url));
const first = join(root, 'first'), second = join(root, 'second');
try {
  await mkdir(join(first, 'assets'), { recursive: true });
  await mkdir(join(second, 'assets'), { recursive: true });
  await writeFile(join(first, 'index.html'), '<script src="/assets/app-aaaaaaaa.js"></script>');
  await writeFile(join(second, 'index.html'), '<script src="/assets/app-bbbbbbbb.js"></script>');
  await writeFile(join(first, 'assets/app-aaaaaaaa.js'), 'console.log("original");');
  await writeFile(join(second, 'assets/app-bbbbbbbb.js'), 'console.log("original");');
  const run = () => spawnSync(process.execPath, [script, first, second], { encoding: 'utf8' });
  assert.equal(run().status, 0, 'hash-only differences should match');
  await writeFile(join(second, 'assets/app-bbbbbbbb.js'), 'console.log("changed");');
  assert.equal(run().status, 1, 'changed JS must be rejected');
  await rm(join(second, 'assets/app-bbbbbbbb.js'));
  assert.equal(run().status, 1, 'missing JS must be rejected');
  console.log('Bundle comparison accepts hash changes and rejects changed or missing assets.');
} finally { await rm(root, { recursive: true, force: true }); }
