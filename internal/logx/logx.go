// Package logx is ezllm's console log format: one line per record,
//
//	2026-10-21 14:03:05.123 INFO  listening addr=127.0.0.1:20129
//
// with the level colored when the output is a terminal. It is a plain
// slog.Handler, so every call site keeps using log/slog.
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ANSI SGR sequences. Kept to the 16-color set every terminal renders.
const (
	reset   = "\x1b[0m"
	bold    = "\x1b[1m"
	dim     = "\x1b[2m"
	red     = "\x1b[31m"
	green   = "\x1b[32m"
	yellow  = "\x1b[33m"
	magenta = "\x1b[35m"
	cyan    = "\x1b[36m"
)

const timeFormat = "2006-01-02 15:04:05.000"

// ParseLevel reads EZLLM_LOG_LEVEL's vocabulary. Empty means info; an
// unknown value reports ok=false so the caller can say so instead of
// silently logging at a level nobody asked for.
func ParseLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, true
	case "debug":
		return slog.LevelDebug, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return slog.LevelInfo, false
}

// ColorEnabled decides whether output to f is colored. mode is
// EZLLM_LOG_COLOR: "always", "never", or auto (anything else), where auto
// colors only a terminal and honors NO_COLOR (https://no-color.org). Docker
// logs without -t are not a terminal, so they stay plain.
func ColorEnabled(mode string, f *os.File) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "always":
		return true
	case "never":
		return false
	}
	if os.Getenv("NO_COLOR") != "" || f == nil {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Options configures NewHandler.
type Options struct {
	Level slog.Leveler // nil = info
	Color bool
}

// NewHandler returns a handler writing the console format to w. Safe for
// concurrent use; each record is one Write.
func NewHandler(w io.Writer, opts Options) slog.Handler {
	lv := opts.Level
	if lv == nil {
		lv = slog.LevelInfo
	}
	return &handler{mu: &sync.Mutex{}, w: w, level: lv, color: opts.Color}
}

type handler struct {
	mu    *sync.Mutex // shared by every derived handler: one writer
	w     io.Writer
	level slog.Leveler
	color bool
	attrs []byte // pre-rendered WithAttrs output
	group string // "a.b." prefix from WithGroup
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level.Level() }

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	buf := make([]byte, 0, 256)
	if !r.Time.IsZero() {
		buf = h.paint(buf, dim, r.Time.Format(timeFormat))
		buf = append(buf, ' ')
	}
	buf = h.paint(buf, levelColor(r.Level), fmt.Sprintf("%-5s", r.Level.String()))
	buf = append(buf, ' ')
	if r.Level >= slog.LevelError {
		buf = h.paint(buf, bold, r.Message)
	} else {
		buf = append(buf, r.Message...)
	}
	buf = append(buf, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		buf = h.appendAttr(buf, h.group, a)
		return true
	})
	buf = append(buf, '\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.w.Write(buf)
	return err
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	h2 := *h
	h2.attrs = append([]byte(nil), h.attrs...)
	for _, a := range attrs {
		h2.attrs = h.appendAttr(h2.attrs, h.group, a)
	}
	return &h2
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h2 := *h
	h2.group = h.group + name + "."
	return &h2
}

func (h *handler) appendAttr(buf []byte, prefix string, a slog.Attr) []byte {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return buf
	}
	if a.Value.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, ga := range a.Value.Group() {
			buf = h.appendAttr(buf, p, ga)
		}
		return buf
	}
	buf = append(buf, ' ')
	buf = h.paint(buf, cyan, prefix+a.Key+"=")
	v := formatValue(a.Value)
	if a.Key == "err" || a.Key == "error" {
		return h.paint(buf, red, v)
	}
	return append(buf, v...)
}

func (h *handler) paint(buf []byte, code, s string) []byte {
	if !h.color {
		return append(buf, s...)
	}
	buf = append(buf, code...)
	buf = append(buf, s...)
	return append(buf, reset...)
}

func levelColor(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return bold + red
	case l >= slog.LevelWarn:
		return bold + yellow
	case l >= slog.LevelInfo:
		return green
	default:
		return magenta
	}
}

func formatValue(v slog.Value) string {
	var s string
	switch v.Kind() {
	case slog.KindString:
		s = v.String()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339Nano)
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			s = err.Error()
		} else {
			s = v.String()
		}
	default:
		return v.String()
	}
	if needsQuote(s) {
		return strconv.Quote(s)
	}
	return s
}

// needsQuote matches slog.TextHandler's rule closely enough that values
// stay unambiguous: empty, spaces, '=', quotes or anything non-printable.
func needsQuote(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		if unicode.IsSpace(r) || r == '"' || r == '=' || !unicode.IsPrint(r) {
			return true
		}
	}
	return false
}
