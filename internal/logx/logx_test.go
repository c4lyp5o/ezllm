package logx

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHandlerPlainLine(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewHandler(&buf, Options{Level: slog.LevelInfo}))
	log.With("svc", "ezllm").WithGroup("req").Info("listening", "addr", "127.0.0.1:20129", "note", "two words", "err", errors.New("boom"))
	line := buf.String()
	for _, want := range []string{" INFO  listening ", " svc=ezllm", " req.addr=127.0.0.1:20129", ` req.note="two words"`, " req.err=boom"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line %q missing %q", line, want)
		}
	}
	if strings.Contains(line, "\x1b[") {
		t.Fatalf("color off must emit no escapes: %q", line)
	}
	if !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
		t.Fatalf("want exactly one line: %q", line)
	}
}

func TestHandlerLevelFilterAndColor(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewHandler(&buf, Options{Level: slog.LevelWarn, Color: true}))
	log.Info("hidden")
	log.Debug("hidden too")
	log.Warn("shown")
	log.Error("bad")
	out := buf.String()
	if strings.Contains(out, "hidden") {
		t.Fatalf("records below the level leaked: %q", out)
	}
	if !strings.Contains(out, bold+yellow+"WARN "+reset) || !strings.Contains(out, bold+red+"ERROR"+reset) {
		t.Fatalf("level labels not colored: %q", out)
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"": slog.LevelInfo, "INFO": slog.LevelInfo, "debug": slog.LevelDebug, " warn ": slog.LevelWarn, "warning": slog.LevelWarn, "error": slog.LevelError} {
		if got, ok := ParseLevel(in); !ok || got != want {
			t.Fatalf("ParseLevel(%q) = %v %v, want %v", in, got, ok, want)
		}
	}
	if _, ok := ParseLevel("verbose"); ok {
		t.Fatal("unknown level must report ok=false")
	}
}

func TestColorEnabledModes(t *testing.T) {
	if !ColorEnabled("always", nil) || ColorEnabled("never", nil) {
		t.Fatal("explicit modes must win")
	}
	t.Setenv("NO_COLOR", "1")
	if ColorEnabled("auto", nil) {
		t.Fatal("NO_COLOR must disable auto color")
	}
}

func TestFirstBootBannerCarriesCode(t *testing.T) {
	var buf bytes.Buffer
	FirstBootBanner(&buf, false, "http://127.0.0.1:20129", "ABCD-EFGH")
	out := buf.String()
	if !strings.Contains(out, "A B C D - E F G H") || !strings.Contains(out, "http://127.0.0.1:20129") || !strings.Contains(out, "EZLLM_DASHBOARD_PASSWORD") {
		t.Fatalf("banner missing code, url or env hint:\n%s", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatal("plain banner must emit no escapes")
	}
	assertPanelAligned(t, out)
}

func TestColoredPanelsStayAligned(t *testing.T) {
	var buf bytes.Buffer
	FirstBootBanner(&buf, true, "http://127.0.0.1:20129", "ABCD-EFGH")
	out := buf.String()
	if !strings.Contains(out, codeStyle+" A B C D - E F G H "+reset) {
		t.Fatalf("code not highlighted:\n%q", out)
	}
	// Escapes must not count toward width: strip them and the frame is square.
	assertPanelAligned(t, stripANSI(out))
}

// assertPanelAligned checks every framed row has the same rune width as its
// top border, so the right edge is straight.
func assertPanelAligned(t *testing.T, out string) {
	t.Helper()
	width := -1
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(l, "╔") && !strings.HasPrefix(l, "║") && !strings.HasPrefix(l, "╚") {
			width = -1
			continue
		}
		n := utf8.RuneCountInString(l)
		if strings.HasPrefix(l, "╔") {
			width = n
			continue
		}
		if n != width {
			t.Fatalf("ragged panel row (%d runes, border %d): %q\n%s", n, width, l, out)
		}
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
