import { useState } from 'react';
import { ApiError, changeDashboardPassword, clearToken } from '../api';
import { Chip, SectionLabel, btn, cx, inputCls } from '../ui';

export default function Settings() {
  const [currentPassword, setCurrentPassword] = useState('');
  const [newPassword, setNewPassword] = useState('');
  const [confirmPassword, setConfirmPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [saved, setSaved] = useState(false);

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    setError('');
    setSaved(false);
    if (newPassword.length < 12) { setError('Use at least 12 characters.'); return; }
    if (newPassword !== confirmPassword) { setError('The new passwords do not match.'); return; }
    setBusy(true);
    try {
      await changeDashboardPassword(currentPassword, newPassword);
      setSaved(true);
      setCurrentPassword(''); setNewPassword(''); setConfirmPassword('');
      // Password change revokes every session, including this browser's.
      window.setTimeout(() => { clearToken(); window.location.reload(); }, 900);
    } catch (cause) {
      setError(cause instanceof ApiError ? cause.message : cause instanceof Error ? cause.message : 'Could not update password.');
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="max-w-2xl space-y-6">
      <section className="rounded-xl border border-line bg-surface p-5 sm:p-6">
        <SectionLabel>Dashboard password</SectionLabel>
        <p className="mt-2 text-sm text-mute">Choose a new password of at least 12 characters. Your active dashboard sessions will be signed out after the change.</p>
        <form className="mt-6 max-w-md space-y-4" onSubmit={submit}>
          <label className="block text-xs font-medium text-dim">Current password
            <input className={cx(inputCls, 'mt-1.5')} type="password" autoComplete="current-password" value={currentPassword} onChange={(e) => setCurrentPassword(e.target.value)} required />
          </label>
          <label className="block text-xs font-medium text-dim">New password
            <input className={cx(inputCls, 'mt-1.5')} type="password" autoComplete="new-password" value={newPassword} onChange={(e) => setNewPassword(e.target.value)} minLength={12} required />
          </label>
          <label className="block text-xs font-medium text-dim">Confirm new password
            <input className={cx(inputCls, 'mt-1.5')} type="password" autoComplete="new-password" value={confirmPassword} onChange={(e) => setConfirmPassword(e.target.value)} minLength={12} required />
          </label>
          {error && <p className="text-sm text-bad" role="alert">{error}</p>}
          {saved && <p className="text-sm text-good" role="status">Password updated. Signing out this session…</p>}
          <button disabled={busy} className={cx(btn.base, btn.primary)} type="submit">{busy ? 'Saving…' : 'Change password'}</button>
        </form>
      </section>
      <div className="flex items-start gap-3 rounded-xl border border-warn/30 bg-[rgba(251,191,36,0.05)] p-4">
        <Chip tone="warn">Security</Chip>
        <p className="text-xs leading-relaxed text-mute">The first-run password is displayed on the sign-in page by design. Change it before exposing the dashboard beyond a trusted network.</p>
      </div>
    </div>
  );
}
