package rules

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// countingUsage fails every read and counts how many were attempted.
type countingUsage struct{ reads atomic.Int64 }

func (c *countingUsage) UsageFor(context.Context, int64, string, string) (int64, error) {
	c.reads.Add(1)
	return 0, errors.New("usage store unavailable")
}

// Rules that never loaded must stay out of routing: with the rule store down
// at startup and usage reads failing too, every model is allowed and no usage
// is read.
func TestUnloadedRulesNeverRefuse(t *testing.T) {
	usage := &countingUsage{}
	e, err := NewEngineRetrying(&flakySource{}, usage, nil, time.Hour)
	if err == nil {
		t.Fatal("precondition: the rule store should fail at startup")
	}
	defer e.Close()
	for _, m := range []string{"m", "other", ""} {
		if err := e.Eligible(context.Background(), 1, m); err != nil {
			t.Fatalf("model %q refused with no rules loaded: %v", m, err)
		}
	}
	if n := usage.reads.Load(); n != 0 {
		t.Fatalf("usage read %d times with no rules", n)
	}
}
