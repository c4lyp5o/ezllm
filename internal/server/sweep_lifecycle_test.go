package server

// Sweep-registry lifecycle (docx item 14): "sweep should just end/delete
// whether on success or not". Two failure modes were reachable before the
// run-identity fix:
//
//  1. Cross-deletion — endSweep(id) deleted whatever entry sat under the
//     id, so an OLD goroutine finishing late deleted a NEWER run's
//     registration. The newer sweep kept running while the map said idle,
//     and the NEXT click either started a second overlapping sweep or hit
//     a 409 against nothing the operator could see.
//  2. A wedged run (budget over, persist hung) left its entry forever —
//     every later click answered 409 "already running" until process
//     restart. beginSweep/sweepStatus now treat a past-deadline entry as
//     dead and replace/drop it, so no sweep can wedge its account.

import (
	"context"
	"testing"
	"time"
)

func TestEndSweepIgnoresARunThatIsNoLongerTheRegisteredOne(t *testing.T) {
	s := &Server{}
	const id = int64(7)
	// A negative budget makes the first entry stale immediately, so the
	// second beginSweep replaces it — the exact moment the race was in.
	if _, err := s.beginSweep(id, []string{"chat"}, 1, -time.Hour); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	first := s.sweeps[id]
	if _, err := s.beginSweep(id, []string{"chat"}, 1, time.Minute); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	s.endSweep(id, first) // old goroutine's deferred cleanup
	if s.sweeps[id] == nil {
		t.Fatalf("old run's endSweep deleted the NEW run's entry — sweeps would run invisible")
	}
	// The current run's own cleanup must still remove it.
	s.endSweep(id, s.sweeps[id])
	if s.sweeps[id] != nil {
		t.Fatalf("current run's endSweep left its own entry behind")
	}
}

func TestAttachCancelOnlyBindsToTheRegisteredRun(t *testing.T) {
	s := &Server{}
	const id = int64(8)
	old, err := s.beginSweep(id, []string{"chat"}, 1, -time.Hour)
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	cur, err := s.beginSweep(id, []string{"chat"}, 1, time.Minute)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	cancelled := false
	s.attachCancel(id, old, func() { cancelled = true }) // stale bind
	if s.sweeps[id].cancel != nil {
		t.Fatalf("stale run attached its cancel onto the CURRENT run")
	}
	s.attachCancel(id, cur, func() { cancelled = true })
	if s.sweeps[id].cancel == nil {
		t.Fatalf("current run's cancel did not bind")
	}
	s.sweeps[id].cancel()
	if !cancelled {
		t.Fatalf("bound cancel did not fire")
	}
}

func TestPastDeadlineSweepSelfHeals(t *testing.T) {
	s := &Server{}
	const id = int64(9)
	// A negative budget makes the entry stale immediately: budget+grace in
	// the past.
	if _, err := s.beginSweep(id, []string{"chat"}, 1, -time.Hour); err != nil {
		t.Fatalf("stale sweep should not block: %v", err)
	}
	// sweepStatus must report (and drop) the stale entry, not a live one.
	st := s.sweepStatus(id)
	if run, _ := st["running"].(bool); run {
		t.Fatalf("stale entry reported as running: %v", st)
	}
	if len(s.sweeps) != 0 {
		t.Fatalf("stale entry was not dropped: %v", s.sweeps)
	}
	// A fresh sweep after self-heal must register cleanly.
	if _, err := s.beginSweep(id, []string{"chat"}, 1, time.Minute); err != nil {
		t.Fatalf("sweep after self-heal: %v", err)
	}
	if st := s.sweepStatus(id); st["running"] != true {
		t.Fatalf("fresh sweep not reported running: %v", st)
	}
	// A live sweep still 409s a second click — the guard is unchanged.
	if _, err := s.beginSweep(id, []string{"chat"}, 1, time.Minute); err == nil {
		t.Fatalf("a live sweep must still refuse a second one")
	}
	_ = context.Background() // keep the import honest if assertions above shrink
}
