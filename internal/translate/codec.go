package translate

// Shared codec plumbing: key classification, presence-aware scalar reads, and
// the content shapes every surface needs. Kept in one place so the three
// decoders cannot drift on what "ignore" and "refuse" mean.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Key classification. A decoder checks every top-level key against these:
//
//	supported -> carried into the IR
//	ignorable -> deliberately dropped (documented, harmless for the subset)
//	refusal   -> refused with a named reason
//	unknown   -> dropped, as the legacy converters drop them. Clients add
//	             vendor extensions faster than any list can track (cache keys,
//	             safety ids, verbosity); refusing each one turned every new
//	             client release into a failed hop. A field whose loss would
//	             change the answer's contract is named in a refusal list.
func keySet(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}

var (
	chatSupported = keySet("model", "messages", "max_tokens", "max_completion_tokens",
		"temperature", "top_p", "stream", "stop", "tools", "tool_choice",
		"parallel_tool_calls", "reasoning_effort", "response_format")
	chatIgnorable = keySet("stream_options", "n", "seed", "user", "metadata", "store",
		"service_tier", "logprobs", "top_logprobs", "logit_bias",
		"presence_penalty", "frequency_penalty")

	messagesSupported = keySet("model", "system", "messages", "max_tokens", "stop_sequences",
		"temperature", "top_p", "stream", "tools", "tool_choice")
	messagesIgnorable = keySet("metadata", "anthropic_version")

	responsesSupported = keySet("model", "input", "instructions", "tools", "tool_choice",
		"max_output_tokens", "temperature", "top_p", "stream", "reasoning",
		"parallel_tool_calls", "text")
	responsesIgnorable = keySet("store", "metadata", "user", "service_tier",
		"prompt_cache_key", "truncation", "include")
)

// chatRefusals names the chat fields whose loss would change what the client
// gets back. response_format is checked by value (type "text" is harmless).
var chatRefusals = map[string]string{
	"functions":     "legacy function calling is not portable; send tools",
	"function_call": "legacy function calling is not portable; send tool_choice",
	"audio":         "audio output is not portable across wires",
}

// topKeys decodes the body into its top-level keys. A body that is not a JSON
// object is a client error, reported as such.
func topKeys(body []byte) (map[string]json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, fmt.Errorf("unmarshal request: %w", err)
	}
	return top, nil
}

// classify refuses the named keys with their own reason. Every other key
// that is neither supported nor ignorable is dropped (see above).
func classify(top map[string]json.RawMessage, what string, supported, ignorable map[string]bool, refusalReasons map[string]string) error {
	for k := range top {
		if supported[k] || ignorable[k] {
			continue
		}
		if reason, ok := refusalReasons[k]; ok {
			return unsupported(what+"."+k, reason)
		}
	}
	return nil
}

// formatType reads the "type" of a response-format object (chat
// response_format, Responses text.format). Absent is "".
func formatType(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var f struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return "", fmt.Errorf("expected a format object: %w", err)
	}
	return f.Type, nil
}

// cacheControlOf returns a content part's cache_control as raw JSON, or "".
func cacheControlOf(raw json.RawMessage) string {
	var p struct {
		CacheControl json.RawMessage `json:"cache_control"`
	}
	if json.Unmarshal(raw, &p) != nil || len(p.CacheControl) == 0 || string(p.CacheControl) == "null" {
		return ""
	}
	return string(p.CacheControl)
}

// partsCacheControl returns the last cache_control among a content array's
// parts ("" for a string or unmarked content): the breakpoint of a value the
// IR carries as one block.
func partsCacheControl(raw json.RawMessage) string {
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	cc := ""
	for _, p := range parts {
		if c := cacheControlOf(p); c != "" {
			cc = c
		}
	}
	return cc
}

// intFromRaw returns a present integer (presence is preserved, so an explicit
// 0 is not "absent").
func intFromRaw(raw json.RawMessage) (*int, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	var v int
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, true, fmt.Errorf("expected an integer: %w", err)
	}
	return &v, true, nil
}

func floatFromRaw(raw json.RawMessage) (*float64, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("expected a number: %w", err)
	}
	return &v, nil
}

func boolFromRaw(raw json.RawMessage) (*bool, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("expected a boolean: %w", err)
	}
	return &v, nil
}

func stringFromRaw(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("expected a string: %w", err)
	}
	return v, nil
}

// stopFromRaw reads the stop sequences, which the chat wire allows as either a
// bare string or an array.
func stopFromRaw(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf("stop must be a string or an array of strings: %w", err)
	}
	return many, nil
}

// textContent reads a chat-style content value: a plain string, or an array of
// parts of which only text parts are portable. Anything else (images, audio,
// files) is refused by kind, so the message says which content type we cannot
// carry.
func textContent(raw json.RawMessage, what string) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", unsupported(what, "content must be a string or an array of parts")
	}
	var out []string
	for _, p := range parts {
		var part struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(p, &part); err != nil {
			return "", unsupported(what, "unreadable content part")
		}
		switch part.Type {
		case "text", "input_text", "output_text":
			out = append(out, part.Text)
		case "":
			// Some compatible upstreams send {text: "..."} with no type.
			out = append(out, part.Text)
		default:
			return "", unsupported(what+"."+part.Type,
				"only text content is portable across the three wires")
		}
	}
	return strings.Join(out, "\n"), nil
}

// argumentsObject parses a call's JSON argument string into a value, refusing
// anything that is not a JSON object: the messages wire's tool_use.input must
// be an object, and sending "{}" for a malformed payload would hand the model a
// valid-looking call it never asked for.
func argumentsObject(args, callID string) (any, error) {
	if strings.TrimSpace(args) == "" {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return nil, unsupported("tool_call.arguments",
			fmt.Sprintf("call %q carries malformed JSON arguments", callID))
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, unsupported("tool_call.arguments",
			fmt.Sprintf("call %q arguments are not a JSON object", callID))
	}
	return v, nil
}

// marshalObjectArgs renders a tool_use input value back to a compact JSON
// string (Go sorts map keys, so this is deterministic).
func marshalObjectArgs(input any) string {
	if input == nil {
		return "{}"
	}
	b, err := json.Marshal(input)
	if err != nil {
		return "{}"
	}
	return string(b)
}
