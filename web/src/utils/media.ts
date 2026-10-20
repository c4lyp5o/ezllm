// Media extraction for the Services "Try it" panel. Generation APIs answer in
// many shapes (OpenAI images: data[].b64_json / data[].url; transcription:
// {text}; video/music: no common standard), so the panel walks the JSON for
// anything playable rather than trusting one vendor's layout.

export type MediaKind = 'image' | 'audio' | 'video';
export type Media = { kind: MediaKind; src: string; mime?: string };
export type Extracted = { media: Media[]; text: string; links: string[] };

const EXT: Record<string, MediaKind> = {
  png: 'image', jpg: 'image', jpeg: 'image', webp: 'image', gif: 'image',
  mp3: 'audio', wav: 'audio', ogg: 'audio', flac: 'audio', m4a: 'audio', aac: 'audio', opus: 'audio',
  mp4: 'video', webm: 'video', mov: 'video',
};

/** The media kind a service's output most likely is — used for base64 that
 *  carries no type of its own. */
export function surfaceKind(surface: string): MediaKind | null {
  if (surface.startsWith('images')) return 'image';
  if (surface === 'audio_tts' || surface === 'music_gen') return 'audio';
  if (surface === 'videos_gen') return 'video';
  return null;
}

const MEDIA_KEY = /^(url|(image|audio|video|music|output|file|download)_?url)$/i;

const MIME: Record<MediaKind, string> = { image: 'image/png', audio: 'audio/mpeg', video: 'video/mp4' };

function kindOfURL(u: string): MediaKind | null {
  const m = /^data:(image|audio|video)\//i.exec(u);
  if (m) return m[1].toLowerCase() as MediaKind;
  try {
    const ext = new URL(u).pathname.split('.').pop()?.toLowerCase() ?? '';
    return EXT[ext] ?? null;
  } catch { return null; }
}

// Long, padding-safe base64 with nothing else in it — short strings (ids,
// hashes) are not media.
const B64 = /^[A-Za-z0-9+/\r\n]{200,}={0,2}$/;

export function extractMedia(data: unknown, surface: string): Extracted {
  const out: Extracted = { media: [], text: '', links: [] };
  const seen = new Set<string>();
  const fallback = surfaceKind(surface);
  const add = (kind: MediaKind, src: string) => {
    if (!seen.has(src)) { seen.add(src); out.media.push({ kind, src }); }
  };
  const walk = (v: unknown, key: string, depth: number) => {
    if (depth > 8 || v == null) return;
    if (typeof v === 'string') {
      if (/^(data:|https?:\/\/)/i.test(v)) {
        // An extensionless URL is media only under a media-named key: a
        // status_url / poll_url is a job to poll, not something to play.
        const kind = kindOfURL(v) ?? (MEDIA_KEY.test(key) ? fallback : null);
        if (kind) add(kind, v);
        else if (/^https?:\/\//i.test(v) && !seen.has(v)) { seen.add(v); out.links.push(v); }
      } else if (B64.test(v) && fallback) {
        add(fallback, `data:${MIME[fallback]};base64,${v.replace(/\s/g, '')}`);
      } else if (key === 'text' && !out.text) {
        out.text = v;
      }
      return;
    }
    if (Array.isArray(v)) { v.forEach((x) => walk(x, key, depth + 1)); return; }
    if (typeof v === 'object') {
      for (const [k, x] of Object.entries(v as Record<string, unknown>)) walk(x, k, depth + 1);
    }
  };
  walk(data, '', 0);
  return out;
}

/** Pretty JSON with base64 runs shortened, so the raw view stays readable. */
export function rawPreview(data: unknown): string {
  return JSON.stringify(data, (_k, v: unknown) =>
    typeof v === 'string' && v.length > 300 ? `${v.slice(0, 80)}… (${v.length.toLocaleString()} characters)` : v, 2);
}
