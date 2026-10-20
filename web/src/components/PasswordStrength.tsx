import { motion } from 'motion/react';
import { passwordStrength } from '../utils/passwordStrength';
import { DUR, EASE } from './motion';
import { cx } from './ui';

// Status colours carry meaning here (house rule): bad below the bar, warn for
// fair, ok for good and up. Segments fill left to right on a short tween.
const TONE = ['bg-bad', 'bg-bad', 'bg-warn', 'bg-ok', 'bg-ok'] as const;
const TEXT = ['text-bad', 'text-bad', 'text-warn', 'text-ok', 'text-ok'] as const;

export default function PasswordStrength({ password }: { password: string }) {
  if (!password) return null;
  const s = passwordStrength(password);
  return (
    <div className="mt-2" aria-live="polite">
      <div className="flex gap-1" role="meter" aria-label="Password strength"
        aria-valuemin={0} aria-valuemax={4} aria-valuenow={s.score} aria-valuetext={s.label}>
        {[1, 2, 3, 4].map((seg) => (
          <div key={seg} className="h-1 flex-1 overflow-hidden rounded-full bg-line">
            <motion.div
              className={cx('h-full origin-left rounded-full', TONE[s.score])}
              initial={false}
              animate={{ scaleX: s.score >= seg || (s.score === 0 && seg === 1) ? 1 : 0, opacity: s.score === 0 ? 0.5 : 1 }}
              transition={{ duration: DUR.base, ease: EASE }}
            />
          </div>
        ))}
      </div>
      <p className="mt-1.5 text-[12px]">
        <span className={cx('font-medium', TEXT[s.score])}>{s.label}</span>
        <span className="text-mute"> · {s.hint}</span>
      </p>
    </div>
  );
}
