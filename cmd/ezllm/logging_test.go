package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/logx"
)

// The boot line has to survive the format the operator chose. In JSON, a field
// named "level" would collide with slog's own level and a parser would read
// whichever came last — the found bug this test exists for.
func TestAnnounceLoggingJSONHasOneLevelKey(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(logx.NewHandler(&buf, logx.Options{Level: slog.LevelDebug, JSON: true}))
	announceLogging(log, slog.LevelDebug, true, false)

	out := strings.TrimSuffix(buf.String(), "\n")
	var rec map[string]any
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("not JSON: %q (%v)", out, err)
	}
	if n := strings.Count(out, `"level"`); n != 1 {
		t.Fatalf("want exactly one level key, got %d: %s", n, out)
	}
	// The record's own level is INFO while the CONFIGURED level is debug: two
	// different facts, which is exactly why they cannot share a key.
	if rec["level"] != "INFO" {
		t.Fatalf("level = %v, want slog's own INFO for this record", rec["level"])
	}
	if rec["log_level"] != "debug" || rec["format"] != "json" {
		t.Fatalf("boot record lost its fields: %v", rec)
	}
}

// In the console format the same record reads as one greppable line.
func TestAnnounceLoggingConsoleLine(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(logx.NewHandler(&buf, logx.Options{Level: slog.LevelInfo}))
	announceLogging(log, slog.LevelWarn, false, false)

	line := buf.String()
	for _, want := range []string{" INFO  console logging ", "log_level=warn", "format=console", "color=false"} {
		if !strings.Contains(line, want) {
			t.Fatalf("boot line %q is missing %q", line, want)
		}
	}
}
