// Package claudebridge serves an Anthropic Messages surface by driving the
// official Claude Code CLI (`claude -p`) as the model client. The CLI is the
// only thing allowed to speak to the Claude subscription: this package never
// holds or sends the subscription credential itself (see
// docs/claude-subscription-design.md, locked decision 1).
package claudebridge

import (
	"fmt"
	"strings"
)

// aliases the CLI accepts verbatim for --model. Everything else must resolve to
// one of these through a family word in the requested id.
var directAliases = map[string]bool{
	"haiku": true, "sonnet": true, "opus": true,
	"haiku[1m]": true, "sonnet[1m]": true, "opus[1m]": true,
}

// ModelAlias maps a requested model id to the value the CLI accepts for
// --model. Clients ask for pinned ids (`claude-sonnet-4-6`, `claude-opus-5-5[1m]`)
// while the CLI is addressed by family alias (`sonnet`, `opus[1m]`), so the map
// is family-word based and keeps the `[1m]` long-context suffix.
//
// Unknown ids are an error, not a silent fallback to a different model: a client
// that asked for Opus must never be served Haiku.
func ModelAlias(id string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(id))
	if s == "" {
		return "", fmt.Errorf("claude-bridge: empty model id")
	}
	if directAliases[s] {
		return s, nil
	}
	long := strings.HasSuffix(s, "[1m]")
	base := strings.TrimSuffix(s, "[1m]")
	var family string
	switch {
	case strings.Contains(base, "haiku"):
		family = "haiku"
	case strings.Contains(base, "sonnet"):
		family = "sonnet"
	case strings.Contains(base, "opus"):
		family = "opus"
	default:
		return "", fmt.Errorf("claude-bridge: model %q does not map to a Claude family (haiku/sonnet/opus)", id)
	}
	if long {
		return family + "[1m]", nil
	}
	return family, nil
}
