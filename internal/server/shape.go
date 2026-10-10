package server

// v8 (M9) request-shape capture. The ledger recorded every call's token COUNTS
// but nothing about the request STRUCTURE, so cacheability was unmeasurable:
// "did this call re-send a context we already paid to prefill?" needs the
// conversation's stable core, which raw_usage (a RESPONSE) cannot reconstruct.
//
// prefix_sha hashes the request's reusable core: system/instructions + tool
// definitions + every message EXCEPT the newest turn. Because a growing agent
// loop appends a turn each call, the hash is NOT equal across consecutive turns
// (turn N+1's history is turn N's full body, one turn longer). What it IS good
// for, and what we measure with it:
//
//   - SIBLING reuse: two calls with the SAME conversation (a retry after a 429,
//     a re-send after a dropped connection, N-best branching) hash identically,
//     so "we sent this exact context k times" is a GROUP BY.
//   - CACHE BREAKS: any edit to the stable core — a timestamp or request id
//     injected into the system prompt, a tool list that reorders/regrows, a
//     compression profile rewriting history — changes the hash. A model+session
//     whose hashes churn is a model+session that is not getting cache hits, and
//     that is the number worth seeing.
//   - Volume weighting: joined with tokens_in/req_bytes, it prices exactly how
//     much was re-sent that a stable prefix would have cached.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// shape is one request's captured structure.
type shape struct {
	PrefixSHA string
	MsgCount  int
	ToolCount int
	ReqBytes  int
}

// maxShapeBody bounds the parse for capture. A real prompt here is 10^4–10^5
// tokens (~100–400 KB); anything wildly larger is either a mistake or a
// multimodal blob we should not burn CPU hashing per request. Above it we still
// record byte size but leave the prefix empty (unknown), never fail the request.
const maxShapeBody = 8 << 20 // 8 MiB

// captureShape parses the body we are about to send upstream and derives the
// stable-prefix hash plus counters. It is best-effort by contract: any parse
// problem yields the zero shape (prefix ""), because a failed attribution must
// never affect whether the inference happens.
func captureShape(body []byte, surface provider.Surface) shape {
	s := shape{ReqBytes: len(body)}
	if len(body) == 0 || len(body) > maxShapeBody {
		return s
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return s // not a JSON object — no shape to attribute
	}

	prefix := map[string]any{}
	saw := false // did we see a conversation to attribute a prefix to?

	// System / instructions, per surface shape.
	if v, ok := root["system"]; ok { // anthropic top-level system
		prefix["system"] = v
	}
	if v, ok := root["instructions"]; ok { // responses API
		prefix["instructions"] = v
	}

	// Tool definitions (both the modern `tools` and legacy `functions`).
	tools := 0
	if v, ok := root["tools"].([]any); ok {
		prefix["tools"] = v
		tools += len(v)
	}
	if v, ok := root["functions"].([]any); ok {
		prefix["functions"] = v
		tools += len(v)
	}
	s.ToolCount = tools

	// The conversation, minus its newest turn — the reusable core.
	switch surface {
	case provider.SurfaceResponses:
		switch in := root["input"].(type) {
		case []any:
			saw = true
			s.MsgCount = len(in)
			if len(in) > 1 {
				prefix["input"] = in[:len(in)-1]
			} else {
				prefix["input"] = []any{}
			}
		case string:
			saw = true
			if in != "" {
				s.MsgCount = 1
			}
			prefix["input"] = []any{} // a single fresh input has no history yet
		}
	default: // openai chat + anthropic messages share a top-level messages array
		if msgs, ok := root["messages"].([]any); ok {
			saw = true
			s.MsgCount = len(msgs)
			if len(msgs) > 1 {
				prefix["messages"] = msgs[:len(msgs)-1]
			} else {
				prefix["messages"] = []any{}
			}
		}
	}

	// No conversation at all (not a chat body, wrong type, missing array):
	// there is no prefix to attribute. Leave it empty rather than fabricate a
	// hash of "system + empty history", which would collide with real calls.
	if !saw {
		return s
	}

	// Canonicalize by re-marshaling (Go sorts map keys, so key ORDER is
	// normalized away — a known, stated limitation: pure re-ordering of an
	// otherwise identical tool block will NOT read as a break here). Content
	// changes — an added/removed/edited field — always change this hash, which
	// is the dominant signal we are after.
	canon, err := json.Marshal(prefix)
	if err != nil {
		return s
	}
	sum := sha256.Sum256(canon)
	s.PrefixSHA = hex.EncodeToString(sum[:8]) // 16 hex chars: ample for grouping
	return s
}

// sessionOf reads the operator's optional grouping key. A client that sends
// X-Ezllm-Session (Hermes, Claude Code via a wrapper) turns per-session cost
// and cache reuse into a single GROUP BY; one that does not leaves it empty and
// we fall back to time-window analysis. Never used for routing or auth.
func sessionOf(r *http.Request) string {
	id := r.Header.Get("X-Ezllm-Session")
	if len(id) > 128 {
		return id[:128] // bound the column; a longer id is a bug, not a feature
	}
	return id
}
