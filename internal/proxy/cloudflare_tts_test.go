package proxy

import (
	"encoding/json"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func TestCloudflareTTSModelPayload(t *testing.T) {
	for _, model := range []string{"@cf/deepgram/aura-1", "@cf/deepgram/aura-2-en", "@cf/deepgram/aura-2-es", "@cf/myshell-ai/melotts"} {
		for _, input := range []string{`{"input":"first phrase","text":"fallback","voice":"alloy"}`, `{"text":"second phrase"}`} {
			t.Run(model+"/"+input, func(t *testing.T) {
				rt := Route{Account: provider.Account{BaseURL: "https://api.cloudflare.com/client/v4/accounts/test/ai"}, Model: model, Surface: provider.SurfaceAudioTTS}
				got, err := prepareHopBodyWire([]byte(input), rt, nil)
				if err != nil {
					t.Fatal(err)
				}
				var payload map[string]any
				if err := json.Unmarshal(got, &payload); err != nil {
					t.Fatal(err)
				}
				field := "text"
				if model == "@cf/myshell-ai/melotts" {
					field = "prompt"
				}
				want := "first phrase"
				if input == `{"text":"second phrase"}` {
					want = "second phrase"
				}
				if len(payload) != 1 || payload[field] != want {
					t.Fatalf("payload = %s, want only %s with translated input", got, field)
				}
			})
		}
	}
}
