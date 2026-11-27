package compress

import (
	"math/rand"
	"testing"
)

// The prefilter may only skip a pattern that cannot match: whenever a pattern
// matches, protectNeeds must have allowed it.
func TestProtectNeedsNeverSkipsAMatch(t *testing.T) {
	if len(protectSpans) != len(protectNeeds("")) {
		t.Fatalf("protectNeeds covers %d patterns, protectSpans has %d", len(protectNeeds("")), len(protectSpans))
	}
	corpus := []string{
		"see https://example.com/x and acme.io:8080/path", "mail me@example.com", "10.0.0.1:80",
		"run `make test` now", "/usr/bin/env and ~/x and ../y/z", `C:\Users\you\file.txt`,
		"min_compress_ratio e2e-test", "camelCase PascalCase", `"quoted" 'single'`, "0x1F", "v1.2.3",
		"50ms 10 GB 3%", "1,000,000", "plain prose with nothing special",
	}
	alphabet := []byte("aZx0159.:/@`\\_-\"', \n~$ABCcom")
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		b := make([]byte, 1+rng.Intn(24))
		for j := range b {
			b[j] = alphabet[rng.Intn(len(alphabet))]
		}
		corpus = append(corpus, string(b))
	}
	for _, s := range corpus {
		need := protectNeeds(s)
		for i, re := range protectSpans {
			if !need[i] && re.MatchString(s) {
				t.Fatalf("pattern %d (%s) matches %q but the prefilter skipped it", i, re, s)
			}
		}
	}
}
