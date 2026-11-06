package copilotbridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests drive the bridge-check surface against a real subprocess stub, so
// the framing, the argv and the child handling are exercised — never the live
// CLI, and never a premium request.

const versionStub = "GitHub Copilot CLI 1.0.105."

// preflightStub answers --version and speaks the handshake plus a created (then
// deleted) session. Everything here is what the static tier must accept.
func preflightStub(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`if [ "$1" = "--version" ]; then echo "` + versionStub + `"; exit 0; fi` + "\n")
	emit := func(s string) { fmt.Fprintf(&b, "printf 'Content-Length: %d\\r\\n\\r\\n%%s' '%s'\n", len(s), s) }
	emit(fmt.Sprintf(`{"id":1,"result":{"protocolVersion":%d}}`, channelProtocolVersion))
	emit(`{"id":2,"result":{"sessionId":"s1"}}`)
	emit(`{"id":3,"result":{}}`)
	b.WriteString("cat >/dev/null\n")
	return stubCLI(t, b.String())
}

func TestPreflightAcceptsAWorkingCLI(t *testing.T) {
	bin := preflightStub(t)
	pre, err := RunPreflight(context.Background(), bin, t.TempDir())
	if err != nil {
		t.Fatalf("RunPreflight: %v", err)
	}
	if pre.Version != "1.0.105" {
		t.Errorf("version = %q, want 1.0.105", pre.Version)
	}
	if pre.ProtocolVersion != channelProtocolVersion {
		t.Errorf("protocol = %d, want %d", pre.ProtocolVersion, channelProtocolVersion)
	}
}

// The release that matter most: a channel at another protocol version is the
// undocumented-surface break this check exists for.
func TestPreflightRefusesAForeignChannelProtocol(t *testing.T) {
	var b strings.Builder
	b.WriteString(`if [ "$1" = "--version" ]; then echo "` + versionStub + `"; exit 0; fi` + "\n")
	emit := func(s string) { fmt.Fprintf(&b, "printf 'Content-Length: %d\\r\\n\\r\\n%%s' '%s'\n", len(s), s) }
	emit(`{"id":1,"result":{"protocolVersion":99}}`)
	b.WriteString("cat >/dev/null\n")
	bin := stubCLI(t, b.String())

	_, err := RunPreflight(context.Background(), bin, t.TempDir())
	if err == nil {
		t.Fatal("a protocol-99 channel passed the preflight")
	}
	if !strings.Contains(err.Error(), "protocol 99") {
		t.Errorf("err = %v, want it to name the protocol it found", err)
	}
}

// A refused session is a login that does not work, and it must be reported as
// that — not as a broken channel.
func TestPreflightReportsALoginThatCannotCreateASession(t *testing.T) {
	var b strings.Builder
	b.WriteString(`if [ "$1" = "--version" ]; then echo "` + versionStub + `"; exit 0; fi` + "\n")
	emit := func(s string) { fmt.Fprintf(&b, "printf 'Content-Length: %d\\r\\n\\r\\n%%s' '%s'\n", len(s), s) }
	emit(fmt.Sprintf(`{"id":1,"result":{"protocolVersion":%d}}`, channelProtocolVersion))
	emit(`{"id":2,"error":{"code":-32000,"message":"Not logged in. Run copilot login."}}`)
	b.WriteString("cat >/dev/null\n")
	bin := stubCLI(t, b.String())

	_, err := RunPreflight(context.Background(), bin, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "login") {
		t.Fatalf("err = %v, want a login problem", err)
	}
}

func TestPreflightRefusesAMissingBinary(t *testing.T) {
	if _, err := RunPreflight(context.Background(), filepath.Join(t.TempDir(), "nope"), t.TempDir()); err == nil {
		t.Fatal("a missing binary passed the preflight")
	}
}

