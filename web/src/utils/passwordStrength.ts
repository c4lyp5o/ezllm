// Password strength for the set/change password forms. A local estimate, not
// zxcvbn (≈400 kB): bits ≈ length × log2(character pool), cut down for the
// patterns that make long passwords weak — repeats, repeated chunks,
// keyboard/alphabet runs, and well-known passwords. It guides; the server
// only enforces the length.

export const MIN_PASSWORD = 12; // store.MinDashboardPasswordLen

export type Strength = {
  score: 0 | 1 | 2 | 3 | 4; // 0 = below the minimum length
  label: 'Too short' | 'Weak' | 'Fair' | 'Good' | 'Strong';
  hint: string;
};

const COMMON = [
  'password', 'passw0rd', 'strongpassword', 'qwerty', 'letmein', 'welcome',
  'admin', 'administrator', 'iloveyou', 'monkey', 'dragon', 'sunshine',
  'football', 'baseball', 'princess', 'trustno1', 'ezllm', 'changeme',
];

const RUNS = ['abcdefghijklmnopqrstuvwxyz', '0123456789', 'qwertyuiop', 'asdfghjkl', 'zxcvbnm'];

function pool(pw: string): number {
  let n = 0;
  if (/[a-z]/.test(pw)) n += 26;
  if (/[A-Z]/.test(pw)) n += 26;
  if (/[0-9]/.test(pw)) n += 10;
  if (/[^a-zA-Z0-9]/.test(pw)) n += 33;
  return Math.max(n, 1);
}

// Characters that add little: each one repeating the previous, or continuing
// an ascending/descending run of a known sequence, counts a quarter.
function weakChars(pw: string): number {
  const s = pw.toLowerCase();
  let weak = 0;
  for (let i = 1; i < s.length; i++) {
    if (s[i] === s[i - 1]) { weak++; continue; }
    const pair = s[i - 1] + s[i];
    const rev = s[i] + s[i - 1];
    if (RUNS.some((r) => r.includes(pair) || r.includes(rev))) weak++;
  }
  return weak;
}

// The shortest chunk the password is a whole repetition of ("weakweakweak"
// → "weak"), so repeating a word adds nothing.
function repeatUnit(pw: string): string {
  for (let n = 1; n <= pw.length / 2; n++) {
    if (pw.length % n === 0 && pw.slice(0, n).repeat(pw.length / n) === pw) return pw.slice(0, n);
  }
  return pw;
}

export function passwordStrength(pw: string): Strength {
  if (pw.length < MIN_PASSWORD) {
    return { score: 0, label: 'Too short', hint: `${MIN_PASSWORD - pw.length} more character${MIN_PASSWORD - pw.length === 1 ? '' : 's'} needed` };
  }
  const lower = pw.toLowerCase();
  const unit = repeatUnit(pw);
  const effective = unit.length - weakChars(unit) * 0.75;
  let bits = effective * Math.log2(pool(pw));
  const common = COMMON.find((w) => lower.includes(w));
  if (common) bits -= common.length * Math.log2(pool(pw)) * 0.8;
  if (new Set(lower).size <= 3) bits = Math.min(bits, 20);

  // Thresholds sit high on purpose: the estimate cannot see dictionary
  // words, so "Sunrise-train7" must land at Good, not Strong.
  if (unit !== pw) return { score: 1, label: 'Weak', hint: 'repeating a chunk adds nothing' };
  if (bits < 50) return { score: 1, label: 'Weak', hint: common ? `avoid well-known words like "${common}"` : 'add length or mix in other character types' };
  if (bits < 70) return { score: 2, label: 'Fair', hint: 'a few more characters would help' };
  if (bits < 95) return { score: 3, label: 'Good', hint: 'solid; a longer passphrase is stronger still' };
  return { score: 4, label: 'Strong', hint: 'excellent' };
}
