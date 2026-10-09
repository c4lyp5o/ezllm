// Minimal markdown renderer for assistant chat replies — bold, italic, strike,
// inline/fenced code, headings, lists, quotes and links. Hand-rolled (zero
// deps): React nodes only, never dangerouslySetInnerHTML, so provider output
// can't inject markup. Single newlines render as line breaks (chat habit),
// blank lines separate paragraphs.
import { Fragment, type ReactNode } from 'react';

// One alternation, scanned left to right: code wins over emphasis so `**x**`
// inside backticks stays literal. Kept as a SOURCE string, not a /g RegExp:
// inline() recurses into itself, and a shared regex object would let the inner
// call reset lastIndex mid-scan, so the outer loop re-matches the same token
// forever — that is the hang that froze the Chat tab on the first bold reply.
const INLINE_SRC = '(`[^`\\n]+`)|(\\*\\*[^*\\n]+\\*\\*)|(__[^_\\n]+__)|(~~[^~\\n]+~~)|(\\*[^*\\n]+\\*)|(_[^_\\n]+_)|(\\[[^\\]\\n]+\\]\\([^)\\s]+\\))';

function inline(src: string, key: string): ReactNode[] {
  const out: ReactNode[] = [];
  const re = new RegExp(INLINE_SRC, 'g'); // fresh scan state per call — never shared
  let last = 0;
  let i = 0;
  for (let m = re.exec(src); m !== null; m = re.exec(src)) {
    if (m[0].length === 0) { re.lastIndex++; continue; } // never spin on a zero-length match
    if (m.index > last) out.push(src.slice(last, m.index));
    const tok = m[0];
    const k = `${key}-${i++}`;
    if (tok.startsWith('`')) {
      out.push(<code key={k} className="rounded border border-line bg-raised px-1 py-0.5 font-mono text-[12px] text-ink">{tok.slice(1, -1)}</code>);
    } else if (tok.startsWith('**') || tok.startsWith('__')) {
      out.push(<strong key={k} className="font-semibold text-ink">{inline(tok.slice(2, -2), k)}</strong>);
    } else if (tok.startsWith('~~')) {
      out.push(<del key={k} className="text-mute">{inline(tok.slice(2, -2), k)}</del>);
    } else if (tok.startsWith('*') || tok.startsWith('_')) {
      out.push(<em key={k}>{inline(tok.slice(1, -1), k)}</em>);
    } else {
      const mm = /^\[([^\]]+)\]\(([^)\s]+)\)$/.exec(tok);
      const label = mm ? mm[1] : tok;
      const href = mm ? mm[2] : '#';
      out.push(
        <a key={k} href={href} target="_blank" rel="noreferrer noopener" className="text-accent underline decoration-accent/40 underline-offset-2 hover:decoration-accent">
          {label}
        </a>,
      );
    }
    last = m.index + tok.length;
  }
  if (last < src.length) out.push(src.slice(last));
  return out;
}

type Block =
  | { kind: 'code'; lang: string; body: string }
  | { kind: 'heading'; level: number; text: string }
  | { kind: 'quote'; text: string }
  | { kind: 'ul'; items: string[] }
  | { kind: 'ol'; items: string[] }
  | { kind: 'p'; lines: string[] };

