package main

import "testing"

func TestAgySessionCacheDefaultsOnWhereItWorksAndRejectsTypos(t *testing.T) {
	for _, tc := range []struct {
		value                    string
		tools, resume, want, bad bool
	}{
		{"", true, true, true, false}, {"", false, true, false, false}, {"", true, false, false, false},
		{"off", true, true, false, false}, {"on", false, true, false, true}, {"on", true, false, false, true},
		{"on", true, true, true, false}, {" TRUE ", true, true, true, false}, {"1", true, true, true, false},
		{"enabled", true, true, false, true},
	} {
		got, err := agySessionCache(func(string) string { return tc.value }, tc.tools, tc.resume)
		if got != tc.want || (err != nil) != tc.bad {
			t.Errorf("%q tools=%v resume=%v: got=%v err=%v", tc.value, tc.tools, tc.resume, got, err)
		}
	}
}
