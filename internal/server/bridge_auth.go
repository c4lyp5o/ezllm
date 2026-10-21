package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// bridgeAccount admits a request to an in-process web bridge (gemini-web,
// claude-web). These routes share the public listener, so only a request
// carrying this process's secret (stamped by the provider adapter) may name
// an account — an account id alone would unlock its stored cookie — and the
// stamped id must be an ENABLED account of the bridge's kind.
func bridgeAccount(r *http.Request, db *store.DB, kind provider.Kind, accountHeader string) (*provider.Account, error) {
	if !provider.ValidBridgeSecret(r.Header.Get(provider.BridgeSecretHeader)) {
		return nil, errors.New("missing or wrong bridge secret")
	}
	h := strings.TrimSpace(r.Header.Get(accountHeader))
	if h == "" {
		return nil, errors.New("missing account header")
	}
	// ParseInt, not Sscan: "12abc" is a bad header, not account 12.
	id, err := strconv.ParseInt(h, 10, 64)
	if err != nil || id <= 0 {
		return nil, errors.New("bad account header")
	}
	acct, err := db.AccountByID(r.Context(), id)
	if err != nil || acct == nil || acct.Kind != kind || !acct.Enabled {
		return nil, errors.New("not an enabled " + string(kind) + " account")
	}
	return acct, nil
}