function parse(src: string): Block[] {
  const lines = src.replace(/\r\n/g, '\n').split('\n');
  const blocks: Block[] = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (line.trim() === '') { i++; continue; }
    const trimmed = line.trim();
    const fence = /^```(\w*)\s*$/.exec(trimmed);
    // Any line that starts a fence opens a code block — not just the exact
    // "```lang" shape. A stray "``` json" or "````" line that missed the strict
    // regex would otherwise fall through to the paragraph branch, match nothing,
    // leave i unchanged, and spin the outer loop forever.
    if (trimmed.startsWith('```')) {
      const body: string[] = [];
      i++;
      while (i < lines.length && !lines[i].trim().startsWith('```')) body.push(lines[i++]);
      i++; // closing fence (or EOF)
      blocks.push({ kind: 'code', lang: fence ? fence[1] : '', body: body.join('\n') });
      continue;
    }
    const h = /^(#{1,4})\s+(.*)$/.exec(line);
    if (h) { blocks.push({ kind: 'heading', level: h[1].length, text: h[2] }); i++; continue; }
    if (/^\s*>\s?/.test(line)) {
      const q: string[] = [];
      while (i < lines.length && /^\s*>\s?/.test(lines[i])) q.push(lines[i++].replace(/^\s*>\s?/, ''));
      blocks.push({ kind: 'quote', text: q.join('\n') });
      continue;
    }
    if (/^\s*[-*+]\s+/.test(line)) {
      const items: string[] = [];
      while (i < lines.length && /^\s*[-*+]\s+/.test(lines[i])) items.push(lines[i++].replace(/^\s*[-*+]\s+/, ''));
      blocks.push({ kind: 'ul', items });
      continue;
    }
    if (/^\s*\d+[.)]\s+/.test(line)) {
      const items: string[] = [];
      while (i < lines.length && /^\s*\d+[.)]\s+/.test(lines[i])) items.push(lines[i++].replace(/^\s*\d+[.)]\s+/, ''));
      blocks.push({ kind: 'ol', items });
      continue;
    }
    const p: string[] = [];
    while (
      i < lines.length && lines[i].trim() !== '' &&
      !/^```/.test(lines[i].trim()) && !/^(#{1,4})\s+/.test(lines[i]) &&
      !/^\s*>\s?/.test(lines[i]) && !/^\s*[-*+]\s+/.test(lines[i]) && !/^\s*\d+[.)]\s+/.test(lines[i])
    ) p.push(lines[i++]);
    if (p.length === 0) { i++; continue; } // last-resort progress guarantee: never leave i stuck
    blocks.push({ kind: 'p', lines: p });
  }
  return blocks;
}

const HEADCLS: Record<number, string> = {
  1: 'text-[17px]',
  2: 'text-[16px]',
  3: 'text-[14.5px]',
  4: 'text-[13.5px]',
};

export function Markdown({ text, className }: { text: string; className?: string }) {
  const blocks = parse(text);
  return (
    <div className={className ?? 'space-y-2.5 text-[13.5px] leading-relaxed'}>
      {blocks.map((b, bi) => {
        const k = `b${bi}`;
        switch (b.kind) {
          case 'code':
            return (
              <pre key={k} className="overflow-x-auto rounded-lg border border-line bg-raised px-3 py-2.5 text-[12px]">
                <code className="font-mono text-dim">{b.body}</code>
              </pre>
            );
          case 'heading':
            return <div key={k} className={`font-semibold text-ink ${HEADCLS[b.level] ?? 'text-[13.5px]'}`}>{inline(b.text, k)}</div>;
          case 'quote':
            return (
              <blockquote key={k} className="border-l-2 border-accent/50 pl-3 text-mute italic">
                {b.text.split('\n').map((l, li) => <div key={`${k}-${li}`}>{inline(l, `${k}-${li}`)}</div>)}
              </blockquote>
            );
          case 'ul':
            return (
              <ul key={k} className="list-disc space-y-1 pl-5">
                {b.items.map((it, ii) => <li key={`${k}-${ii}`}>{inline(it, `${k}-${ii}`)}</li>)}
              </ul>
            );
          case 'ol':
            return (
              <ol key={k} className="list-decimal space-y-1 pl-5">
                {b.items.map((it, ii) => <li key={`${k}-${ii}`}>{inline(it, `${k}-${ii}`)}</li>)}
              </ol>
            );
          default:
            return (
              <p key={k} className="whitespace-pre-wrap">
                {b.lines.map((l, li) => (
                  <Fragment key={`${k}-${li}`}>
                    {li > 0 && <br />}
                    {inline(l, `${k}-${li}`)}
                  </Fragment>
                ))}
              </p>
            );
        }
      })}
    </div>
  );
}
