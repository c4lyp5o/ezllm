// The brand row every ez app uses: the shared ez icon (the favicon file), the
// lowercase name, and an optional scope after a divider.
export function Brand({ name, scope, size = 20 }: { name: string; scope?: string; size?: number }) {
  return (
    <div className="flex min-w-0 items-center gap-2.5">
      <img src="/icons/icon-192.png" alt="" width={size} height={size} className="flex-none rounded-[4px]" />
      <span className="text-[15px] font-semibold tracking-[-0.01em] text-ink">{name}</span>
      {scope && <span className="truncate border-l border-line-strong pl-3 text-[12px] text-mute">{scope}</span>}
    </div>
  );
}
