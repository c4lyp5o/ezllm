package codexbridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Every child is writable on the app-server channel; there is no read-only
// mode to fall back to.
func TestWritableSandboxReachesThreadStart(t *testing.T) {
	r, home := fakeRunner(t, "ok")
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

// The writable policy also reaches the exec fallback argv.
func TestWritableSandboxReachesExecArgv(t *testing.T) {
	r, home := fakeRunner(t, "ok")
	r.Channel = ChannelExec
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

// The native CLI configuration must be noninteractive before any thread exists,
// including resumed templates and the exec fallback.
func TestNeverApproveAppliesToCLIAndResumedTemplates(t *testing.T) {
	{
		r, home := fakeRunner(t, "ok")
		r.ForkCache = true
		for i := 0; i < 2; i++ {
			if _, err := r.Run(context.Background(), simpleRequest(), nil); err != nil {
				t.Fatal(err)
			}
			args, _, _, _ := readStarted(t, home)
			if index := slices.Index(args, `approval_policy="never"`); index < 1 || args[index-1] != "-c" {
				t.Fatalf("interactive child argv: %v", args)
			}
		}
		raw, err := os.ReadFile(filepath.Join(home, "cache_rpc.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		resumed := false
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var call struct {
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if json.Unmarshal([]byte(line), &call) != nil {
				t.Fatal(line)
			}
			if call.Method == "thread/resume" {
				resumed = true
				if call.Params["approvalPolicy"] != "never" {
					t.Fatal("resumed template can ask for approvals")
				}
			}
			if call.Method == "thread/fork" && call.Params["experimentalPredictionMode"] != true {
				t.Fatal("fork did not inherit template configuration")
			}
		}
		if !resumed {
			t.Fatal("resume path not exercised")
		}
		r.Shutdown()
		waitTemplatesDeleted(t, r)
	}
}
