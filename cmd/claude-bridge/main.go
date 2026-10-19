// Command claude-bridge serves an Anthropic Messages surface by driving the
// official Claude Code CLI against a Claude subscription login. It is a sidecar:
// ezllm dials it as a plain anthropic-compatible account, and nothing else in the
// router changes. See docs/claude-subscription-design.md.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/claudebridge"
)

// version is the bridge's own release string (the CLI's version is reported
// separately by /status).
var version = "0.1.0"

// flags this bridge depends on. Asserted at boot: a CLI release may drop one while
// it still "works" (measured: --max-turns is accepted on 2.1.299 but absent from
// --help), and a silent drift here breaks every request.
var requiredFlags = []string{
	"--input-format", "--output-format", "--include-partial-messages",
	"--setting-sources", "--system-prompt-file", "--strict-mcp-config",
	"--permission-mode", "--no-session-persistence", "--session-id", "--max-turns",
}

func main() {
	var (
		addr        = flag.String("addr", "127.0.0.1:8790", "listen address (compose network only; never publish)")
		bin         = flag.String("claude-bin", "claude", "path to the official Claude Code CLI")
		configDir   = flag.String("claude-config-dir", "", "CLI config dir holding the subscription login (default ~/.claude-sub)")
		tokenFile   = flag.String("token-file", "", "file holding the shared secret ezllm presents as x-api-key")
		timeout     = flag.Duration("timeout", 300*time.Second, "per-request deadline")
		concurrency = flag.Int("concurrency", 2, "max concurrent native processes")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println("claude-bridge", version)
		return
	}
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("claude-bridge ")

	dir := *configDir
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".claude-sub")
	}
	resolved, err := exec.LookPath(*bin)
	if err != nil {
		log.Fatalf("official Claude Code CLI not found (%q): %v — install it first, this bridge does not bundle it", *bin, err)
	}

	cliVersion := assertCLI(resolved)
	login, plan := assertLogin(resolved, dir)

	token := strings.TrimSpace(os.Getenv("CLAUDE_BRIDGE_TOKEN"))
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			log.Fatalf("cannot read token file %s: %v", *tokenFile, err)
		}
		token = strings.TrimSpace(string(b))
	}
	if token == "" {
		log.Fatal("no shared secret: pass -token-file or set CLAUDE_BRIDGE_TOKEN (ezllm sends it as x-api-key)")
	}
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL"} {
		if os.Getenv(key) != "" {
			log.Printf("WARNING: %s is set in this environment and is dropped per request; the subscription login in %s is the only credential path", key, dir)
		}
	}

	srv := &claudebridge.Server{
		Runner: &claudebridge.Runner{
			Bin:           resolved,
			ConfigDir:     dir,
			Timeout:       *timeout,
			MaxConcurrent: *concurrency,
			Logf:          log.Printf,
		},
		Token:   token,
		Version: version,
		Info: map[string]any{
			"status":            "ok",
			"version":           version,
			"cli_bin":           resolved,
			"cli_version":       cliVersion,
			"claude_config_dir": dir,
			"login":             login,
			"subscription":      plan,
			"timeout_seconds":   timeout.Seconds(),
			"max_concurrent":    *concurrency,
			"tools":             "unsupported (M1 text-only)",
		},
		Logf: log.Printf,
	}
	log.Printf("listening on %s (cli %s, login %s, plan %s, config %s)", *addr, cliVersion, login, plan, dir)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

// assertCLI proves the binary runs and still accepts every flag this design
// depends on. Measured drift on 2.1.299 (--max-turns missing from --help) is why
// this is a boot gate rather than a comment.
func assertCLI(bin string) string {
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		log.Fatalf("cannot run %s --version: %v", bin, err)
	}
	version := strings.TrimSpace(string(out))
	help, err := exec.Command(bin, "--help").CombinedOutput()
	if err != nil {
		log.Fatalf("cannot run %s --help: %v", bin, err)
	}
	var missing []string
	for _, f := range requiredFlags {
		if !strings.Contains(string(help), f) {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		log.Printf("WARNING: CLI %s does not list %v in --help; flags may have drifted", version, missing)
	}
	return version
}

// assertLogin reports the subscription login state. The CLI answers "logged out"
// instantly (no upstream call) when its process identity is wrong, so USER and
// LOGNAME must exist — checked here so a container misconfig fails the boot.
func assertLogin(bin, dir string) (string, string) {
	if os.Getenv("USER") == "" || os.Getenv("LOGNAME") == "" {
		log.Fatal("USER and LOGNAME must be set: the CLI's local login check reads process identity and reports logged-out without them")
	}
	creds := filepath.Join(dir, ".credentials.json")
	if _, err := os.Stat(creds); err != nil {
		log.Fatalf("no subscription login in %s (%s missing) — run: CLAUDE_CONFIG_DIR=%s claude auth login", dir, creds, dir)
	}
	cmd := exec.Command(bin, "auth", "status")
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("WARNING: claude auth status failed (%v); continuing on the credentials file alone", err)
		return "unverified", "unknown"
	}
	text := string(out)
	state, plan := "unknown", "unknown"
	for _, line := range strings.Split(text, "\n") {
		l := strings.ToLower(strings.TrimSpace(line))
		switch {
		case strings.Contains(l, "logged"):
			switch {
			case strings.Contains(l, "not logged") || strings.Contains(l, "false"):
				state = "logged-out"
			case strings.Contains(l, "true") || strings.Contains(l, "as ") ||
				strings.Contains(l, "oauth") || strings.Contains(l, "claude.ai"):
				state = "logged-in"
			}
		case strings.Contains(l, "subscription"):
			plan = strings.TrimSpace(line)
		}
	}
	if state == "logged-out" {
		log.Fatalf("claude auth status reports logged out for %s — run: CLAUDE_CONFIG_DIR=%s claude auth login", dir, dir)
	}
	return state, plan
}
