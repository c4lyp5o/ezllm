// Bridge pieces shared by Add provider and Get started.
import { CopyButton } from './ui';

// Sign-in commands per bridge, matching the README: every bridge logs in to its
// own ~/.<bridge>-sub folder with the binary the bridge mounts (the updater's
// layout; the runbook covers other install paths).
export const BRIDGE_SIGN_IN: Record<string, { login: string[]; check: string; note: string }> = {
  'claude-sub': {
    login: ['CLAUDE_CONFIG_DIR=~/.claude-sub ~/.local/share/claude-bridge/current/claude auth login'],
    check: 'CLAUDE_CONFIG_DIR=~/.claude-sub ~/.local/share/claude-bridge/current/claude auth status',
    note: 'Opens a browser so you can sign in with the account that has the Claude plan.',
  },
  'chatgpt-sub': {
    login: ['CODEX_HOME=~/.codex-sub ~/.local/share/codex-bridge/current/bin/codex login --device-auth'],
    check: 'CODEX_HOME=~/.codex-sub ~/.local/share/codex-bridge/current/bin/codex login status',
    note: 'Prints a URL and a code. Open the URL, enter the code and approve it with your ChatGPT account.',
  },
  'antigravity-sub': {
    login: ['mkdir -p ~/.agy-sub && chmod 700 ~/.agy-sub', 'env -u DBUS_SESSION_BUS_ADDRESS HOME=$HOME/.agy-sub ~/.local/share/agy-bridge/current/agy'],
    check: 'ls ~/.agy-sub/.gemini/antigravity-cli/antigravity-oauth-token',
    note: 'Opens the CLI. Sign in, send one prompt, then type /exit. Unsetting D-Bus makes it save the login to a file the container can read.',
  },
  'copilot-sub': {
    login: ['env -u DBUS_SESSION_BUS_ADDRESS COPILOT_HOME=~/.copilot-sub ~/.local/share/copilot-bridge/current/copilot login --device-code'],
    check: 'ls ~/.copilot-sub/config.json',
    note: 'Prints a URL and a code. Open the URL, enter the code and approve it with the GitHub account that has Copilot.',
  },
};

export function SignInCommands({ id }: { id: string }) {
  const s = BRIDGE_SIGN_IN[id];
  if (!s) return null;
  const line = (cmd: string) => (
    <div key={cmd} className="flex items-start gap-2">
      <code className="font-code min-w-0 flex-1 break-all rounded bg-shade px-2 py-1 text-[12px] text-dim">{cmd}</code>
      <CopyButton text={cmd} label="Copy" />
    </div>
  );
  return (
    <div className="space-y-1.5 text-[12px]">
      <p className="text-mute">Sign in on the host (once):</p>
      {s.login.map(line)}
      <p className="text-mute">{s.note} Check it worked:</p>
      {line(s.check)}
    </div>
  );
}
