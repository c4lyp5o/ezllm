// Compare a fresh Vite build with the committed bundle, ignoring only Vite's
// content-hash suffixes in asset filenames and references. Chunk contents are
// compared byte-for-byte after those references are normalized.
import { readdir, readFile } from 'node:fs/promises';
import { join, relative } from 'node:path';

const [committed, fresh] = process.argv.slice(2);
if (!committed || !fresh) throw new Error('usage: node scripts/check-dist.mjs COMMITTED FRESH');

async function files(root, dir = root) {
  const out = [];
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) out.push(...await files(root, path));
    else if (entry.isFile()) out.push(relative(root, path).replaceAll('\\', '/'));
  }
  return out;
}
const canonical = name => name.replace(/-([A-Za-z0-9_-]{8})(?=\.(?:js|css)$)/, '-HASH');
async function readBuild(root) {
  const paths = await files(root);
  const replacements = paths.filter(p => canonical(p) !== p).map(p => [p.split('/').pop(), canonical(p).split('/').pop()]);
  const result = new Map();
  for (const path of paths) {
    const key = canonical(path);
    if (result.has(key)) throw new Error(`ambiguous asset name: ${key}`);
    let content = await readFile(join(root, path));
    if (/\.(?:js|css|html)$/.test(path)) {
      let text = content.toString('utf8');
      for (const [from, to] of replacements) text = text.replaceAll(from, to);
      content = Buffer.from(text);
    }
    result.set(key, content);
  }
  return result;
}
const a = await readBuild(committed), b = await readBuild(fresh);
const mismatches = [...new Set([...a.keys(), ...b.keys()])].filter(k => !a.has(k) || !b.has(k) || !a.get(k).equals(b.get(k)));
if (mismatches.length) {
  console.error('Committed dashboard differs from a fresh build:', mismatches.join(', '));
  process.exitCode = 1;
} else console.log('Committed dashboard matches the fresh build.');
