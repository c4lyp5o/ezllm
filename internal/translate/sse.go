package translate

import (
	"encoding/json"
)

// mustMarshalString JSON-encodes s; it cannot fail for a string.
func mustMarshalString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// --- upstream chunk decode types (OpenAI streaming shape) -------------

type openaiToolDelta struct {
	Index    int              `json:"index"`
	ID       string           `json:"id,omitempty"`
	Function *openaiFuncDelta `json:"function,omitempty"`
}

type openaiFuncDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}
