package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// The periodic catalog refresh reads the account the way every other path
// does — custom headers included. A gateway that needs a tenant header used
// to answer the refresh with 403 every six hours.
func TestCatalogRefreshCarriesTheFullAccount(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Tenant") != "acme" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
	}))
	defer up.Close()
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "t.sqlite"), BatchSize: 1, BatchWait: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := db.UpsertAccount(ctx, provider.Account{Name: "g", Namespace: "g", Kind: provider.KindOpenAICompatible,
		BaseURL: up.URL + "/v1", Enabled: true, CustomHeaders: map[string]string{"X-Tenant": "acme"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AddKey(ctx, id, "primary", "sk-test-key-123456"); err != nil {
		t.Fatal(err)
	}
	refreshCatalogsOnce(ctx, db, provider.NewRegistry(up.Client()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ok, err := db.ModelExists(ctx, id, "m2")
	if err != nil || !ok {
		t.Fatalf("catalog not refreshed through the tenant header: exists=%v err=%v", ok, err)
	}
}

// The in-process bridges dial the server's own listener. A wildcard bind
// (0.0.0.0, [::], ":port") is not an address to dial; loopback is.
func TestBridgeBaseIsLoopbackForWildcardBinds(t *testing.T) {
	for addr, want := range map[string]string{
		":20129":         "http://127.0.0.1:20129",
		"0.0.0.0:20129":  "http://127.0.0.1:20129",
		"[::]:20129":     "http://127.0.0.1:20129",
		"127.0.0.1:9000": "http://127.0.0.1:9000",
		"10.0.0.5:20129": "http://10.0.0.5:20129",
	} {
		if got := bridgeOrigin(addr); got != want {
			t.Errorf("bridgeOrigin(%q) = %q, want %q", addr, got, want)
		}
	}
}
