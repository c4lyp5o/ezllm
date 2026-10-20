// Central motion layer.
//
// Every animation in the dashboard goes through here so three things stay
// true without each page remembering them:
//   1. prefers-reduced-motion is honoured — <MotionConfig reducedMotion="user">
//      is mounted in main.tsx. That flag alone is NOT enough: it disables
//      TRANSFORM and layout animations but keeps opacity, so a fade still
//      ran for anyone who asked the OS for less motion. Every primitive
//      below therefore checks useReducedMotion() itself and renders the
//      settled state with no animation at all.
//   2. Durations come from the CSS tokens, so one edit retimes the app.
//   3. Nothing animates on a timer-driven repaint for longer than --dur-base:
//      the overview polls every 15s and a long ease reads as lag.
import { AnimatePresence, motion, useMotionValue, useMotionValueEvent, useReducedMotion, useSpring, type Transition } from 'motion/react';
import { useEffect, useRef, type ReactNode } from 'react';
import { cx } from '../utils/lib';

export const DUR = { fast: 0.14, base: 0.22, slow: 0.36 } as const;
export const EASE = [0.22, 1, 0.36, 1] as const;

export const rise: Transition = { duration: DUR.base, ease: EASE };
export const spring: Transition = { type: 'spring', stiffness: 420, damping: 34, mass: 0.8 };

// The motion layer (motion.dev). Modern and quick, never in the way:
//   - sections rise 12px and fade in, staggered ~60ms, ONCE per visit (never
//     on a poll's re-render);
//   - everything is transform + opacity, 300-450ms on an expo-out curve;
//   - a user who asked the OS for less motion gets the settled state with no
//     animation whatsoever.
const ENTER = { duration: 0.42, ease: EASE } as const;

/** A block that rises into place the first time it scrolls into view. */
export function Reveal({
  children, delay = 0, className, y = 12,
}: { children: ReactNode; delay?: number; className?: string; y?: number }) {
  const reduce = useReducedMotion();
  if (reduce) return <div className={className}>{children}</div>;
  return (
    <motion.div
      className={className}
      initial={{ opacity: 0, y }}
      whileInView={{ opacity: 1, y: 0 }}
      viewport={{ once: true, margin: '-40px' }}
      transition={{ ...ENTER, delay }}
    >
      {children}
    </motion.div>
  );
}

/** Staggered group: children use <StaggerItem> and must be the DIRECT
 *  children of this element — the stagger delay is computed from the
 *  orchestrator's own motion children, so a plain <div> in between would
 *  swallow it and every item would animate at once. */
export function Stagger({ children, className, step = 0.06 }: { children: ReactNode; className?: string; step?: number }) {
  const reduce = useReducedMotion();
  if (reduce) return <div className={className}>{children}</div>;
  return (
    <motion.div
      className={className}
      initial="hidden"
      animate="show"
      variants={{ show: { transition: { staggerChildren: step, delayChildren: 0.02 } } }}
    >
      {children}
    </motion.div>
  );
}

export const staggerItem = {
  hidden: { opacity: 0, y: 12 },
  show: { opacity: 1, y: 0, transition: ENTER },
};

/**
 * The table-row variant: opacity ONLY.
 *
 * A translate on a <tr> is a trap — a transform on a table row does not
 * participate in the table's own row sizing, so the row slides while the
 * cells stay put and the row visibly detaches from its own columns. A fade
 * has no such problem.
 */
export const staggerRow = {
  hidden: { opacity: 0 },
  show: { opacity: 1, transition: ENTER },
};

export function StaggerItem({ children, className }: { children: ReactNode; className?: string }) {
  const reduce = useReducedMotion();
  // `min-w-0` is load-bearing, not decoration. A grid or flex child defaults
  // to min-width:auto, so the wrapper's MIN-CONTENT width — set by the
  // longest unbreakable token inside it (a model id, a key name) — becomes the
  // track's minimum. The wrapper then widens its parent past the viewport
  // even when every child inside it already truncates. min-w-0 lets the track
  // shrink to the space it was given and lets the inner content handle its
  // own overflow.
  if (reduce) return <div className={cx('min-w-0', className)}>{children}</div>;
  return <motion.div className={cx('min-w-0', className)} variants={staggerItem}>{children}</motion.div>;
}

/**
 * The style's selection mark, animated: a 1px accent line under the chosen
 * label that SLIDES to the next choice (shared layoutId) instead of blinking
 * from one label to another. Render it inside a `relative` label.
 */
export function SelectionLine({ layoutId }: { layoutId: string }) {
  return (
    <motion.span
      layoutId={layoutId}
      aria-hidden
      className="pointer-events-none absolute inset-x-0 -bottom-px h-px bg-accent"
      transition={spring}
    />
  );
}

