package store

import (
	"context"
	"encoding/json"
	"testing"
)

// The cached profile count follows every write: none, one, still one after an
// edit, none after the delete, and a refused delete leaves it alone.
func TestHasCompressionProfilesFollowsWrites(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	if db.HasCompressionProfiles(ctx) {
		t.Fatal("a new database has no profiles")
	}
	p := CompressionProfile{Name: "trim", Enabled: true, Stages: json.RawMessage(`[]`)}
	id, err := db.UpsertCompressionProfile(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if !db.HasCompressionProfiles(ctx) {
		t.Fatal("after creating one, a profile exists")
	}
	p.ID, p.Notes = id, "edited"
	if _, err := db.UpsertCompressionProfile(ctx, p); err != nil || !db.HasCompressionProfiles(ctx) {
		t.Fatalf("an edit keeps the profile (%v)", err)
	}
	if _, err := db.Writer().Exec(`INSERT INTO combos (name, strategy, enabled, compression_profile_id) VALUES ('daily','failover',1,?)`, id); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteCompressionProfile(ctx, id); err == nil || !db.HasCompressionProfiles(ctx) {
		t.Fatalf("a refused delete keeps the profile (%v)", err)
	}
	if _, err := db.Writer().Exec(`UPDATE combos SET compression_profile_id = NULL`); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteCompressionProfile(ctx, id); err != nil {
		t.Fatal(err)
	}
	if db.HasCompressionProfiles(ctx) {
		t.Fatal("after the delete, no profile exists")
	}
}
