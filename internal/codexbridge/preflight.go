package codexbridge

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// QualifiedVersion is the CLI release the M0 measurements and the
// no-execution proof were made on. Another version still boots (the lockdown
// and schema checks below are what guard it) but is reported as unqualified
// until the live proof is re-run against it.
const QualifiedVersion = "0.163.0"

// RequiredMethods are the app-server requests this bridge sends; a CLI that
// drops one would fail every request at runtime, so boot fails instead.
var RequiredMethods = []string{"initialize", "thread/start", "turn/start", "model/list", "account/rateLimits/read"}

// RequiredNotifications are the server notifications a turn is read from.
var RequiredNotifications = []string{"item/agentMessage/delta", "turn/completed", "thread/tokenUsage/updated", "error"}

// Preflight is what boot learned about the CLI.
type Preflight struct {
	Version     string
	Qualified   bool
	Login       string
	LockdownOK  bool
	SchemaOK    bool
	MissingRPCs []string
	// Channel is ChannelAppServer, or ChannelExec when the app-server lacks a
	// method this bridge needs (operator decision §4.1).
	Channel string
}

// execFlags are the `codex exec` flags the fallback channel depends on.
var execFlags = []string{"--json", "--ephemeral", "--sandbox", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules"}

// RunPreflight checks the binary, the lockdown flags, the protocol surface and
// the login. Any failure is returned as an error: a bridge that boots without
// them would look healthy while every request failed, or — worse for the
// lockdown — run with a tool switched back on.
func RunPreflight(ctx context.Context, bin, codexHome string) (Preflight, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var p Preflight
	tmp, err := os.MkdirTemp("", "codex-bridge-preflight-")
	if err != nil {
		return p, err
	}
	defer os.RemoveAll(tmp)
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = tmp
		cmd.Env = childEnv(tmp, codexHome)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := run("--version")
	if err != nil {
		return p, fmt.Errorf("codex bridge: %s --version failed: %v (%s)", bin, err, strings.TrimSpace(out))
	}
	p.Version = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "codex-cli"))
	p.Qualified = p.Version == QualifiedVersion

	out, err = run(append(LockdownArgs(false), "features", "list")...)
	if err != nil || strings.Contains(out, "Unknown feature flag") {
		return p, fmt.Errorf("codex bridge: the CLI refused the lockdown flags (%s): this release renamed or dropped a feature the no-execution guarantee depends on", strings.TrimSpace(firstLine(out)))
	}
	if bad := lockdownViolations(out); len(bad) > 0 {
		return p, fmt.Errorf("codex bridge: features still enabled under lockdown: %s", strings.Join(bad, ", "))
	}
	p.LockdownOK = true

	schemaDir := filepath.Join(tmp, "schema")
	if out, err := run("app-server", "generate-json-schema", "--out", schemaDir); err != nil {
		return p, fmt.Errorf("codex bridge: cannot read the app-server schema: %v (%s)", err, strings.TrimSpace(firstLine(out)))
	}
	requests, _ := os.ReadFile(filepath.Join(schemaDir, "ClientRequest.json"))
	notes, _ := os.ReadFile(filepath.Join(schemaDir, "ServerNotification.json"))
	p.MissingRPCs = missingNames(string(requests), RequiredMethods)
	p.MissingRPCs = append(p.MissingRPCs, missingNames(string(notes), RequiredNotifications)...)
	p.SchemaOK = len(p.MissingRPCs) == 0
	p.Channel = ChannelAppServer
	if !p.SchemaOK {
		help, _ := run("exec", "--help")
		if missing := missingFlags(help, execFlags); len(missing) > 0 {
			return p, fmt.Errorf("codex bridge: app-server %s no longer offers %s, and the exec fallback lacks %s",
				p.Version, strings.Join(p.MissingRPCs, ", "), strings.Join(missing, ", "))
		}
		p.Channel = ChannelExec
	}

	if _, err := os.Stat(filepath.Join(codexHome, "auth.json")); err != nil {
		return p, fmt.Errorf("codex bridge: no ChatGPT login in %s (run: CODEX_HOME=%s codex login --device-auth)", codexHome, codexHome)
	}
	out, err = run("login", "status")
	p.Login = strings.TrimSpace(firstLine(out))
	if err != nil || !strings.Contains(strings.ToLower(out), "logged in") {
		return p, fmt.Errorf("codex bridge: login check failed in %s: %s", codexHome, p.Login)
	}
	return p, nil
}

// ignoredDisables are lockdown flags the CLI accepts but does not act on.
// unified_exec reports true under `--disable unified_exec` on 0.163.0
// (measured 2026-10-23); its tools (exec_command, write_stdin) are exposed only
// through shell_tool, which IS off — the locked tool list carried neither, and
// strace saw no model-driven execve (design §2.5). The flag stays in the argv
// so a release that starts honouring it is covered too.
var ignoredDisables = map[string]bool{"unified_exec": true}

// retiredDisables are lockdown features a release retired: `features list`
// shows them at stage "removed" and reports them true, because the flag no
// longer controls anything. Retired is not the same as harmless — Codex retires
// a flag both when it deletes a feature and when it makes one unconditional —
// so each name here was traced in that release's source to nothing a bridge
// child can reach. 0.166.0 (traced 2026-10-30, rust-v0.163.0 vs rust-v0.166.0):
//   - worktrees gated only TUI worktree views and the operator option
//     `codex exec --worktree`; 0.166.0 accepts --worktree unconditionally but
//     refuses it with --ephemeral or --ignore-user-config, which the exec channel
//     always passes, and no model tool creates a worktree.
//   - realtime_conversation gated only TUI voice; the app-server's
//     thread/realtime/* methods were already ungated on 0.163.0 and are client
//     requests this bridge never sends.
//
// The exception holds only at stage "removed": a release that brings either
// feature back at any other stage must report it false again. The flags stay
// in the argv for the same reason.
var retiredDisables = map[string]bool{"worktrees": true, "realtime_conversation": true}

// lockdownViolations parses `codex features list` ("name  stage  enabled",
// where a stage may be two words: "under development") and returns every
// lockdown feature that is not reported false.
func lockdownViolations(list string) []string {
	type feature struct{ stage, enabled string }
	state := map[string]feature{}
	for _, line := range strings.Split(list, "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 {
			state[f[0]] = feature{stage: strings.Join(f[1:len(f)-1], " "), enabled: f[len(f)-1]}
		}
	}
	var bad []string
	for _, name := range LockdownFeatures {
		s := state[name]
		if ignoredDisables[name] && s.enabled != "" {
			continue
		}
		if retiredDisables[name] && s.stage == "removed" {
			continue
		}
		if s.enabled != "false" {
			bad = append(bad, name+"="+orMissing(s.enabled))
		}
	}
	return bad
}

func missingFlags(help string, flags []string) []string {
	var missing []string
	for _, f := range flags {
		if !strings.Contains(help, f) {
			missing = append(missing, f)
		}
	}
	return missing
}

func missingNames(schema string, names []string) []string {
	var missing []string
	for _, n := range names {
		if !strings.Contains(schema, `"`+n+`"`) {
			missing = append(missing, n)
		}
	}
	return missing
}

func orMissing(s string) string {
	if s == "" {
		return "missing"
	}
	return s
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}