/**
 * A row that enters from the top and collapses on exit — for the live call
 * feed, where new items are prepended and the list is capped. `layout` keeps
 * the reflow smooth when the cap evicts a row.
 *
 * Only for lists whose keys are STABLE across updates (the SSE feed keys on
 * ts|model|tokens|status). A list keyed by array index would re-key every
 * row on every poll and the whole table would animate in on a 15s timer.
 *
 * `as` picks the element: a row inside a <ul> must BE an <li>, and a <div>
 * nested in a <ul> is invalid HTML that browsers silently fix up (which
 * breaks the parent's border/spacing rules).
 */
export function SlideRow({
  children, className, as = 'div',
}: { children: ReactNode; className?: string; as?: 'div' | 'li' }) {
  const reduce = useReducedMotion();
  const Comp = as === 'li' ? motion.li : motion.div;
  if (reduce) {
    const Plain = as === 'li' ? 'li' : 'div';
    return <Plain className={className}>{children}</Plain>;
  }
  return (
    <Comp
      className={className}
      layout="position"
      initial={{ opacity: 0, y: -8 }}
      animate={{ opacity: 1, y: 0 }}
      exit={{ opacity: 0, height: 0, marginTop: 0, marginBottom: 0 }}
      transition={spring}
    >
      {children}
    </Comp>
  );
}

/**
 * Count-up. The tween is deliberately SKIPPED for exact small counts
 * (<10k): a call count of 612 animating through 612.4 reads as a rounded lie
 * about an exact number. Aggregates (token sums, money) are already rounded
 * by their formatter, so tweening them is honest.
 *
 * The span owns its own text: React renders it EMPTY and the motion value
 * writes the string into it. Rendering the number as a React child instead
 * fights the motion value — React would put "0" back on the node on every
 * re-render and the tween would restart from the wrong place.
 */
export function CountUp({
  value, format, className,
}: { value: number; format: (n: number) => string; className?: string }) {
  const ref = useRef<HTMLSpanElement>(null);
  const reduce = useReducedMotion();
  const tween = Number.isFinite(value) && Math.abs(value) >= 10_000 && !reduce;
  const spring$ = useSpring(tween ? 0 : value, { stiffness: 120, damping: 22, mass: 0.6 });
  const text = useMotionValue(format(value));

  // `format` is usually an inline arrow, so it is a new identity on every
  // render. Holding it in a ref keeps the effect's deps stable: a changing
  // `format` would re-run the effect and re-target the spring on EVERY
  // render, which reads as a number that never settles.
  const fmt = useRef(format);
  fmt.current = format;

  useEffect(() => {
    if (!tween) { text.set(fmt.current(value)); return; }
    spring$.set(value);
  }, [value, tween, spring$, text]);

  useMotionValueEvent(spring$, 'change', (latest) => text.set(fmt.current(latest)));
  useMotionValueEvent(text, 'change', (latest) => {
    if (ref.current && ref.current.textContent !== latest) ref.current.textContent = latest;
  });

  // First paint: the number is already in the node before the effect runs, so
  // there is no flash of an empty span.
  useEffect(() => {
    if (ref.current) ref.current.textContent = fmt.current(value);
  }, []);

  return <span ref={ref} className={cx(className)} />;
}

/**
 * A block that expands and collapses on a height animation instead of
 * appearing out of nowhere. Used for the conditional sub-sections of a form
 * (the manual model picker, the strategy help text) where the content has a
 * real, measurable height — a fade alone leaves the layout jumping.
 */
export function Collapse({
  open, children, className, height,
}: { open: boolean; children: ReactNode; className?: string; height?: number }) {
  const reduce = useReducedMotion();
  if (reduce) {
    return open ? <div className={className}>{children}</div> : null;
  }
  return (
    <AnimatePresence initial={false}>
      {open && (
        <motion.div
          key="collapse"
          className={className}
          initial={{ height: 0, opacity: 0 }}
          animate={{ height: height ?? 'auto', opacity: 1 }}
          exit={{ height: 0, opacity: 0 }}
          transition={{ duration: DUR.base, ease: EASE }}
          style={{ overflow: 'hidden' }}
        >
          {children}
        </motion.div>
      )}
    </AnimatePresence>
  );
}

/**
 * A row that can be reordered by the operator (a combo's hops, a profile's
 * compression stages). `layout` makes the row SLIDE to its new position
 * instead of teleporting, so the reordering is legible — the one motion in
 * the app that carries real information.
 */
export function ReorderItem({ children, className }: { children: ReactNode; className?: string }) {
  const reduce = useReducedMotion();
  if (reduce) return <div className={className}>{children}</div>;
  return (
    <motion.div className={className} layout transition={spring}>
      {children}
    </motion.div>
  );
}
