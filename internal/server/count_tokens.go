package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"unicode/utf8"

	"github.com/c4lyp5o/ezllm/internal/jsonbody"
	"github.com/c4lyp5o/ezllm/internal/provider"
)

// countTokensServed counts POST /v1/messages/count_tokens answers by source,
// exported on /metrics as ezllm_count_tokens_total.
var countTokensServed struct{ upstream, estimate atomic.Int64 }

// countTokensSourceHeader says where a count came from: "upstream" (an
// Anthropic-wire hop answered) or "estimate" (the local estimator).
const countTokensSourceHeader = "X-Ezllm-Token-Count"

// countTokens serves the Anthropic token-counting endpoint Claude Code calls
// for context accounting. The model resolves like a Messages request; the
// first native Anthropic hop that has the endpoint answers. Everything else
// (bridges, translated hops, gateways without it) gets the shared estimator
// over the prompt — the same one compression uses — so the client never
// sees a 404. Counting costs nothing upstream: no ledger row, no budget.
func (s *Server) countTokens(w http.ResponseWriter, r *http.Request) {
	liftDeadline := limitBodyRead(w, inferenceBodyTimeout)
	body, tooLarge, err := readBody(r, s.maxBody)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "could not read body: "+err.Error())
		return
	}
	if tooLarge {
		writeErr(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
			fmt.Sprintf("request body exceeds %d bytes", s.maxBody))
		return
	}
	liftDeadline()
	doc := jsonbody.Open(body)
	if doc.Err() != nil || doc.Map() == nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "body must be a JSON object")
		return
	}
	requested, err := extractModelDoc(doc)
	if err != nil || requested == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	client := clientName(r)
	if !s.allowModel(w, r, client, requested) {
		return
	}
	routes, err := s.resolver.ResolveCandidates(r.Context(), requested, provider.SurfaceAnthropic, client)
	if err != nil {
		writeResolveErr(w, err)
		return
	}
	if s.dispatcher != nil {
		if n, _, ok := s.dispatcher.CountTokens(r.Context(), r, routes, doc); ok {
			countTokensServed.upstream.Add(1)
			w.Header().Set(countTokensSourceHeader, "upstream")
			writeJSON(w, http.StatusOK, map[string]int64{"input_tokens": n})
			return
		}
	}
	countTokensServed.estimate.Add(1)
	w.Header().Set(countTokensSourceHeader, "estimate")
	writeJSON(w, http.StatusOK, map[string]int64{"input_tokens": estimatePromptTokens(doc.Map())})
}

// estimatePromptTokens sizes what the model would read: system, messages and
// tool definitions, as JSON. It must lean high, the safe side for a client
// deciding when to compact its context. compress.EstimateTokens (4 characters
// per token) is too low here: Anthropic counted 25,636 tokens for 60,000
// characters of Markdown and 4,329 for a 10 KB Go file (claude-sonnet-5-5,
// 2026-11-21), about 2.35 characters per token, so this uses 2.
func estimatePromptTokens(m map[string]any) int64 {
	prompt := map[string]any{}
	for _, k := range []string{"system", "messages", "tools", "tool_choice"} {
		if v, ok := m[k]; ok {
			prompt[k] = v
		}
	}
	raw, err := json.Marshal(prompt)
	if err != nil {
		return 1
	}
	return int64(utf8.RuneCount(raw)/estimateCharsPerToken) + 1
}

// estimateCharsPerToken is the estimate's characters per token (see above).
const estimateCharsPerToken = 2
