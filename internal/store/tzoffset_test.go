package store

import (
	"testing"
	"time"
)

// Usage buckets for a +08:00 rule follow +08:00, not UTC.
func TestUsageWindowFixedOffset(t *testing.T) {
	if _, off := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).In(windowSpec{tz: "+08:00"}.loc()).Zone(); off != 8*3600 {
		t.Fatalf("offset = %d, want 8h", off)
	}
	r := ModelRule{AccountID: 1, ModelID: "m", WinTZ: "Asia/Kuala_Lumpyr"}
	if err := ValidateModelRule(&r); err == nil {
		t.Fatal("a misspelt zone was accepted")
	}
}