// liveStub answers each turn from a state file: the first child reports a tool
// batch, later children report text — which is the shape of a real tool loop.
func liveStub(t *testing.T, toolNames []string) string {
	t.Helper()
	dir := t.TempDir()
	count := filepath.Join(dir, "n")
	var b strings.Builder
	b.WriteString(`if [ "$1" = "--version" ]; then echo "` + versionStub + `"; exit 0; fi` + "\n")
	b.WriteString(`n=$(cat ` + count + ` 2>/dev/null || echo 0); echo $((n+1)) > ` + count + "\n")
	emit := func(s string) { fmt.Fprintf(&b, "printf 'Content-Length: %d\\r\\n\\r\\n%%s' '%s'\n", len(s), s) }
	emit(fmt.Sprintf(`{"id":1,"result":{"protocolVersion":%d}}`, channelProtocolVersion))
	emit(`{"id":2,"result":{"sessionId":"s1"}}`)
	emit(`{"id":3,"result":{"messageId":"m1"}}`)

	var calls []string
	for i, n := range toolNames {
		calls = append(calls, fmt.Sprintf(`{"name":%q,"toolCallId":"call_%d","arguments":{"city":"Kuala Lumpur"}}`, n, i))
	}
	batched := `{"type":"assistant.message","data":{"content":"","toolRequests":[` + strings.Join(calls, ",") + `]}}`
	answered := `{"type":"assistant.message","data":{"content":"It is 31 degrees in Kuala Lumpur.","model":"gpt-6.1-sol"}}`
	b.WriteString(`if [ "$n" = "0" ]; then` + "\n")
	emit(`{"method":"session.event","params":{"sessionId":"s1","event":` + batched + `}}`)
	b.WriteString(`else` + "\n")
	emit(`{"method":"session.event","params":{"sessionId":"s1","event":` + answered + `}}`)
	b.WriteString("fi\n")
	emit(`{"method":"session.event","params":{"sessionId":"s1","event":{"type":"session.usage_checkpoint","data":{"totalNanoAiu":960000.0,"totalPremiumRequests":1,"accountingSnapshot":{"totalPremiumRequestCost":1.0,"tokenDetails":{"input":{"tokenCount":71},"cache_read":{"tokenCount":0},"cache_write":{"tokenCount":0},"output":{"tokenCount":5}}}}}}}`)
	emit(`{"method":"session.event","params":{"sessionId":"s1","event":{"type":"session.idle","data":{}}}}`)
	b.WriteString("cat >/dev/null\n")
	return stubCLI(t, b.String())
}

func liveRunner(t *testing.T, bin string) *Runner {
	t.Helper()
	return &Runner{Bin: bin, Home: t.TempDir(), Timeout: 20 * time.Second}
}

func TestLiveToolsAcceptsARealBatch(t *testing.T) {
	bin := liveStub(t, []string{"weather", "clock"})
	if err := LiveTools(context.Background(), liveRunner(t, bin), "auto"); err != nil {
		t.Fatalf("LiveTools: %v", err)
	}
}

// An undeclared tool coming back is exactly the failure the channel has to make
// impossible: the CLI's own tools must never reach a caller.
func TestLiveToolsRefusesAnUndeclaredTool(t *testing.T) {
	bin := liveStub(t, []string{"bash"})
	err := LiveTools(context.Background(), liveRunner(t, bin), "auto")
	if err == nil || !strings.Contains(err.Error(), "never declared") {
		t.Fatalf("err = %v, want an undeclared-tool refusal", err)
	}
}

// A model that calls nothing leaves the batch path unproven, and the check must
// say so rather than pass on the replay turn.
func TestLiveToolsRefusesAnEmptyBatch(t *testing.T) {
	bin := liveStub(t, nil)
	err := LiveTools(context.Background(), liveRunner(t, bin), "auto")
	if err == nil || !strings.Contains(err.Error(), "asked for no tool") {
		t.Fatalf("err = %v, want an empty-batch refusal", err)
	}
}

func TestLivePlainAcceptsAPongAndUsage(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString(`if [ "$1" = "--version" ]; then echo "` + versionStub + `"; exit 0; fi` + "\n")
	emit := func(s string) { fmt.Fprintf(&b, "printf 'Content-Length: %d\\r\\n\\r\\n%%s' '%s'\n", len(s), s) }
	emit(fmt.Sprintf(`{"id":1,"result":{"protocolVersion":%d}}`, channelProtocolVersion))
	emit(`{"id":2,"result":{"sessionId":"s1"}}`)
	emit(`{"id":3,"result":{"messageId":"m1"}}`)
	emit(`{"method":"session.event","params":{"sessionId":"s1","event":{"type":"assistant.message","data":{"content":"pong"}}}}`)
	emit(`{"method":"session.event","params":{"sessionId":"s1","event":{"type":"session.usage_checkpoint","data":{"totalNanoAiu":960000.0,"totalPremiumRequests":1,"accountingSnapshot":{"totalPremiumRequestCost":1.0,"tokenDetails":{"input":{"tokenCount":9},"cache_read":{"tokenCount":0},"cache_write":{"tokenCount":0},"output":{"tokenCount":1}}}}}}}`)
	emit(`{"method":"session.event","params":{"sessionId":"s1","event":{"type":"session.idle","data":{}}}}`)
	b.WriteString("cat >/dev/null\n")
	bin := stubCLI(t, b.String())
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	if err := LivePlain(context.Background(), liveRunner(t, bin), "auto"); err != nil {
		t.Fatalf("LivePlain: %v", err)
	}
}
