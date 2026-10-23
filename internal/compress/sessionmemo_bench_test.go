package compress

import (
	"strconv"
	"testing"
)

// BenchmarkSessionMemoPutWhenFull is steady-state insertion into a memo at its
// global cap: every new message evicts one. An initially empty memo would
// hide the eviction cost entirely.
func BenchmarkSessionMemoPutWhenFull(b *testing.B) {
	for _, capN := range []int{256, DefaultMemoMaxEntries} {
		b.Run(strconv.Itoa(capN), func(b *testing.B) {
			m := NewSessionMemo(&SessionMemoOptions{MaxEntries: capN})
			msg := func(i int) any { return map[string]any{"role": "user", "content": "m" + strconv.Itoa(i)} }
			for i := 0; i < capN; i++ {
				m.Put("s", "p", msg(i), msg(i))
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				k := capN + i
				m.Put("s", "p", msg(k), msg(k))
			}
		})
	}
}
