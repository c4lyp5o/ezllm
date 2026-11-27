package logx

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// A message carrying upstream text cannot forge a second log line or repaint
// the terminal.
func TestMessageControlCharsEscaped(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewHandler(&buf, Options{}))
	log.Info("claude-bridge: stream failed: boom\nINFO fake line \x1b[31mred")
	out := buf.String()
	if strings.Count(out, "\n") != 1 || strings.Contains(out, "\x1b") {
		t.Fatalf("log output %q has a raw newline or escape", out)
	}
	if !strings.Contains(out, `boom\nINFO fake line \x1b[31mred`) {
		t.Fatalf("log output %q, want the escaped text", out)
	}
	if got := safeMessage("plain message"); got != "plain message" {
		t.Fatalf("plain message changed: %q", got)
	}
	if n := testing.AllocsPerRun(100, func() { _ = safeMessage("plain message") }); n != 0 {
		t.Fatalf("plain message allocates %v times", n)
	}
}
