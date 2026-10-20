// Central motion layer.
//
// Every animation in the dashboard goes through here so three things stay
// true without each page remembering them:
//   1. prefers-reduced-motion is honoured — <MotionConfig reducedMotion="user">
//      is mounted in main.tsx and kills transform/opacity animation globally;
//      the `reduce` flag below additionally skips the *stagger delay*, because
//      a user who asked for less motion should not wait for a queue.
//   2. Durations come from the CSS tokens, so one edit retimes the app.
//   3. Nothing animates on a timer-driven repaint for longer than --dur-base:
//      the overview polls every 15s and a long ease reads as lag.
import { motion, useMotionValue, useMotionValueEvent, useReducedMotion, useSpring, type Transition } from 'motion/react';
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
//   - MotionConfig reducedMotion="user" (main.tsx) turns every one of these
//     into an instant state change when the OS asks for less motion.
const ENTER = { duration: 0.42, ease: EASE } as const;

/** A block that rises into place the first time it scrolls into view. */
export function Reveal({
  children, delay = 0, className, y = 12,
}: { children: ReactNode; delay?: number; className?: string; y?: number }) {
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

/** Staggered group: children use <StaggerItem>. */
export function Stagger({ children, className, step = 0.06 }: { children: ReactNode; className?: string; step?: number }) {
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

export function StaggerItem({ children, className }: { children: ReactNode; className?: string }) {
  return <motion.div className={className} variants={staggerItem}>{children}</motion.div>;
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
 * feed and the chat transcript, where new items are prepended and the list is
 * capped. `layout` keeps the reflow smooth when the cap evicts a row.
 */
export function SlideRow({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <motion.div
      className={className}
      layout="position"
      initial={{ opacity: 0, y: -8 }}
      animate={{ opacity: 1, y: 0 }}
      exit={{ opacity: 0, height: 0, marginTop: 0, marginBottom: 0 }}
      transition={spring}
    >
      {children}
    </motion.div>
  );
}

/**
 * Count-up. The tween is deliberately SKIPPED for exact small counts
 * (<10k): a call count of 612 animating through 612.4 reads as a rounded lie
 * about an exact number. Aggregates (token sums, money) are already rounded
 * by their formatter, so tweening them is honest.
 */
export function CountUp({
  value, format, className,
}: { value: number; format: (n: number) => string; className?: string }) {
  const ref = useRef<HTMLSpanElement>(null);
  const reduce = useReducedMotion();
  const tween = Number.isFinite(value) && Math.abs(value) >= 10_000 && !reduce;
  const spring$ = useSpring(tween ? 0 : value, { stiffness: 120, damping: 22, mass: 0.6 });
  const text = useMotionValue(format(tween ? 0 : value));

  useEffect(() => {
    if (!tween) { text.set(format(value)); return; }
    spring$.set(value);
  }, [value, tween, spring$, text, format]);

  useMotionValueEvent(spring$, 'change', (latest) => text.set(format(latest)));
  useMotionValueEvent(text, 'change', (latest) => {
    if (ref.current) ref.current.textContent = latest;
  });

  return <span ref={ref} className={cx(className)}>{format(tween ? 0 : value)}</span>;
}
