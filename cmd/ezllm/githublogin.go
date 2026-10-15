package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/c4lyp5o/ezllm/internal/githubcopilot"
)

// runGitHubLogin is the `ezllm github-login` subcommand: the interactive
// device flow for a GitHub Copilot account. It prints the verification URL
// and user code, polls until the operator authorizes, verifies the mint
// works, and prints the session JSON to store as the account's provider key
// (via the admin API's key-add path — never pasted into a chat or a log).
//
// The flow uses GitHub's PUBLIC Copilot CLI client id and scope read:user —
// the official CLI's exact path. Custom client ids (and their restricted
// tier) are opt-in via GITHUB_OAUTH_CLIENT_ID.
func runGitHubLogin() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := githubcopilot.Config{}
	dc, err := githubcopilot.RequestDeviceCode(ctx, cfg)
	if err != nil {
		return fmt.Errorf("device code request failed: %w", err)
	}
	fmt.Println("=== GitHub Copilot authorization (device flow) ===")
	fmt.Println()
	fmt.Println("  1. Open:", dc.VerificationURI)
	fmt.Println("  2. Enter code:", dc.UserCode)
	fmt.Println()
	fmt.Println("Waiting for authorization (Ctrl-C to abort)…")

	session, err := githubcopilot.PollAccessToken(ctx, cfg, dc)
	if err != nil {
		return fmt.Errorf("authorization failed: %w", err)
	}

	// Verify the mint immediately: a token that can't mint is useless to
	// store, and this is the earliest honest failure point.
	token, expiresAt, err := githubcopilot.MintCopilotToken(ctx, cfg, session.AccessToken)
	if err != nil {
		return fmt.Errorf("copilot token mint failed (is the account entitled to Copilot?): %w", err)
	}
	session.CopilotToken, session.CopilotExpiresAt = token, expiresAt

	if info, err := githubcopilot.FetchUserInfo(ctx, cfg, session.AccessToken); err == nil {
		session.GitHubLogin = info.Login
		fmt.Printf("Authorized as @%s — copilot token valid until %s\n", info.Login, expiresAt.Format(time.RFC3339))
	} else {
		fmt.Printf("Authorized — copilot token valid until %s\n", expiresAt.Format(time.RFC3339))
	}

	// List the first few models so the operator knows what the account can
	// actually serve (entitlement is account-specific).
	if models, err := githubcopilot.ListModels(ctx, cfg, token); err == nil && len(models) > 0 {
		shown := min(len(models), 8)
		ids := make([]string, 0, shown)
		for _, m := range models[:shown] {
			ids = append(ids, m.ID)
		}
		fmt.Printf("Catalog (%d models, first %d): %s\n", len(models), shown, strings.Join(ids, ", "))
	} else {
		fmt.Println("Catalog: /models unavailable right now (the bootstrap fallback list applies)")
	}

	fmt.Println()
	fmt.Println("Store this as the account's provider key (kind github-copilot):")
	fmt.Println("---8<---")
	fmt.Println(session.Encode())
	fmt.Println("---8<---")
	fmt.Println("Key material is live account access: keep it in the encrypted key store only.")
	return nil
}
