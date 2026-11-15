package logx

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Log timestamps are written in the usage timezone set by SetLocation, in
// both formats, and fall back to UTC; the process TZ plays no part.
func TestLogTimestampsFollowSetLocation(t *testing.T) {
	t.Cleanup(func() { SetLocation(nil) })
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	emit := func(json bool) string {
		var buf bytes.Buffer
		h := NewHandler(&buf, Options{JSON: json})
		if err := h.Handle(context.Background(), slog.NewRecord(at, slog.LevelInfo, "m", 0)); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	SetLocation(nil)
	if got := emit(false); !strings.HasPrefix(got, "2026-11-15 12:00:00.000") {
		t.Fatalf("UTC console line = %q", got)
	}
	kl, err := time.LoadLocation("Asia/Kuala_Lumpur")
	if err != nil {
		t.Skip("no tz database")
	}
	SetLocation(kl)
	if got := emit(false); !strings.HasPrefix(got, "2026-11-15 20:00:00.000") {
		t.Fatalf("usage-timezone console line = %q", got)
	}
	if got := emit(true); !strings.Contains(got, `"time":"2026-11-15T20:00:00+08:00"`) {
		t.Fatalf("usage-timezone JSON line = %q", got)
	}
}
