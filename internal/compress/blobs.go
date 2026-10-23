package compress

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

// blobs replaces long base64 payloads in tool output — a raw base64 run or a
// data: URI — with a marker carrying their length and a short sha256. The
// model cannot read base64 as content; the marker keeps the fact that the
// payload was there, how big it was, and which one it was. Detection is
// exact, not a guess: at least min_bytes (default 1024) base64 characters,
// mixing upper case, lower case and digits (a hex digest or a long word is
// one class and never matches). Minified code is deliberately NOT a blob: a
// long space-free line is indistinguishable from compact JSON or a long log
// line, and guessing wrong deletes real content.
//
// Never on preserve-class output (file reads). Safe inside JSON strings: the
// marker has no quote or backslash, so the document stays valid.
func blobs(msgs []any, opts map[string]any) []any {
	minBytes := intOption(opts, "min_bytes", 1024)
	if minBytes < 64 {
		minBytes = 1024
	}
	classes := toolClassOverrides(opts)
	return editAll(msgs, callNames(msgs, opts), func(p textPart) (string, bool) {
		if p.kind != partToolOutput || len(p.text) < minBytes {
			return "", false
		}
		if toolClassFor(p.tool, classes) == toolPreserve {
			return "", false
		}
		out := replaceBlobs(p.text, minBytes)
		if len(out) >= len(p.text) {
			return "", false
		}
		return out, true
	})
}

var (
	dataURI = regexp.MustCompile(`data:([A-Za-z0-9.+/-]+);base64,([A-Za-z0-9+/]+={0,2})`)
	b64Seq  = regexp.MustCompile(`[A-Za-z0-9+/_-]{64,}={0,2}`)
)

func replaceBlobs(text string, minBytes int) string {
	text = dataURI.ReplaceAllStringFunc(text, func(m string) string {
		sub := dataURI.FindStringSubmatch(m)
		if len(sub[2]) < minBytes || !mixedClasses(sub[2]) {
			return m
		}
		return "[ezllm/blob: data:" + sub[1] + ";base64, " + strconv.Itoa(len(sub[2])) + " chars, sha256 " + shortHash(sub[2]) + " elided]"
	})
	return b64Seq.ReplaceAllStringFunc(text, func(m string) string {
		if len(m) < minBytes || !mixedClasses(m) {
			return m
		}
		return "[ezllm/blob: " + strconv.Itoa(len(m)) + " chars base64, sha256 " + shortHash(m) + " elided]"
	})
}

// mixedClasses: base64 of real data mixes upper case, lower case and digits.
func mixedClasses(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r >= 'A' && r <= 'Z' }) >= 0 &&
		strings.IndexFunc(s, func(r rune) bool { return r >= 'a' && r <= 'z' }) >= 0 &&
		strings.IndexFunc(s, func(r rune) bool { return r >= '0' && r <= '9' }) >= 0
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}
