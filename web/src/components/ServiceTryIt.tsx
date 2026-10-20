// "Try it" for the media services: the operator's own request, sent with the
// service's DEFAULT model through the real pipeline (POST
// /admin/services/{surface}/generate), with the output shown in the card.
// Nothing is stored server-side; results live only in this panel.
import { useEffect, useRef, useState, type FormEvent } from 'react';
import { ApiError, generateService, type ServiceSurface } from '../utils/api';
import { extractMedia, rawPreview, type Media } from '../utils/media';
import { Spinner, btn, cx, inputCls, labelCls } from './ui';
import { Collapse } from './motion';

export const GENERATABLE: ReadonlySet<ServiceSurface> = new Set<ServiceSurface>([
  'images_gen', 'images_edit', 'audio_tts', 'audio_stt', 'videos_gen', 'music_gen',
]);

const IMAGE_SIZES = ['1024x1024', '1024x1536', '1536x1024', '512x512'];

type Result = {
  ms: number;
  media: Media[];
  text: string;
  links: string[];
  raw: string; // '' for a binary answer
};

// The download's extension follows the real type (a data: URI's or the
// blob's), not the media kind — TTS may answer wav, opus or mp3.
function extOf(m: Media): string {
  const mime = m.mime ?? /^data:([^;,]+)/.exec(m.src)?.[1] ?? '';
  const sub = mime.split('/')[1]?.split(';')[0] ?? '';
  const known: Record<string, string> = { mpeg: 'mp3', 'x-wav': 'wav', wave: 'wav', jpeg: 'jpg', quicktime: 'mov' };
  return known[sub] ?? (/^[a-z0-9]+$/.test(sub) ? sub : m.kind === 'image' ? 'png' : m.kind === 'audio' ? 'mp3' : 'mp4');
}

function MediaView({ m, index }: { m: Media; index: number }) {
  const ext = extOf(m);
  return (
    <figure className="space-y-1.5">
      {m.kind === 'image' && <img src={m.src} alt={`Generated result ${index + 1}`} className="max-h-[420px] w-auto max-w-full rounded-lg border border-line bg-raised object-contain" />}
      {m.kind === 'audio' && <audio controls src={m.src} className="w-full" />}
      {m.kind === 'video' && <video controls src={m.src} className="max-h-[420px] w-full rounded-lg border border-line bg-black" />}
      <a href={m.src} download={`ezllm-${index + 1}.${ext}`} target="_blank" rel="noreferrer"
        className="text-[12px] text-accent underline-offset-2 hover:underline">
        {m.src.startsWith('http') ? 'Open original' : 'Download'}
      </a>
    </figure>
  );
}

