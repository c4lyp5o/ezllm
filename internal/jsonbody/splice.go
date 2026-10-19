package jsonbody

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"
)

// SpliceModel returns raw with the top-level "model" value replaced by
// newModel (inserted first when absent), copying every other byte as the
// client sent it. It is the hop swap's fast path: a json.Marshal of the
// decoded map costs ~2.6 ms and ~1 MiB of garbage per hop at 1 MiB, while
// this is one forward scan that jumps between structural bytes.
//
// raw's first JSON value must already be known to be an object (the caller
// decoded it). The output is that object alone: leading whitespace and
// anything after it are dropped, as the re-marshal dropped them.
//
// ok=false asks the caller to fall back to the re-marshal, which normalizes
// what a splice would pass through:
//   - invalid UTF-8 (json.Marshal replaces it with U+FFFD);
//   - a duplicated TOP-LEVEL key: we read the last one (model, stream), and an
//     upstream that reads the first would disagree with the routing decision.
//     The re-marshal collapses duplicates; nested ones carry no routing fact
//     and are passed through.
func SpliceModel(raw []byte, newModel string) (out []byte, ok bool) {
	if !utf8.Valid(raw) {
		return nil, false
	}
	enc, err := json.Marshal(newModel)
	if err != nil {
		return nil, false
	}
	i := skipWS(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return nil, false
	}
	start := i
	i = skipWS(raw, i+1)
	seen := map[string]struct{}{}
	vs, ve := -1, -1 // span of the model value
	members := 0
	for {
		if i >= len(raw) {
			return nil, false
		}
		if raw[i] == '}' {
			break
		}
		if members > 0 {
			if raw[i] != ',' {
				return nil, false
			}
			i = skipWS(raw, i+1)
		}
		if i >= len(raw) || raw[i] != '"' {
			return nil, false
		}
		ke := skipString(raw, i)
		if ke < 0 {
			return nil, false
		}
		key, kok := keyText(raw[i:ke])
		if !kok {
			return nil, false
		}
		if _, dup := seen[key]; dup {
			return nil, false
		}
		seen[key] = struct{}{}
		i = skipWS(raw, ke)
		if i >= len(raw) || raw[i] != ':' {
			return nil, false
		}
		i = skipWS(raw, i+1)
		end := skipValue(raw, i)
		if end < 0 {
			return nil, false
		}
		if key == "model" {
			vs, ve = i, end
		}
		members++
		i = skipWS(raw, end)
	}
	objEnd := i // index of the closing brace

	var b bytes.Buffer
	if vs >= 0 {
		b.Grow(objEnd + 1 - start - (ve - vs) + len(enc))
		b.Write(raw[start:vs])
		b.Write(enc)
		b.Write(raw[ve : objEnd+1])
		return b.Bytes(), true
	}
	b.Grow(objEnd + 1 - start + len(enc) + 10)
	b.WriteString(`{"model":`)
	b.Write(enc)
	if members > 0 {
		b.WriteByte(',')
		b.Write(raw[start+1 : objEnd+1])
	} else {
		b.WriteByte('}')
	}
	return b.Bytes(), true
}

func skipWS(raw []byte, i int) int {
	for i < len(raw) {
		switch raw[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

// skipString returns the index just past the string starting at raw[i]=='"',
// or -1 when it is unterminated. It jumps quote to quote with IndexByte (the
// vectorized search; strings are most of a body) and treats a quote as
// closing when an even number of backslashes precedes it.
func skipString(raw []byte, i int) int {
	j := i + 1
	for {
		k := bytes.IndexByte(raw[j:], '"')
		if k < 0 {
			return -1
		}
		q := j + k
		n := 0
		for b := q - 1; b > i && raw[b] == '\\'; b-- {
			n++
		}
		if n%2 == 0 {
			return q + 1
		}
		j = q + 1
	}
}

// skipValue returns the index just past the value starting at raw[i], or -1.
func skipValue(raw []byte, i int) int {
	if i >= len(raw) {
		return -1
	}
	switch raw[i] {
	case '"':
		return skipString(raw, i)
	case '{', '[':
		// Outside strings, structural bytes are a few apart, so a plain loop
		// beats a per-call IndexAny; strings are skipped wholesale.
		depth := 0
		for j := i; j < len(raw); {
			switch raw[j] {
			case '"':
				if j = skipString(raw, j); j < 0 {
					return -1
				}
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1
				}
			}
			j++
		}
		return -1
	default: // number, true, false, null
		j := i
		for j < len(raw) {
			switch raw[j] {
			case ',', '}', ']', ' ', '\t', '\r', '\n':
				return j
			}
			j++
		}
		return j
	}
}

// keyText decodes a quoted key. Escapes are rare in keys, but "model" IS
// "model" to the decoder, so it must be to the splice too.
func keyText(quoted []byte) (string, bool) {
	if bytes.IndexByte(quoted, '\\') < 0 {
		return string(quoted[1 : len(quoted)-1]), true
	}
	var s string
	if err := json.Unmarshal(quoted, &s); err != nil {
		return "", false
	}
	return s, true
}
