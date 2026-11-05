package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// The bridge logs with printf-style arguments; handing it log.Info directly
// printed "session=%s ... !BADKEY=" for every turn.
func TestPrintfLoggerFormatsInsteadOfBadKeys(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	printfLogger(log)("copilot-bridge: turn ok session=%s in=%d", "sess-1", 42)
	out := buf.String()
	if !strings.Contains(out, "turn ok session=sess-1 in=42") || strings.Contains(out, "BADKEY") {
		t.Errorf("log line = %q", out)
	}
}
