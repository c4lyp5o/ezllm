package store

import (
	"context"
	"path/filepath"
	"testing"
)

// v30 renames the chat-completions wire from "openai" to "chat". A database
// stamped v29 carries the old column, pin values and ledger surfaces; opening
// it must carry all three over, and opening it again must change nothing.
func TestMigrateRenamesTheOpenAIWireToChat(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.sqlite")

	db, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("open fresh: %v", err)
	}
	w := db.Writer()
	for _, q := range []string{
		`INSERT INTO accounts(id, name, namespace, kind, base_url, enabled) VALUES (1,'a','ns','openai-compatible','http://x',1)`,
		`INSERT INTO models(account_id, model_id, proto_chat, proto_pin) VALUES (1,'m',1,'chat')`,
		`INSERT INTO calls(ts, client, surface, account, model, status) VALUES ('2026-10-05T10:00:00.000Z','c','chat','a','m',200)`,
		// Rewind to the v29 shape.
		`ALTER TABLE models RENAME COLUMN proto_chat TO proto_openai`,
		`UPDATE models SET proto_pin='openai'`,
		`UPDATE calls SET surface='openai'`,
		`DELETE FROM schema_version`,
		`INSERT INTO schema_version(version) VALUES (29)`,
	} {
		if _, err := w.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	for pass := 1; pass <= 2; pass++ {
		db, err = Open(ctx, Options{Path: path})
		if err != nil {
			t.Fatalf("pass %d reopen: %v", pass, err)
		}
		var chat int
		var pin, surface string
		if err := db.Reader().QueryRow(`SELECT proto_chat, proto_pin FROM models WHERE model_id='m'`).Scan(&chat, &pin); err != nil {
			t.Fatalf("pass %d read models: %v", pass, err)
		}
		if chat != 1 || pin != "chat" {
			t.Errorf("pass %d: proto_chat=%d pin=%q, want 1 / chat", pass, chat, pin)
		}
		if err := db.Reader().QueryRow(`SELECT surface FROM calls`).Scan(&surface); err != nil || surface != "chat" {
			t.Errorf("pass %d: ledger surface = %q (%v), want chat", pass, surface, err)
		}
		if has, _ := hasColumn(ctx, db.Reader(), "models", "proto_openai"); has {
			t.Errorf("pass %d: proto_openai column survived", pass)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
