package store

import (
	"context"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// Addendum: the dashboard's add-account path (CreateAccount ->
// ValidateAccountInput) must accept the bridge kinds' sentinel base URLs —
// before this, `internal://claudeweb` was refused with "base_url must start
// with http:// or https://", so claude-web / gemini-web accounts could not
// be registered from the dashboard at all (the runbooks' registration step
// only worked via direct store writes in tests).
func TestValidateAccountInputBridgeSentinels(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		baseURL string
		wantErr string // "" = must pass
	}{
		{"claude-web empty passes validation (defaulting is CreateAccount's job)", "claude-web", "", ""},
		{"claude-web explicit sentinel", "claude-web", "internal://claudeweb", ""},
		{"claude-web WRONG sentinel refused", "claude-web", "internal://geminiweb", "must be internal://claudeweb"},
		{"claude-web https URL refused", "claude-web", "https://claude.example", "must be internal://claudeweb"},
		{"gemini-web empty defaults to sentinel", "gemini-web", "", ""},
		{"gemini-web explicit sentinel", "gemini-web", "internal://geminiweb", ""},
		{"gemini-web wrong sentinel refused", "gemini-web", "internal://claudeweb", "must be internal://geminiweb"},
		{"github-copilot is NOT a bridge kind", "github-copilot", "https://api.githubcopilot.com", ""},
		{"github-copilot sentinel refused", "github-copilot", "internal://claudeweb", "http://"},
		{"openai-compatible keeps the old rule", "openai-compatible", "internal://anything", "http://"},
		{"openai-compatible https still fine", "openai-compatible", "https://api.example.com/v1", ""},
	}
	for _, c := range cases {
		err := ValidateAccountInput(AccountInput{
			Name: "t", Namespace: "t-ns", Kind: c.kind, BaseURL: c.baseURL,
		})
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error: %v", c.name, err)
		case c.wantErr != "" && err == nil:
			t.Errorf("%s: want error containing %q, got nil", c.name, c.wantErr)
		case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
			t.Errorf("%s: error %q does not contain %q", c.name, err, c.wantErr)
		}
	}
}

// CreateAccount must DEFAULT an empty base URL to the sentinel so the row
// the bridge later resolves actually carries it (the validator takes the
// input by value — the fill has to happen on the write path).
func TestCreateAccountBridgeSentinelDefaulted(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	id, err := d.CreateAccount(ctx, AccountInput{Name: "cw", Namespace: "cw-ns", Kind: "claude-web"})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	acct, err := d.AccountByID(ctx, id)
	if err != nil {
		t.Fatalf("AccountByID: %v", err)
	}
	if acct.BaseURL != "internal://claudeweb" {
		t.Errorf("base_url = %q, want the sentinel internal://claudeweb", acct.BaseURL)
	}
	if acct.Kind != provider.KindClaudeWeb {
		t.Errorf("kind = %q, want claude-web", acct.Kind)
	}
}
