// The dashboard's words. The API and the router keep their field names and
// messages (account, hop, surface, wire, probe, pin); the page says provider,
// model, API, test and force. docs/glossary.md is the list for people.

const API_LABEL: Record<string, string> = {
  chat: 'Chat Completions', anthropic: 'Anthropic Messages', responses: 'Responses', gemini: 'Gemini',
};

/** The name of an API, for people: "chat" → "Chat Completions". */
export const apiLabel = (s: string) => API_LABEL[s] ?? s;

type Rewrite = [RegExp, (...m: string[]) => string];

// Whole phrases first, so they read as sentences; single words after.
const PHRASES: Rewrite[] = [
  [/^router: /, () => ''],
  [/^hop (\S+\/\S+): /, (_m, id) => `${id}: `],
  [/^hop account (\S+): /, (_m, id) => `${id}: `],
  [/probed unable to serve the (\w+) surface and not proven on another wire/g, (_m, s) => `tested: doesn't work on ${apiLabel(s)}, and no other API is proven`],
  [/probed unable to serve the (\w+) surface/g, (_m, s) => `tested: doesn't work on ${apiLabel(s)}`],
  [/model is pinned to a wire the probe disproved/g, () => "forced to an API that testing showed doesn't work"],
  [/cannot serve the (\w+) wire/g, (_m, s) => `doesn't work on ${apiLabel(s)}`],
  [/no request converter for (\w+) client on the (\w+) wire/g, (_m, a, b) => `can't convert a ${apiLabel(a)} request to ${apiLabel(b)}`],
];
const WORDS: [RegExp, string][] = [
  [/\baccounts\b/g, 'providers'], [/\baccount\b/g, 'provider'], [/\bAccount\b/g, 'Provider'],
  [/\bhops\b/g, 'models'], [/\bhop\b/g, 'model'], [/\bHops\b/g, 'Models'], [/\bHop\b/g, 'Model'],
  [/\bsurfaces\b/g, 'APIs'], [/\bsurface\b/g, 'API'], [/\bwires\b/g, 'APIs'], [/\bwire\b/g, 'API'],
  [/\bprobed\b/g, 'tested'], [/\bprobes\b/g, 'tests'], [/\bprobe\b/g, 'test'],
  [/\bpinned\b/g, 'forced'], [/\bparked\b/g, 'paused'], [/\btranslated\b/g, 'converted'],
];

/** A router or tester message in the dashboard's words. */
export function plainReason(s: string | null | undefined): string {
  if (!s) return '';
  let out = s;
  for (const [re, to] of PHRASES) out = out.replace(re, to as (...m: string[]) => string);
  for (const [re, to] of WORDS) out = out.replace(re, to);
  return out;
}
