package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSummariseTurns(t *testing.T) {
	v := []int{1, 30, 2, 5, 1, 20, 3, 8, 1, 4}
	got := summariseTurns("a", "m", v)
	if got.Turns != 10 || got.Requests != 75 || got.Median != 3.5 || got.P75 != 8 || got.P90 != 30 || got.Max != 30 ||
		got.Single != 0.3 || got.Long != 0.2 {
		t.Fatalf("summary = %+v", got)
	}
}

// Rows group by (client, turn_id) per model; rows with no turn id and rows
// outside the window are left out.
func TestTurnLengthsGroupsByClientAndTurn(t *testing.T) {
	db, err := Open(context.Background(), Options{Path: filepath.Join(t.TempDir(), "l.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	rec := func(client, turn, model string, ts time.Time) {
		db.RecordCall(Call{TS: ts, Client: client, Account: "acc", Model: model, Status: 200, TurnID: turn})
	}
	for range 3 {
		rec("my-app", "t1", "sol", now)
	}
	rec("my-app", "t2", "sol", now)
	rec("other", "t1", "sol", now) // same key, other client: a separate turn
	rec("my-app", "", "sol", now)  // no key: not counted
	rec("my-app", "t3", "terra", now)
	rec("my-app", "t9", "sol", now.Add(-48*time.Hour)) // outside the window
	// Failed requests are not work: a 429 and its retry must not lengthen t2,
	// and a turn made only of failures is not a turn.
	db.RecordCall(Call{TS: now, Client: "my-app", Account: "acc", Model: "sol", Status: 429, TurnID: "t2"})
	db.RecordCall(Call{TS: now, Client: "my-app", Account: "acc", Model: "sol", Status: 502, TurnID: "t5"})
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, err := db.TurnLengths(context.Background(), now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Model != "sol" || rows[0].Turns != 3 || rows[0].Requests != 5 || rows[0].Max != 3 ||
		rows[1].Model != "terra" || rows[1].Turns != 1 {
		t.Fatalf("rows = %+v", rows)
	}
}
