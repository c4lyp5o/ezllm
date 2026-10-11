// Theme-flip proof.
//
// Screenshots CANNOT prove that a CSS custom property actually changed value:
// both palettes can look plausible in a screenshot while the token resolves to
// the same colour in both modes. That is precisely the silent failure this
// theme restructure is built to avoid — declaring the dark palette inside
// `@theme` emits the values at :root, so both modes resolve identically and a
// toggle appears broken with no error anywhere.
//
// So: read the resolved values, in both modes, and require that EVERY token
// differs. It also asserts the default is dark (no stored choice) and checks
// body-text contrast against WCAG AA in both modes.
//
// Usage:  BASE=http://127.0.0.1:4174 node verify-theme.mjs
//         (start it with `npx vite preview --port 4174` first)
import { chromium } from 'playwright';

const BASE = process.env.BASE ?? 'http://127.0.0.1:4174';

const VARS = [
  '--color-bg', '--color-surface', '--color-raised', '--color-ink', '--color-dim',
  '--color-mute', '--color-accent', '--color-ok', '--color-warn', '--color-bad',
  '--color-line', '--color-wash', '--color-inset', '--color-viz-in',
  '--color-viz-out', '--color-viz-cached', '--color-viz-grid',
];

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1440, height: 960 } });
await page.goto(BASE, { waitUntil: 'load' });

const read = () =>
  page.evaluate((names) => {
    const cs = getComputedStyle(document.documentElement);
    const out = {};
    for (const n of names) out[n] = cs.getPropertyValue(n).trim();
    out['__bodyBg'] = getComputedStyle(document.body).backgroundColor;
    out['__bodyColor'] = getComputedStyle(document.body).color;
    out['__htmlClass'] = document.documentElement.className;
    return out;
  }, VARS);

// 1. Fresh profile, no stored preference: must be DARK.
const dark = await read();
console.log('default class   :', dark.__htmlClass);
console.log('default body bg :', dark.__bodyBg, 'text:', dark.__bodyColor);
if (!dark.__htmlClass.includes('dark')) {
  console.error('FAIL: the default theme is not dark — <html> should carry .dark');
  await browser.close();
  process.exit(1);
}

// 2. Swap the class, exactly what ThemeToggle does, and re-read.
await page.evaluate(() => {
  const r = document.documentElement;
  r.classList.remove('dark');
  r.classList.add('light');
});
const light = await read();
console.log('light class     :', light.__htmlClass);
console.log('light body bg   :', light.__bodyBg, 'text:', light.__bodyColor);

let same = 0;
console.log('\ntoken                          dark                       light');
for (const k of VARS) {
  const bad = dark[k] === light[k] || dark[k] === '';
  if (bad) same++;
  console.log(
    `${k.padEnd(26)} ${String(dark[k]).padEnd(26)} ${String(light[k]).padEnd(26)}` +
    (bad ? (dark[k] === '' ? '   <-- UNRESOLVED' : '   <-- IDENTICAL') : ''),
  );
}

const contrast = (fg, bg) => {
  const lum = (c) => {
    const [r, g, b] = c.match(/\d+(\.\d+)?/g).slice(0, 3).map(Number)
      .map((v) => { const s = v / 255; return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4; });
    return 0.2126 * r + 0.7152 * g + 0.0722 * b;
  };
  const a = lum(fg), b = lum(bg);
  return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
};
const cd = contrast(dark.__bodyColor, dark.__bodyBg);
const cl = contrast(light.__bodyColor, light.__bodyBg);
console.log('\nbody-text contrast dark  :', cd.toFixed(2), ':1');
console.log('body-text contrast light :', cl.toFixed(2), ':1');

await browser.close();
const failures = [];
if (same > 0) failures.push(`${same} theme token(s) identical or unresolved between dark and light`);
if (cd < 4.5) failures.push(`dark body-text contrast ${cd.toFixed(2)} below AA 4.5:1`);
if (cl < 4.5) failures.push(`light body-text contrast ${cl.toFixed(2)} below AA 4.5:1`);
if (failures.length) { console.error('\nFAIL: ' + failures.join('; ')); process.exit(1); }
console.log('\nOK: default is dark, every token flips, both themes clear AA contrast');
