package provider

import "net/http"

// KindCopilotBridge is GitHub Copilot through the official CLI behind this
// process's loopback bridge (internal/copilotbridge). It is its own kind rather
// than an "openai-compatible" endpoint on purpose: the account is a
// SUBSCRIPTION whose credential lives in the CLI's own login, and an operator
// looking at the provider list must be able to tell it apart from a generic
// endpoint they pasted a key into. It replaces the retired OAuth/device-flow
// provider (docs/copilot-cli-bridge-design.md).
const KindCopilotBridge Kind = "copilot-bridge"

// copilotBridgeAdapter is the openai-compatible WIRE under the bridge's own
// kind. The bridge speaks Chat Completions, so everything the generic adapter
// does (auth header, path join, catalog, quota-less operation) is correct
// unchanged; only the identity differs.
//
// Embedding rather than copying keeps ONE implementation of the wire: a fix to
// the openai-compatible path applies here automatically, which is the point of a
// bridge that speaks a standard protocol.
type copilotBridgeAdapter struct{ *openAICompatible }

func (a *copilotBridgeAdapter) Kind() Kind { return KindCopilotBridge }

// NewCopilotBridge registers the bridge kind over the openai-compatible wire.
func NewCopilotBridge(c *http.Client) Adapter {
	return &copilotBridgeAdapter{&openAICompatible{client: c}}
}
