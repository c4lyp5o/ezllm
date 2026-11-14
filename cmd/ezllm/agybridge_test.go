package main

import "testing"

func TestAgySessionCacheRequiresToolAdapterAndRejectsTypos(t *testing.T) {
	for _, tc := range []struct {
		value            string
		tools, want, bad bool
	}{
		{"", false, false, false}, {"off", true, false, false}, {"on", false, false, true}, {"on", true, true, false}, {" TRUE ", true, true, false}, {"1", true, true, false}, {"enabled", true, false, true},
	} {
		got, err := agySessionCache(func(string) string { return tc.value }, tc.tools)
		if got != tc.want || (err != nil) != tc.bad {
			t.Errorf("%q tools=%v: got=%v err=%v", tc.value, tc.tools, got, err)
		}
	}
}
