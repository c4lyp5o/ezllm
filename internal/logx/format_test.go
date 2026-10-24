package logx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// The console format is the default; JSON is opt-in for a collector.
func TestJSONFormatEmitsOneObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewHandler(&buf, Options{Level: slog.LevelInfo, JSON: true}))
	log.Info("request", "status", 200, "model", "gpt-6-luna")

	out := strings.TrimSuffix(buf.String(), "\n")
	if strings.Contains(out, "\n") {
		t.Fatalf("one record must be one line: %q", out)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("not JSON: %q (%v)", out, err)
	}
	for k, want := range map[string]any{"msg": "request", "status": float64(200), "model": "gpt-6-luna", "level": "INFO"} {
		if rec[k] != want {
			t.Fatalf("%s = %v, want %v (%v)", k, rec[k], want, rec)
		}
	}
	if _, ok := rec["time"]; !ok {
		t.Fatalf("no time field: %v", rec)
	}
}

// Console stays the default: an empty option must not switch a human's log to
// JSON.
func TestConsoleIsTheDefaultFormat(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewHandler(&buf, Options{Level: slog.LevelInfo}))
	log.Info("listening", "addr", "127.0.0.1:1")
	if strings.HasPrefix(strings.TrimSpace(buf.String()), "{") {
		t.Fatalf("default format emitted JSON: %q", buf.String())
	}
	if !strings.Contains(buf.String(), " INFO  listening addr=127.0.0.1:1") {
		t.Fatalf("console line = %q", buf.String())
	}
}

func TestParseFormat(t *testing.T) {
	for _, s := range []string{"json", "JSON", " json "} {
		if !ParseFormat(s) {
			t.Fatalf("%q must select json", s)
		}
	}
	for _, s := range []string{"", "pretty", "console", "text", "jsn"} {
		if ParseFormat(s) {
			t.Fatalf("%q must keep the console format", s)
		}
	}
}

func TestLevelName(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug, "info": slog.LevelInfo,
		"warn": slog.LevelWarn, "error": slog.LevelError,
	}
	for want, l := range cases {
		if got := LevelName(l); got != want {
			t.Fatalf("LevelName(%v) = %q, want %q", l, got, want)
		}
	}
	// Values above error still render as error, never as something unparseable.
	if got := LevelName(slog.LevelError + 4); got != "error" {
		t.Fatalf("LevelName(error+4) = %q", got)
	}
}
