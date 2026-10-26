// Package geminiweb speaks Google Gemini's WEB protocol
// (gemini.google.com) — the reverse-engineered surface documented in
// docs/gemini-web-design.md and ~/workspace/gemini-web-api-protocol-digest.md.
//
// M12 scope is deliberately narrow: cookie auth, session init,
// GET_USER_STATUS (auth oracle + catalog), and streaming text generation.
// No function calling, no structured output, no images — those are typed
// refusals above this package, not half-protocols inside it.
package geminiweb

import (
	"bytes"
	"errors"
	"strconv"
	"unicode/utf8"
)

// errMalformedFrame means the byte stream is not wrb.fr framing (bad or
// oversized length prefix, or a frame that cannot close on a rune boundary).
var errMalformedFrame = errors.New("geminiweb: malformed wrb.fr frame")

// maxFrameUnits bounds a single declared frame length. A frame beyond this
// is treated as corruption rather than buffered — a hostile or desynced
// stream must not grow the parser's buffer without limit.
const maxFrameUnits = 8 << 20

// FrameParser incrementally reassembles wrb.fr frames from arbitrary chunks.
//
// Wire format (digest §4.1): each frame is
//
//	<decimal-length-in-UTF-16-code-units>\n<payload bytes>
//
// The length counts UTF-16 code units (astral runes count 2), the payload
// is a JSPB JSON array, and several frames concatenate within one HTTP
// chunk. Boundaries can split at any byte — including mid-rune — so the
// parser buffers until a frame is complete.
//
// The unit counter resumes where the last Feed stopped: `scanned` is how
// many payload bytes have already been counted, so a chunk boundary never
// re-counts a rune (double counting desyncs every later frame).
type FrameParser struct {
	buf []byte

	havePrefix bool
	expected   int // declared length in UTF-16 code units
	units      int // UTF-16 units counted so far for the current frame
	scanned    int // payload bytes counted so far for the current frame
}

// Feed appends chunk to the buffer and returns every payload frame that is
// now complete, in order. A returned error means the stream is malformed;
// the caller decides whether the session dies or the frame is skipped.
func (p *FrameParser) Feed(chunk []byte) ([]string, error) {
	p.buf = append(p.buf, chunk...)
	var out []string
	for {
		frame, ok, err := p.tryFrame()
		if err != nil {
			return out, err
		}
		if !ok {
			return out, nil
		}
		out = append(out, frame)
	}
}

// tryFrame consumes one complete frame from the head of the buffer.
// ok=false means "need more bytes" (neither error nor frame).
func (p *FrameParser) tryFrame() (string, bool, error) {
	if !p.havePrefix {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			if len(p.buf) > 16 { // no sane decimal prefix is this long
				return "", false, errMalformedFrame
			}
			return "", false, nil
		}
		length, err := strconv.Atoi(string(p.buf[:i]))
		if err != nil || length <= 1 || length > maxFrameUnits {
			return "", false, errMalformedFrame
		}
		// Google's declared length counts the separator newline as its first
		// UTF-16 unit (reference client: payload_start = len(digits), so the
		// slice starts at "\n"). Consume the newline here and count
		// length-1 units of actual payload; keeping the full length would
		// overrun every frame by one byte and desync the stream.
		p.expected = length - 1
		p.units = 0
		p.scanned = 0
		p.havePrefix = true
		p.buf = p.buf[i+1:]
	}

	// Walk runes from where the previous Feed stopped. An astral rune is
	// consumed atomically: no well-formed frame length can split its two
	// units, so the cut always lands on a rune boundary. Invalid UTF-8
	// counts as one RuneError unit of one byte.
	i := p.scanned
	for p.units < p.expected && i < len(p.buf) {
		r, size := utf8.DecodeRune(p.buf[i:])
		units := 1
		if r > 0xFFFF {
			units = 2
		}
		if p.units+units > p.expected {
			return "", false, errMalformedFrame
		}
		p.units += units
		i += size
	}
	p.scanned = i
	if p.units < p.expected {
		return "", false, nil // need more bytes
	}
	frame := string(p.buf[:i])
	p.buf = p.buf[i:]
	p.havePrefix = false
	return frame, true, nil
}
