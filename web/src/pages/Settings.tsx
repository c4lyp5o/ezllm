import { useState } from 'react';
import { ApiError, changeDashboardPassword, clearToken } from '../utils/api';
import { Chip, SectionLabel, btn, cx, inputCls } from '../components/ui';
import PasswordStrength from '../components/PasswordStrength';
import { MIN_PASSWORD } from '../utils/passwordStrength';

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
    if (newPassword.length < MIN_PASSWORD) { setError(`Use at least ${MIN_PASSWORD} characters.`); return; }
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
        <p className="mt-2 text-sm text-mute">Choose a new password of at least {MIN_PASSWORD} characters. Your active dashboard sessions will be signed out after the change.</p>
        <form className="mt-6 max-w-md space-y-4" onSubmit={submit}>
          <label className="block text-xs font-medium text-dim">Current password
            <input className={cx(inputCls, 'mt-1.5')} type="password" autoComplete="current-password" value={currentPassword} onChange={(e) => setCurrentPassword(e.target.value)} required />
          </label>
          <label className="block text-xs font-medium text-dim">New password
            <input className={cx(inputCls, 'mt-1.5')} type="password" autoComplete="new-password" value={newPassword} onChange={(e) => setNewPassword(e.target.value)} minLength={MIN_PASSWORD} required />
            <PasswordStrength password={newPassword} />
          </label>
          <label className="block text-xs font-medium text-dim">Confirm new password
            <input className={cx(inputCls, 'mt-1.5')} type="password" autoComplete="new-password" value={confirmPassword} onChange={(e) => setConfirmPassword(e.target.value)} minLength={MIN_PASSWORD} required />
          </label>
          {error && <p className="text-sm text-bad" role="alert">{error}</p>}
          {saved && <p className="text-sm text-good" role="status">Password updated. Signing out this session…</p>}
          <button disabled={busy} className={cx(btn.base, btn.primary)} type="submit">{busy ? 'Saving…' : 'Change password'}</button>
        </form>
      </section>
      <div className="flex items-start gap-3 rounded-xl border border-warn/30 bg-warn-dim p-4">
        <Chip tone="warn">Security</Chip>
        <p className="text-xs leading-relaxed text-mute">This password guards provider keys and client tokens, and there is no second factor. Keep it long and unique — the strength bar should read Good or Strong — especially if the dashboard is reachable beyond a trusted network.</p>
      </div>
    </div>
  );
}
