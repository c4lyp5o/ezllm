package registration

// S10 regression: truncate cut at s[:n] with no regard for rune boundaries.
// The err column is TEXT, the dashboard renders it, and a provider whose
// message contains multi-byte UTF-8 (accents, CJK, curly quotes — routine in
// provider error prose) could be split mid-rune, producing INVALID UTF-8 in
// the ledger and a replacement-character render in the dashboard.
//
// proxy.go's snippet truncation already walked back to a rune boundary; this
// copies that behaviour so both places agree.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateNeverSplitsARune(t *testing.T) {
	// A string of 3-byte runes (e.g. CJK): a naive cut at 160 bytes lands
	// mid-rune. The result must still be valid UTF-8 AND shorter.
	long := strings.Repeat("错误", 80) // 480 bytes, 160 runes
	out := truncate(long, 160)
	if !utf8.ValidString(out) {
		t.Fatalf("truncate produced invalid UTF-8: %q", out)
	}
	// At most n bytes of content plus the 3-byte ellipsis.
	if len(out) > 160+3 {
		t.Fatalf("truncate produced %d bytes, want <= %d", len(out), 160+3)
	}
	if !strings.HasSuffix(out, "…") {
		t.Errorf("truncate must end with the ellipsis, got %q", out[len(out)-10:])
	}
	// Every byte present must belong to a whole rune.
	for _, r := range out {
		if r == utf8.RuneError {
			t.Fatalf("truncate emitted a replacement char — a rune was split")
		}
	}
}

// Mixed multi-byte content, including 2- and 4-byte runes.
func TestTruncateHandlesMixedWidthRunes(t *testing.T) {
	s := strings.Repeat("é€你🎉", 60) // 2,3,3,4 bytes respectively
	out := truncate(s, 100)
	if !utf8.ValidString(out) {
		t.Fatalf("invalid UTF-8 after truncate: %q", out)
	}
	// Content bytes stay within the cap (+ the 3-byte ellipsis).
	if len(out) > 100+3 {
		t.Fatalf("truncate produced %d bytes, want <= %d", len(out), 100+3)
	}
	for i, r := range out {
		if r == utf8.RuneError {
			t.Fatalf("replacement char at rune %d — a multi-byte rune was split", i)
		}
	}
	// The string was 720 bytes, so the cut really did happen.
	if len(out) == len(s) {
		t.Fatal("mixed-width string was not truncated at all")
	}
}

// The short case is untouched, and ASCII cuts exactly at n.
func TestTruncateShortAndASCII(t *testing.T) {
	if got := truncate("short", 160); got != "short" {
		t.Errorf("short string must pass through, got %q", got)
	}
	ascii := strings.Repeat("a", 400)
	got := truncate(ascii, 160)
	if got != strings.Repeat("a", 160)+"…" {
		t.Errorf("ASCII cut = %d bytes, want exactly 160 + ellipsis", len(got))
	}
}
