package server

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func TestAccountKeyOperationsRejectOtherAccounts(t *testing.T) {
	var calls atomic.Int64
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	owner := seededAccount(t, h)
	other, err := h.db.UpsertAccount(context.Background(), provider.Account{
		Name: "other", Namespace: "other", Kind: provider.KindOpenAICompatible,
		BaseURL: h.upstream.URL, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var keyID int64
	if err := h.db.Reader().QueryRow(`SELECT id FROM provider_keys WHERE account_id=?`, owner).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/admin/accounts/%d/keys/%d", other, keyID)
	code, body := jsonDo(t, h, http.MethodPost, path+"/retest", "tok-admin", map[string]any{})
	if code != http.StatusNotFound {
		t.Errorf("cross-account retest: got %d %s, want 404", code, body)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("cross-account retest made %d upstream calls", got)
	}
	code, body = jsonDo(t, h, http.MethodPatch, path, "tok-admin", map[string]any{"enabled": false})
	if code != http.StatusNotFound {
		t.Errorf("cross-account toggle: got %d %s, want 404", code, body)
	}
	var enabled int
	var lastTestOK *bool
	if err := h.db.Reader().QueryRow(`SELECT enabled, last_test_ok FROM provider_keys WHERE id=?`, keyID).Scan(&enabled, &lastTestOK); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 || lastTestOK != nil {
		t.Errorf("cross-account operations changed key: enabled=%d last_test_ok=%v", enabled, lastTestOK)
	}
	path = fmt.Sprintf("/admin/accounts/%d/keys/%d", owner, keyID)
	code, body = jsonDo(t, h, http.MethodPatch, path, "tok-admin", map[string]any{"enabled": false})
	if code != http.StatusOK {
		t.Fatalf("owner toggle: got %d %s, want 200", code, body)
	}
	if err := h.db.Reader().QueryRow(`SELECT enabled FROM provider_keys WHERE id=?`, keyID).Scan(&enabled); err != nil || enabled != 0 {
		t.Errorf("owner toggle did not disable key: enabled=%d err=%v", enabled, err)
	}
}
