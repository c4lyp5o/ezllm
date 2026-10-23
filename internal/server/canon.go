package server

import (
	"encoding/json"
	"io"
	"sort"
	"unicode/utf8"
)

// canonEncoder streams v as the exact bytes json.Marshal would produce —
// sorted map keys, HTML-safe string escaping, number literals verbatim — so
// shape capture can hash a request's history without first building a full
// serialized copy of it. Bytes are appended to buf (as encoding/json does)
// and handed to w in chunks of about canonChunk. It handles the value types
// the shared decoder yields (map[string]any, []any, string, json.Number, bool,
// nil); encode reports false for anything else and the caller falls back to
// json.Marshal. Byte equality with json.Marshal is pinned by
// TestCanonicalMatchesMarshal and FuzzCanonicalMatchesMarshal.
type canonEncoder struct {
	w   io.Writer
	buf []byte
}

const canonChunk = 32 << 10

// encode writes v and flushes what remains buffered.
func (e *canonEncoder) encode(v any) bool {
	if !e.value(v) {
		return false
	}
	_, err := e.w.Write(e.buf)
	e.buf = e.buf[:0]
	return err == nil
}

func (e *canonEncoder) spill() {
	if len(e.buf) >= canonChunk {
		e.w.Write(e.buf)
		e.buf = e.buf[:0]
	}
}

func (e *canonEncoder) value(v any) bool {
	switch x := v.(type) {
	case nil:
		e.buf = append(e.buf, "null"...)
	case bool:
		if x {
			e.buf = append(e.buf, "true"...)
		} else {
			e.buf = append(e.buf, "false"...)
		}
	case string:
		e.str(x)
	case json.Number:
		if x == "" {
			e.buf = append(e.buf, '0') // json.Marshal's encoding of an empty Number
		} else {
			e.buf = append(e.buf, x...)
		}
	case []any:
		e.buf = append(e.buf, '[')
		for i, el := range x {
			if i > 0 {
				e.buf = append(e.buf, ',')
			}
			if !e.value(el) {
				return false
			}
		}
		e.buf = append(e.buf, ']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		e.buf = append(e.buf, '{')
		for i, k := range keys {
			if i > 0 {
				e.buf = append(e.buf, ',')
			}
			e.str(k)
			e.buf = append(e.buf, ':')
			if !e.value(x[k]) {
				return false
			}
		}
		e.buf = append(e.buf, '}')
	default:
		return false
	}
	e.spill()
	return true
}

const lowerHex = "0123456789abcdef"

// str is encoding/json's string encoder with HTML escaping on (json.Marshal's
// default): runs of safe bytes are appended in one copy.
func (e *canonEncoder) str(s string) {
	e.buf = append(e.buf, '"')
	start := 0
	for i := 0; i < len(s); {
		if b := s[i]; b < utf8.RuneSelf {
			if b >= 0x20 && b != '"' && b != '\\' && b != '<' && b != '>' && b != '&' {
				i++
				continue
			}
			e.buf = append(e.buf, s[start:i]...)
			switch b {
			case '\\', '"':
				e.buf = append(e.buf, '\\', b)
			case '\b':
				e.buf = append(e.buf, '\\', 'b')
			case '\f':
				e.buf = append(e.buf, '\\', 'f')
			case '\n':
				e.buf = append(e.buf, '\\', 'n')
			case '\r':
				e.buf = append(e.buf, '\\', 'r')
			case '\t':
				e.buf = append(e.buf, '\\', 't')
			default: // other control bytes and <, >, &
				e.buf = append(e.buf, '\\', 'u', '0', '0', lowerHex[b>>4], lowerHex[b&0xF])
			}
			i++
			start = i
			continue
		}
		c, size := utf8.DecodeRuneInString(s[i:])
		if c == utf8.RuneError && size == 1 {
			// Invalid UTF-8 becomes a RAW U+FFFD, as json.Marshal does on
			// this toolchain (Go 1.27; older releases wrote the \ufffd
			// escape). The differential test pins whichever is current.
			e.buf = append(e.buf, s[start:i]...)
			e.buf = append(e.buf, "\ufffd"...)
			i += size
			start = i
			continue
		}
		if c == '\u2028' || c == '\u2029' {
			e.buf = append(e.buf, s[start:i]...)
			e.buf = append(e.buf, '\\', 'u', '2', '0', '2', lowerHex[c&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	e.buf = append(e.buf, s[start:]...)
	e.buf = append(e.buf, '"')
}
