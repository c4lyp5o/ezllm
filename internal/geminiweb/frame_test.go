package geminiweb

import (
	"strings"
	"testing"
)

// utf16Units counts UTF-16 code units the way wrb.fr's length prefix does.
func utf16Units(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func frame(s string) string {
	return itoa(utf16Units(s)) + "\n" + s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// FeedOne delivers the stream in one shot and expects exactly one frame.
func FeedOne(t *testing.T, stream string, want string) {
	t.Helper()
	var p FrameParser
	got, err := p.Feed([]byte(stream))
	if err != nil {
		t.Fatalf("Feed(%q): %v", stream, err)
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("Feed(%q) = %q, want [%q]", stream, got, want)
	}
}

func TestFrameParserWholeFrame(t *testing.T) {
	FeedOne(t, frame(`["abc",123,[1,2]]`), `["abc",123,[1,2]]`)
}

func TestFrameParserAstralPayload(t *testing.T) {
	// Emoji count as TWO UTF-16 units — the length prefix must agree or the
	// frame is malformed. A parser counting runes would desync here.
	payload := `["héllo 🌈 ok"]`
	FeedOne(t, frame(payload), payload)
	if u := utf16Units(payload); u != 15 { // 13 BMP units + 2 for the emoji
		t.Fatalf("utf16Units = %d, want 15", u)
	}
}

func TestFrameParserMultipleFramesOneChunk(t *testing.T) {
	stream := frame(`["a"]`) + frame(`["b"]`) + frame(`["c"]`)
	var p FrameParser
	got, err := p.Feed([]byte(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != `["a"]` || got[1] != `["b"]` || got[2] != `["c"]` {
		t.Fatalf("got %q", got)
	}
}

func TestFrameParserByteByByte(t *testing.T) {
	stream := frame(`["streamed","payload",{"k":"v"}]`)
	var p FrameParser
	var out []string
	for i := 0; i < len(stream); i++ {
		got, err := p.Feed([]byte{stream[i]})
		if err != nil {
			t.Fatalf("byte %d: %v", i, err)
		}
		out = append(out, got...)
	}
	if len(out) != 1 || out[0] != `["streamed","payload",{"k":"v"}]` {
		t.Fatalf("byte-by-byte got %q", out)
	}
}

func TestFrameParserFragmented(t *testing.T) {
	stream := frame(`["frame one"]`) + frame(`["frame two"]`)
	var p FrameParser
	var out []string
	for i := 0; i < len(stream); i += 3 { // 3-byte chunks: boundaries land mid-prefix/mid-rune/mid-payload
		end := i + 3
		if end > len(stream) {
			end = len(stream)
		}
		got, err := p.Feed([]byte(stream[i:end]))
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		out = append(out, got...)
	}
	if len(out) != 2 || out[0] != `["frame one"]` || out[1] != `["frame two"]` {
		t.Fatalf("fragmented got %q", out)
	}
}

func TestFrameParserPartialFrameHangsNothing(t *testing.T) {
	// A truncated tail must simply buffer forever (no error, no panic, no
	// fake frame): the stream may still deliver the rest later.
	var p FrameParser
	// ["partial"] is 11 UTF-16 units — declaring 12 and completing with 11
	// would make the parser swallow the NEXT frame's prefix into this frame.
	got, err := p.Feed([]byte("11\n[\"partial"))
	if err != nil || len(got) != 0 {
		t.Fatalf("partial: got=%q err=%v, want nothing", got, err)
	}
	if p.Buffered() == 0 {
		t.Fatal("partial frame not buffered")
	}
	// …and it completes later.
	rest := `"]` + frame(`["next"]`)
	got, err = p.Feed([]byte(rest))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != `["partial"]` || got[1] != `["next"]` {
		t.Fatalf("completion got %q", got)
	}
}

func TestFrameParserMalformed(t *testing.T) {
	cases := map[string]string{
		"non-numeric length": "abc\npayload",
		"zero length":        "0\n",
		"negative length":    "-5\npayload",
		"oversized length":   "99999999999\npayload",
		"unclosable astral":  "2\n\"" + "🐀", // 2 units declared, but quote(1)+astral(2)=3 can never close
	}
	for name, stream := range cases {
		var p FrameParser
		if _, err := p.Feed([]byte(stream)); err == nil {
			t.Errorf("%s: expected errMalformedFrame", name)
		}
	}
}

func TestFrameParserEmptyPayloadAfterPrefix(t *testing.T) {
	// A well-formed empty-string payload (2 units for the quotes).
	stream := itoa(utf16Units(`""`)) + "\n" + `""`
	var p FrameParser
	got, err := p.Feed([]byte(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != `""` {
		t.Fatalf("got %q", got)
	}
}

func TestFrameParserPrefixSplitAcrossFeeds(t *testing.T) {
	stream := frame(`["split prefix"]`)
	var p FrameParser
	var out []string
	// First byte alone (prefix digit), then a two-byte chunk splitting the
	// prefix, then the rest: the prefix must survive across Feed calls.
	got, err := p.Feed([]byte(stream[:1]))
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, got...)
	got, err = p.Feed([]byte(stream[1:3]))
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, got...)
	got, err = p.Feed([]byte(stream[3:]))
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, got...)
	if len(out) != 1 || out[0] != `["split prefix"]` {
		t.Fatalf("split-prefix got %q", out)
	}
}

func TestFrameParserNeverInfiniteLoop(t *testing.T) {
	// A frame whose declared length never completes must not loop Feed:
	// one call returns what's available and stops.
	var p FrameParser
	if _, err := p.Feed([]byte(`5\n"ab`)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Feed([]byte("c")); err != nil {
		t.Fatal(err)
	}
	// still incomplete (needs one more byte): no error, no output, bound buffer
	got, err := p.Feed([]byte(""))
	if err != nil || len(got) != 0 {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestFrameParserPayloadIsJSONArray(t *testing.T) {
	// The parser returns raw payload strings; well-formed payloads for our
	// use are JSPB JSON arrays. Guard the fixture shape we actually stream.
	payload := `[[["answer","text"]],[null,[2]]]`
	if !strings.HasPrefix(payload, "[") {
		t.Fatal("fixture must be a JSON array")
	}
	FeedOne(t, frame(payload), payload)
}
