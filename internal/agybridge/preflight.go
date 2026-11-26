package agybridge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// RequiredFlags are asserted against `agy --help` at boot: a release that drops
// one would otherwise fail every request.
var RequiredFlags = []string{
	"--print", "--output-format", "--input-format", "--model", "--disable-slash-commands",
}

// TokenPath is where agy keeps the subscription login when no keyring is
// reachable (measured on 1.4.7 with DBUS_SESSION_BUS_ADDRESS unset).
func TokenPath(home string) string {
	return filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
}

// Model is one row of GET /v1/models.
type Model struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	OwnedBy     string `json:"owned_by"`
	DisplayName string `json:"display_name,omitempty"`
}

// Preflight is what boot learned about the CLI.
type Preflight struct {
	Version            string
	Models             []Model
	StructuredOutput   bool
	ConversationResume bool
}

// RunPreflight proves the CLI is the one this bridge speaks to and that the
// login is usable: version, flag surface, token file, and a live `agy models`
// (which needs a valid token). Every failure names its fix.
func RunPreflight(ctx context.Context, bin, home string) (Preflight, error) {
	var p Preflight
	run := func(args ...string) (string, error) {
		c, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(c, bin, args...)
		cmd.Env = childEnv(home)
		cmd.Dir = home
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := run("--version")
	if err != nil {
		return p, fmt.Errorf("agy bridge: %s --version failed: %v: %s", bin, err, strings.TrimSpace(out))
	}
	p.Version = strings.TrimSpace(out)

	help, err := run("--help")
	if err != nil {
		return p, fmt.Errorf("agy bridge: %s --help failed: %w", bin, err)
	}
	p.StructuredOutput = helpHasFlag(help, "--json-schema")
	p.ConversationResume = helpHasFlag(help, "--conversation")
	for _, f := range RequiredFlags {
		if !helpHasFlag(help, f) {
			return p, fmt.Errorf("agy bridge: agy %s does not offer required flag %s (docs/agy-bridge-runbook.md)", p.Version, f)
		}
	}
	if _, err := os.Stat(TokenPath(home)); err != nil {
		return p, fmt.Errorf("agy bridge: no Antigravity login at %s: log in once on the host with DBUS unset and HOME pointed at the mounted login directory (docs/bridges/antigravity.md)", TokenPath(home))
	}
	if err := CheckSettings(home); err != nil {
		return p, err
	}
	out, err = run("models")
	if err != nil {
		return p, fmt.Errorf("agy bridge: agy models failed (login expired or no network?): %v: %s", err, strings.TrimSpace(urlPattern.ReplaceAllString(out, "[link withheld]")))
	}
	p.Models = ParseModels(out)
	if len(p.Models) == 0 {
		return p, fmt.Errorf("agy bridge: agy models listed nothing for this login: %s", strings.TrimSpace(out))
	}
	return p, nil
}

// ParseModels reads `agy models` output: one "slug<TAB>Display name" per line
// after a progress line.
func ParseModels(out string) []Model {
	var models []Model
	for _, line := range strings.Split(out, "\n") {
		slug, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || slug == "" || strings.ContainsAny(slug, " :") {
			continue
		}
		models = append(models, Model{ID: slug, Object: "model", OwnedBy: "antigravity", DisplayName: strings.TrimSpace(name)})
	}
	return models
}

// SafePermissionMode reports whether the CLI's tool policy is review-only, so a
// headless turn soft-denies every tool that needs approval. request-review is
// the measured default; strict is the documented stricter preset.
func SafePermissionMode(mode string) bool {
	return mode == "request-review" || mode == "strict"
}

// CheckSettings refuses a login whose settings.json loosens the tool policy:
// the agent runs on the ezllm host, next to /data, so it must not run shell
// commands, auto-proceed, or read outside its empty per-request workspace.
func CheckSettings(home string) error {
	path := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("agy bridge: cannot read %s: %w", path, err)
	}
	var s struct {
		ToolPermission          string `json:"toolPermission"`
		AllowNonWorkspaceAccess bool   `json:"allowNonWorkspaceAccess"`
		AgentMode               string `json:"agentMode"`
		Permissions             struct {
			Allow []json.RawMessage `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("agy bridge: %s is not valid JSON: %w", path, err)
	}
	var bad []string
	if s.ToolPermission != "" && !SafePermissionMode(s.ToolPermission) {
		bad = append(bad, "toolPermission="+s.ToolPermission)
	}
	if s.AllowNonWorkspaceAccess {
		bad = append(bad, "allowNonWorkspaceAccess=true")
	}
	if s.AgentMode == "auto" {
		bad = append(bad, "agentMode=auto")
	}
	if len(s.Permissions.Allow) > 0 {
		bad = append(bad, fmt.Sprintf("permissions.allow has %d rule(s)", len(s.Permissions.Allow)))
	}
	if len(bad) > 0 {
		return fmt.Errorf("agy bridge: %s loosens the tool policy (%s); the agent runs on this host, so remove those keys (docs/agy-bridge-runbook.md#lockdown)", path, strings.Join(bad, ", "))
	}
	return nil
}

// helpHasFlag reports whether --help lists flag as a whole word: followed by
// a space, tab, newline, "=" or ",", never as the prefix of a longer flag.
func helpHasFlag(help, flag string) bool {
	for i := 0; ; {
		j := strings.Index(help[i:], flag)
		if j < 0 {
			return false
		}
		end := i + j + len(flag)
		if end == len(help) || strings.ContainsRune(" \t\r\n=,", rune(help[end])) {
			return true
		}
		i = end
	}
}
