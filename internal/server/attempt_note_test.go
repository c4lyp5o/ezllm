package server

import "testing"

func TestAttemptNoteOnlyWhenRetried(t *testing.T) {
	for _, tc := range []struct {
		attempts, budget int
		want             string
	}{
		{0, 8, ""},
		{1, 8, ""},
		{2, 8, "upstream attempts 2/8"},
		{3, 0, "upstream attempts 3/unlimited"},
	} {
		if got := attemptNote(tc.attempts, tc.budget); got != tc.want {
			t.Errorf("attemptNote(%d, %d) = %q, want %q", tc.attempts, tc.budget, got, tc.want)
		}
	}
}
