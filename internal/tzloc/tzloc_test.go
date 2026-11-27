package tzloc

import (
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for name, want := range map[string]int{"": 0, "+08:00": 8 * 3600, "-05:30": -(5*3600 + 30*60), "Asia/Singapore": 8 * 3600} {
		l, err := Load(name)
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if _, off := now.In(l).Zone(); off != want {
			t.Errorf("%q: offset %d, want %d", name, off, want)
		}
	}
	for _, bad := range []string{"+25:00", "Europe/Atlantis", "+08"} {
		if _, err := Load(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