export function ServiceTryIt({ surface, model }: { surface: ServiceSurface; model: string }) {
  const [prompt, setPrompt] = useState('');
  const [size, setSize] = useState(IMAGE_SIZES[0]);
  const [voice, setVoice] = useState('alloy');
  const [file, setFile] = useState<File | null>(null);
  const [mask, setMask] = useState<File | null>(null);
  const [running, setRunning] = useState(false);
  const [error, setError] = useState('');
  const [result, setResult] = useState<Result | null>(null);
  const abort = useRef<AbortController | null>(null);
  const objectURL = useRef<string | null>(null);

  // Release the previous blob URL when a new result replaces it, and on unmount.
  const releaseURL = () => {
    if (objectURL.current) { URL.revokeObjectURL(objectURL.current); objectURL.current = null; }
  };
  useEffect(() => () => { abort.current?.abort(); releaseURL(); }, []);

  const needsFile = surface === 'audio_stt' || surface === 'images_edit';
  const needsPrompt = surface !== 'audio_stt';
  const ready = (!needsPrompt || prompt.trim() !== '') && (!needsFile || file !== null);

  const buildBody = (): Record<string, unknown> | FormData => {
    switch (surface) {
      case 'images_gen':
        return { prompt: prompt.trim(), n: 1, size };
      case 'audio_tts':
        return { input: prompt.trim(), voice: voice.trim() || 'alloy' };
      case 'images_edit': {
        const fd = new FormData();
        fd.append('prompt', prompt.trim());
        fd.append('n', '1');
        if (file) fd.append('image', file);
        if (mask) fd.append('mask', mask);
        return fd;
      }
      case 'audio_stt': {
        const fd = new FormData();
        if (file) fd.append('file', file);
        return fd;
      }
      default: // videos_gen, music_gen — no common upstream standard; prompt only
        return { prompt: prompt.trim() };
    }
  };

  const run = async (e: FormEvent) => {
    e.preventDefault();
    if (!ready || running) return;
    if (!window.confirm(`This sends a real request to ${model} and may spend provider quota. Continue?`)) return;
    abort.current = new AbortController();
    setRunning(true); setError('');
    try {
      const out = await generateService(surface, buildBody(), abort.current.signal);
      releaseURL();
      if (out.kind === 'blob') {
        const url = URL.createObjectURL(out.blob);
        objectURL.current = url;
        const kind = out.contentType.split('/')[0];
        if (kind === 'image' || kind === 'audio' || kind === 'video') {
          setResult({ ms: out.ms, media: [{ kind, src: url, mime: out.contentType }], text: '', links: [], raw: '' });
        } else {
          setResult({ ms: out.ms, media: [], text: await out.blob.text(), links: [], raw: '' });
        }
      } else {
        const x = extractMedia(out.data, surface);
        setResult({ ms: out.ms, ...x, raw: rawPreview(out.data) });
      }
    } catch (err) {
      if (abort.current?.signal.aborted) setError('Cancelled.');
      else setError(err instanceof ApiError || err instanceof Error ? err.message : 'Generation failed.');
    } finally {
      setRunning(false);
      abort.current = null;
    }
  };

  const fieldCls = cx(inputCls, 'bg-surface');
  return (
    <form onSubmit={run} className="space-y-3">
      <p className="text-[12px] text-mute">
        Uses the saved default <code className="font-mono text-dim">{model}</code>. Results are not stored.
      </p>

      {needsFile && (
        <div className="grid gap-3 sm:grid-cols-2">
          <div>
            <label className={labelCls} htmlFor={`try-file-${surface}`}>{surface === 'audio_stt' ? 'Audio file' : 'Image to edit'}</label>
            <input id={`try-file-${surface}`} type="file" accept={surface === 'audio_stt' ? 'audio/*' : 'image/*'}
              onChange={(e) => setFile(e.target.files?.[0] ?? null)} disabled={running}
              className="block w-full text-[12px] text-mute file:mr-3 file:rounded-full file:border file:border-line-strong file:bg-transparent file:px-3 file:py-1 file:text-[12px] file:text-ink" />
          </div>
          {surface === 'images_edit' && (
            <div>
              <label className={labelCls} htmlFor={`try-mask-${surface}`}>Mask <span className="text-mute">(optional)</span></label>
              <input id={`try-mask-${surface}`} type="file" accept="image/png"
                onChange={(e) => setMask(e.target.files?.[0] ?? null)} disabled={running}
                className="block w-full text-[12px] text-mute file:mr-3 file:rounded-full file:border file:border-line-strong file:bg-transparent file:px-3 file:py-1 file:text-[12px] file:text-ink" />
            </div>
          )}
        </div>
      )}

      {needsPrompt && (
        <div>
          <label className={labelCls} htmlFor={`try-prompt-${surface}`}>
            {surface === 'audio_tts' ? 'Text to speak' : surface === 'images_edit' ? 'Edit instruction' : 'Prompt'}
          </label>
          <textarea id={`try-prompt-${surface}`} rows={3} maxLength={4000} value={prompt}
            onChange={(e) => setPrompt(e.target.value)} disabled={running} className={cx(fieldCls, 'resize-y')} />
        </div>
      )}

      {(surface === 'images_gen' || surface === 'audio_tts') && (
        <div className="max-w-[220px]">
          {surface === 'images_gen' ? (
            <>
              <label className={labelCls} htmlFor={`try-size-${surface}`}>Size</label>
              <select id={`try-size-${surface}`} value={size} onChange={(e) => setSize(e.target.value)} disabled={running} className={fieldCls}>
                {IMAGE_SIZES.map((s) => <option key={s} value={s}>{s}</option>)}
              </select>
            </>
          ) : (
            <>
              <label className={labelCls} htmlFor={`try-voice-${surface}`}>Voice</label>
              <input id={`try-voice-${surface}`} value={voice} onChange={(e) => setVoice(e.target.value)} disabled={running} className={cx(fieldCls, 'font-mono')} />
            </>
          )}
        </div>
      )}

      {(surface === 'videos_gen' || surface === 'music_gen') && (
        <p className="text-[11px] leading-relaxed text-mute">
          There is no common upstream format for this service: only the prompt is sent, and if the provider answers with a job to check later, the response is shown as it came.
        </p>
      )}

      <div className="flex flex-wrap items-center gap-2">
        <button type="submit" disabled={!ready || running} className={cx(btn.base, btn.primary)}>
          {running ? <><Spinner /> Generating…</> : 'Generate'}
        </button>
        {running && <button type="button" onClick={() => abort.current?.abort()} className={cx(btn.base, btn.ghost)}>Cancel</button>}
      </div>

      {error && <p className="rounded-lg border border-bad/30 bg-bad-dim px-3 py-2 text-[13px] text-bad" role="alert">{error}</p>}

      <Collapse open={result !== null}>
        {result && (
          <div className="space-y-3 rounded-lg border border-line bg-raised p-3.5" aria-live="polite">
            <p className="text-[11px] text-mute">Answered in <span className="font-mono">{result.ms.toLocaleString()}</span> ms</p>
            {result.media.map((m, i) => <MediaView key={m.src} m={m} index={i} />)}
            {result.text && <p className="whitespace-pre-wrap text-[13px] text-ink">{result.text}</p>}
            {result.links.map((l) => (
              <a key={l} href={l} target="_blank" rel="noreferrer" className="block break-all text-[12px] text-accent underline-offset-2 hover:underline">{l}</a>
            ))}
            {result.media.length === 0 && !result.text && result.links.length === 0 && (
              <p className="text-[12px] text-mute">Nothing playable was found in the response — the raw answer is below.</p>
            )}
            {result.raw && (
              <details>
                <summary className="cursor-pointer text-[12px] text-mute">Raw response</summary>
                <pre className="mt-2 max-h-64 overflow-auto rounded border border-line bg-surface p-2 font-mono text-[11px] text-dim">{result.raw}</pre>
              </details>
            )}
          </div>
        )}
      </Collapse>
    </form>
  );
}
