package codexbridge

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLockdownViolations(t *testing.T) {
	var b strings.Builder
	for _, f := range LockdownFeatures {
		b.WriteString(f + "                     stable             false\n")
	}
	if bad := lockdownViolations(b.String()); len(bad) != 0 {
		t.Fatalf("all-false list reported %v", bad)
	}
	list := strings.Replace(b.String(), "shell_tool                     stable             false", "shell_tool   stable   true", 1)
	list = strings.Replace(list, "code_mode_host", "renamed_host", 1)
	bad := lockdownViolations(list)
	if strings.Join(bad, ",") != "shell_tool=true,code_mode_host=missing" {
		t.Fatalf("violations = %v, want shell_tool=true and code_mode_host=missing", bad)
	}
	// unified_exec ignores its --disable on 0.163.0; reported true, it is not a
	// violation — but a release that drops the feature name still is.
	list = strings.Replace(b.String(), "unified_exec                     stable             false", "unified_exec   stable   true", 1)
	if bad := lockdownViolations(list); len(bad) != 0 {
		t.Fatalf("unified_exec=true reported %v", bad)
	}
	list = strings.Replace(b.String(), "unified_exec                     stable             false\n", "", 1)
	if bad := lockdownViolations(list); strings.Join(bad, ",") != "unified_exec=missing" {
		t.Fatalf("a dropped unified_exec = %v, want unified_exec=missing", bad)
	}
}

func TestMissingNames(t *testing.T) {
	schema := `{"enum":["initialize"]} {"enum":["thread/start"]} {"enum":["model/list"]}`
	got := missingNames(schema, RequiredMethods)
	if strings.Join(got, ",") != "turn/start,account/rateLimits/read" {
		t.Fatalf("missing = %v", got)
	}
}

// The real CLI, when present on this host: the lockdown and schema checks
// must pass against the qualified release.
func TestPreflightAgainstTheInstalledCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns the real CLI")
	}
	bin, home := liveCLI(t)
	p, err := RunPreflight(context.Background(), bin, home)
	if err != nil {
		t.Fatalf("preflight: %v (%+v)", err, p)
	}
	if !p.LockdownOK || !p.SchemaOK || p.Version == "" {
		t.Fatalf("preflight = %+v", p)
	}
}

// A release that accepts `--disable shell_tool` but stops honouring it must
// fail preflight end to end, not just in the parser: the bridge updater
// promotes on this verdict (SPEC-bridge-update.md §5).
func TestPreflightRefusesAReleaseThatIgnoresTheLockdown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake")
	}
	var list strings.Builder
	for _, f := range LockdownFeatures {
		state := "false"
		if f == "shell_tool" {
			state = "true"
		}
		list.WriteString(f + "  stable  " + state + "\n")
	}
	bin := filepath.Join(t.TempDir(), "codex")
	script := "#!/bin/sh\ncase \"$*\" in\n--version) echo codex-cli 9.9.9;;\n*'features list'*) cat <<'EOF'\n" +
		list.String() + "EOF\n;;\nesac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := RunPreflight(context.Background(), bin, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "shell_tool=true") {
		t.Fatalf("err = %v, want the re-enabled shell_tool named", err)
	}
	if p.LockdownOK || p.Version != "9.9.9" {
		t.Fatalf("preflight = %+v, want lockdown not ok on 9.9.9", p)
	}
}

// A retired lockdown feature passes only by name and only at stage "removed";
// anywhere else — or for any other feature — reported true is a violation.
func TestLockdownRetiredFeatures(t *testing.T) {
	var b strings.Builder
	for _, f := range LockdownFeatures {
		b.WriteString(f + "   stable   false\n")
	}
	base := b.String()
	with := func(name, line string) string {
		return strings.Replace(base, name+"   stable   false\n", line+"\n", 1)
	}
	for _, c := range []struct {
		name, list, want string
	}{
		{"worktrees retired", with("worktrees", "worktrees   removed   true"), ""},
		{"realtime retired", with("realtime_conversation", "realtime_conversation   removed   true"), ""},
		{"worktrees back at stable", with("worktrees", "worktrees   stable   true"), "worktrees=true"},
		{"worktrees back under development", with("worktrees", "worktrees   under development   true"), "worktrees=true"},
		{"worktrees dropped", with("worktrees", ""), "worktrees=missing"},
		{"shell_tool is never retired", with("shell_tool", "shell_tool   removed   true"), "shell_tool=true"},
		{"a two-word stage still reads its value", with("goals", "goals   under development   false"), ""},
	} {
		if got := strings.Join(lockdownViolations(c.list), ","); got != c.want {
			t.Errorf("%s: violations %q, want %q", c.name, got, c.want)
		}
	}
}

// The real `features list` output under the bridge's lockdown argv, captured
// from the qualified release and from 0.166.0, which retired worktrees and
// realtime_conversation (both reported "removed  true").
func TestLockdownAgainstMeasuredReleases(t *testing.T) {
	for _, v := range []string{"0.163.0", "0.166.0"} {
		raw, err := os.ReadFile(filepath.Join("testdata", "features-lockdown-"+v+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		list := string(raw)
		if bad := lockdownViolations(list); len(bad) != 0 {
			t.Errorf("%s: violations %v, want none", v, bad)
		}
		flipped := strings.Replace(list, "\nshell_tool                               stable             false\n",
			"\nshell_tool                               stable             true\n", 1)
		if flipped == list {
			t.Fatalf("%s: fixture has no shell_tool line to flip", v)
		}
		if bad := strings.Join(lockdownViolations(flipped), ","); bad != "shell_tool=true" {
			t.Errorf("%s with shell_tool on: violations %q, want shell_tool=true", v, bad)
		}
	}
}
