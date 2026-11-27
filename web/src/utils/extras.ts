// The extras: what ezllm does beyond routing. One place for their names,
// what each one does in a sentence, and whether it's in use, shared by the
// sidebar, the Dashboard card and each page's introduction.
import { useEffect, useState } from 'react';
import { compressionProfiles, get, modelRules, serviceCards, type Combo, type CompressionProfile, type ModelRule, type ServiceCard } from './api';
import { listTokens, type TokenInfo } from './tokens';

export type ExtraKey = 'compression' | 'rules' | 'services' | 'chat' | 'neighbourhood';

export type Extra = {
  key: ExtraKey; label: string; to: string;
  /** One sentence for the Dashboard card. */
  line: string;
  /** Tools have nothing to set up, so they never say "in use" or "not set up". */
  tool?: boolean;
};

export const EXTRAS: Extra[] = [
  { key: 'compression', label: 'Compression', to: '/compression', line: 'Trims what the model doesn\'t need from long chats, so you pay for fewer tokens.' },
  { key: 'rules', label: 'Rules', to: '/rules', line: 'Token limits and allowed hours per model, checked before anything is sent.' },
  { key: 'services', label: 'Services', to: '/services', line: 'Embeddings, images, speech and transcription through the same address and keys.' },
  { key: 'chat', label: 'Chat', to: '/chat', line: 'Talk to your models side by side to compare their answers.', tool: true },
  { key: 'neighbourhood', label: 'Neighbourhood', to: '/neighbourhood', line: 'Your providers drawn as a small city, busy ones lit up.', tool: true },
];

/** true = in use, false = not set up, null = not known (it didn't load, or it's a tool). */
export type ExtraStates = Record<ExtraKey, boolean | null>;

export type ExtraInputs = {
  profiles: CompressionProfile[] | null;
  combos: Combo[] | null;
  tokens: TokenInfo[] | null;
  rules: ModelRule[] | null;
  services: ServiceCard[] | null;
};

/**
 * Compression is in use when a profile that's on is attached to a combo or a
 * key. Rules, when any rule is on. Services, when any service has a model to
 * send to, picked by you or found by itself.
 */
export function extraStates(i: ExtraInputs): ExtraStates {
  let compression: boolean | null = null;
  if (i.profiles && i.combos && i.tokens) {
    const on = new Set(i.profiles.filter((p) => p.enabled).map((p) => p.id));
    compression = i.combos.some((c) => c.compression_profile_id != null && on.has(c.compression_profile_id))
      || i.tokens.some((t) => t.compression_profile_id != null && on.has(t.compression_profile_id));
  }
  return {
    compression,
    rules: i.rules ? i.rules.some((r) => r.enabled) : null,
    services: i.services ? i.services.some((s) => s.mode !== 'off' && !!s.resolved_target) : null,
    chat: null,
    neighbourhood: null,
  };
}

// One fetch shared by every caller on the page; a new read after 5 seconds.
let pending: Promise<ExtraStates> | null = null;
let pendingAt = 0;
function loadExtras(): Promise<ExtraStates> {
  if (pending && Date.now() - pendingAt < 5000) return pending;
  pendingAt = Date.now();
  const soft = <T,>(p: Promise<T>) => p.catch(() => null);
  pending = Promise.all([
    soft(compressionProfiles()), soft(get<Combo[]>('/admin/combos')), soft(listTokens()),
    soft(modelRules()), soft(serviceCards().then((r) => r.services)),
  ]).then(([profiles, combos, tokens, rules, services]) => extraStates({ profiles, combos, tokens, rules, services }));
  return pending;
}

/** Which extras are in use; null until loaded. A change of `key` reads again. */
export function useExtras(key?: unknown): ExtraStates | null {
  const [states, setStates] = useState<ExtraStates | null>(null);
  useEffect(() => {
    let alive = true;
    loadExtras().then((s) => { if (alive) setStates(s); }).catch(() => undefined);
    return () => { alive = false; };
  }, [key]);
  return states;
}
