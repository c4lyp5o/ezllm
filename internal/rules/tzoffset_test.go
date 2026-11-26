package rules

import (
	"testing"
	"time"
)

// The API accepts +08:00 for win_tz. It used to fail the engine's check, so
// the rule was dropped, and the window was read in UTC.
func TestWindowAcceptsFixedOffset(t *testing.T) {
	w := Window{TZ: "+08:00"}
	if err := w.ValidateTZ(); err != nil {
		t.Fatalf("+08:00 refused: %v", err)
	}
	if _, off := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).In(w.loc()).Zone(); off != 8*3600 {
		t.Fatalf("offset = %d, want 8h", off)
	}
}
