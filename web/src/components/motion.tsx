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
import { motion, useInView, useMotionValue, useMotionValueEvent, useReducedMotion, useSpring, type Transition } from 'framer-motion';
import { useEffect, useRef, type ReactNode } from 'react';
import { cx } from '../utils/lib';

export const DUR = { fast: 0.14, base: 0.22, slow: 0.36 } as const;
export const EASE = [0.22, 1, 0.36, 1] as const;

export const rise: Transition = { duration: DUR.base, ease: EASE };
export const spring: Transition = { type: 'spring', stiffness: 420, damping: 34, mass: 0.8 };

/**
 * Fade + 4px rise. `once` (default) means a card does not re-animate every
 * time the 15s poll re-renders it — the animation is keyed to entering the
 * viewport, not to data changing.
 */
export function Reveal({
  children, delay = 0, className, y = 4,
}: { children: ReactNode; delay?: number; className?: string; y?: number }) {
  const reduce = useReducedMotion();
  const ref = useRef<HTMLDivElement>(null);
  const inView = useInView(ref, { once: true, margin: '-40px' });
  return (
    <motion.div
      ref={ref}
      className={className}
      initial={reduce ? false : { opacity: 0, y }}
      animate={reduce || inView ? { opacity: 1, y: 0 } : undefined}
      transition={reduce ? { duration: 0 } : { ...rise, delay }}
    >
      {children}
    </motion.div>
  );
}

/** Staggered list: children use <StaggerItem>. */
export function Stagger({ children, className, step = 0.05 }: { children: ReactNode; className?: string; step?: number }) {
  return (
    <motion.div
      className={className}
      initial="hidden"
      animate="show"
      variants={{ show: { transition: { staggerChildren: step } } }}
    >
      {children}
    </motion.div>
  );
}

export const staggerItem = {
  hidden: { opacity: 0, y: 4 },
  show: { opacity: 1, y: 0, transition: { duration: DUR.base, ease: EASE } },
};

export function StaggerItem({ children, className }: { children: ReactNode; className?: string }) {
  return <motion.div className={className} variants={staggerItem}>{children}</motion.div>;
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
