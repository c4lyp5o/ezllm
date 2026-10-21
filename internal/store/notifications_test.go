package store

import (
	"context"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func TestNotifyDedupFoldsRepeatsUntilRead(t *testing.T) {
	db := openEventsDB(t, 4)
	defer db.Close()
	ctx := context.Background()

	fail := NotificationInput{Kind: NoteCatalogRefreshFailed, Severity: "warn", Title: "refresh failed", DedupKey: "refresh:1"}
	for i := 0; i < 3; i++ {
		fail.Body = "attempt " + string(rune('1'+i))
		if err := db.Notify(ctx, fail); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Notify(ctx, NotificationInput{Kind: NoteFreeModelsAdded, Title: "2 new free models", Data: map[string]any{"models": []string{"a:free", "b:free"}}}); err != nil {
		t.Fatal(err)
	}
	items, unread, err := db.ListNotifications(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || unread != 2 {
		t.Fatalf("items=%d unread=%d, want 2 and 2 (three repeats fold into one)", len(items), unread)
	}
	var folded Notification
	for _, n := range items {
		if n.Kind == NoteCatalogRefreshFailed {
			folded = n
		}
	}
	if folded.Count != 3 || folded.Body != "attempt 3" || folded.Severity != "warn" {
		t.Fatalf("folded row = %+v, want count 3 with the newest body", folded)
	}

	// Once read, the next occurrence is a new row.
	if err := db.MarkNotificationsRead(ctx, []int64{folded.ID}, false); err != nil {
		t.Fatal(err)
	}
	if err := db.Notify(ctx, fail); err != nil {
		t.Fatal(err)
	}
	items, unread, _ = db.ListNotifications(ctx, 10)
	if len(items) != 3 || unread != 2 {
		t.Fatalf("after read + repeat: items=%d unread=%d, want 3 and 2", len(items), unread)
	}
	if err := db.MarkNotificationsRead(ctx, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, unread, _ = db.ListNotifications(ctx, 10); unread != 0 {
		t.Fatalf("mark all read left %d unread", unread)
	}
}

func TestNotifyValidates(t *testing.T) {
	db := openEventsDB(t, 4)
	defer db.Close()
	ctx := context.Background()
	if err := db.Notify(ctx, NotificationInput{Title: "no kind"}); err == nil {
		t.Fatal("a notification without a kind must be refused")
	}
	if err := db.Notify(ctx, NotificationInput{Kind: "k", Title: "t", Severity: "panic"}); err == nil {
		t.Fatal("an unknown severity must be refused")
	}
}

func TestUpsertModelsStampsCatalogSyncedAt(t *testing.T) {
	db := openEventsDB(t, 4)
	defer db.Close()
	ctx := context.Background()
	id, err := db.UpsertAccount(ctx, provider.Account{Name: "s", Namespace: "s", Kind: provider.KindOpenAICompatible, BaseURL: "http://x", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	synced := func() string {
		accts, err := db.ListAccounts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range accts {
			if a.ID == id {
				return a.CatalogSyncedAt
			}
		}
		t.Fatal("account missing")
		return ""
	}
	if s := synced(); s != "" {
		t.Fatalf("never-synced account reports %q", s)
	}
	if err := db.UpsertModels(ctx, id, []provider.ModelInfo{{ID: "m"}}); err != nil {
		t.Fatal(err)
	}
	if s := synced(); s == "" {
		t.Fatal("a saved catalog must stamp catalog_synced_at")
	}
}
