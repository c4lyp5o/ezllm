package translate

import (
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// The stored per-account fact maps onto the codec convention. An unstated
// fact (auto, or the pre-migration empty string) reads as exclusive — the
// meaning the ledger has always given auto — so a cross-wire Messages hop is
// never refused for it. Only a value outside the vocabulary is refused.

func TestUsageConventionOfStoredFact(t *testing.T) {
	for _, tc := range []struct {
		stored provider.UsageConvention
		want   UsageConvention
	}{
		{provider.UsageConventionAnthropic, UsageExclusive},
		{provider.UsageConventionTotal, UsageInclusive},
		{provider.UsageConventionAuto, UsageExclusive},
		{provider.UsageConvention(""), UsageExclusive},
	} {
		if got, err := UsageConventionOf(tc.stored); err != nil || got != tc.want {
			t.Fatalf("UsageConventionOf(%q) = %q, %v; want %q", tc.stored, got, err, tc.want)
		}
	}
	if _, err := UsageConventionOf(provider.UsageConvention("bogus")); err == nil || !strings.Contains(err.Error(), "usage_convention") {
		t.Fatalf("a value outside the vocabulary must be refused naming the field, got %v", err)
	}
	// The guard feeds the encoder: an exclusive convention adds the cache
	// counters, an inclusive one keeps the total as reported.
	excl, err := UsageConventionOf(provider.UsageConventionAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	in, cache := 3, 5
	norm, err := NormalizeMessagesUsage(UsageIR{Input: &in, CacheRead: &cache}, excl)
	if err != nil || norm.Input == nil || *norm.Input != 8 {
		t.Fatalf("exclusive normalize = %+v, %v", norm, err)
	}
	incl, err := UsageConventionOf(provider.UsageConventionTotal)
	if err != nil {
		t.Fatal(err)
	}
	// A declared 'total' account reports input already including cache.
	total := 8
	if norm, err = NormalizeMessagesUsage(UsageIR{Input: &total, CacheRead: &cache}, incl); err != nil || *norm.Input != 8 {
		t.Fatalf("inclusive normalize = %+v, %v", norm, err)
	}
	// The guard: a declared 'total' whose input is SMALLER than its cache
	// cannot be inclusive (that would make the split negative), so the code
	// reads it as exclusive instead of emitting nonsense.
	if norm, err = NormalizeMessagesUsage(UsageIR{Input: &in, CacheRead: &cache}, incl); err != nil || *norm.Input != 8 {
		t.Fatalf("inconsistent-total guard = %+v, %v; want exclusive reading 8", norm, err)
	}
}
