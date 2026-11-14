package codexbridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The writable mode is an explicit operator opt-in: anything that is not
// exactly EZLLM_CODEX_SANDBOX=writable reads back as read-only.
func TestSandboxModeFromEnvFailClosed(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  SandboxMode
	}{
		{"unset", "", SandboxReadOnly},
		{"writable", "writable", SandboxWritable},
		{"case-insensitive with spaces", "  WRITABLE ", SandboxWritable},
		{"read-only stays read-only", "read-only", SandboxReadOnly},
		{"unknown value fails closed", "danger-full-access", SandboxReadOnly},
		{"empty-ish fails closed", "yes", SandboxReadOnly},
	}
	for _, tc := range cases {
		if got := SandboxModeFromEnv(func(string) string { return tc.value }); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Default (zero-value) runner stays read-only on both channels.
func TestDefaultSandboxModeIsReadOnly(t *testing.T) {
	r, home := fakeRunner(t, "ok")
	if _, err := r.Run(context.Background(), simpleRequest(), nil); err != nil {
		t.Fatal(err)
	}
	var ts map[string]any
	b, _ := os.ReadFile(filepath.Join(home, "thread_start.json"))
	_ = json.Unmarshal(b, &ts)
	if ts["sandbox"] != "read-only" || ts["approvalPolicy"] != "never" {
		t.Fatalf("default thread/start = %v, want read-only/never", ts)
	}
}

// Writable mode flows through to thread/start on the app-server channel.
func TestWritableSandboxModeReachesThreadStart(t *testing.T) {
	r, home := fakeRunner(t, "ok")
	r.SandboxMode = SandboxWritable
	if _, err := r.Run(context.Background(), simpleRequest(), nil); err != nil {
		t.Fatal(err)
	}
	var ts map[string]any
	b, _ := os.ReadFile(filepath.Join(home, "thread_start.json"))
	_ = json.Unmarshal(b, &ts)
	if ts["sandbox"] != "workspace-write" || ts["approvalPolicy"] != "never" {
		t.Fatalf("writable thread/start = %v, want workspace-write/never", ts)
	}
}

// Writable mode flows through to the exec fallback argv.
func TestWritableSandboxModeReachesExecArgv(t *testing.T) {
	r, home := fakeRunner(t, "ok")
	r.Channel = ChannelExec
	r.SandboxMode = SandboxWritable
	if _, err := r.Run(context.Background(), simpleRequest(), nil); err != nil {
		t.Fatal(err)
	}
	args, _ := readExec(t, home)
	if !slices.Contains(args, "workspace-write") {
		t.Errorf("writable exec argv lacks workspace-write: %v", args)
	}
	if i := slices.Index(args, `approval_policy="never"`); i < 1 || args[i-1] != "-c" {
		t.Errorf("writable exec argv lacks headless approval config: %v", args)
	}
	if slices.Contains(args, "--ask-for-approval") {
		t.Errorf("exec does not accept --ask-for-approval after the subcommand: %v", args)
	}
	if slices.Contains(args, "read-only") {
		t.Errorf("writable exec argv still carries read-only: %v", args)
	}
}
