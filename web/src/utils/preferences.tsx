import { useSyncExternalStore, type ReactNode } from 'react';
import { MotionConfig, useReducedMotion as useSystemReducedMotion } from 'motion/react';
import type { RangeKey } from './api';

const EVENT = 'ezllm-preferences';
const memory = new Map<string, string>();
function read(key: string, fallback: string): string { try { return localStorage.getItem(key) ?? fallback; } catch { return memory.get(key) ?? fallback; } }
function subscribe(fn: () => void) { window.addEventListener(EVENT, fn); window.addEventListener('storage', fn); return () => { window.removeEventListener(EVENT, fn); window.removeEventListener('storage', fn); }; }
export function preferenceChanged() { window.dispatchEvent(new Event(EVENT)); }
export function savePreference(key: string, value: string) { memory.set(key, value); try { localStorage.setItem(key, value); } catch { /* The in-memory preference remains available. */ } preferenceChanged(); applyMotion(); }
export function readRange(): RangeKey { const range = read('ezllm.range', '24h'); return ['24h', '7d', '30d', '365d'].includes(range) ? range as RangeKey : '24h'; }
export function readMotion(): 'system' | 'reduce' { return read('ezllm.motion', 'system') === 'reduce' ? 'reduce' : 'system'; }
function applyMotion() { document.documentElement.dataset.reduceMotion = String(readMotion() === 'reduce'); }
export function useReducedMotion(): boolean { const preference = useSyncExternalStore(subscribe, readMotion); const system = useSystemReducedMotion(); return preference === 'reduce' || !!system; }
export function PreferencesMotion({ children }: { children: ReactNode }) { const preference = useSyncExternalStore(subscribe, readMotion); applyMotion(); return <MotionConfig reducedMotion={preference === 'reduce' ? 'always' : 'user'}>{children}</MotionConfig>; }
export function subscribePreferences(fn: () => void) { return subscribe(fn); }
