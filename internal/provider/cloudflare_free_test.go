package provider

import (
	"strings"
	"testing"
)

const cfBase = "https://api.cloudflare.com/client/v4/accounts/abc123/ai/v1"

func TestCloudflareFreeTable(t *testing.T) {
	if len(cloudflareFree) != 65 {
		t.Fatalf("free list has %d entries, want the 65 supplied", len(cloudflareFree))
	}
	text := 0
	for id, task := range cloudflareFree {
		if !strings.HasPrefix(id, "@cf/") || task == "" {
			t.Errorf("malformed entry %q → %q", id, task)
		}
		if task == cloudflareTextTask {
			text++
		}
	}
	if text != 31 {
		t.Errorf("%d text-generation entries, want 31", text)
	}
	for id, want := range map[string]string{
		"@cf/openai/gpt-oss-120b":              "Text Generation",
		"@cf/baai/bge-m3":                      "Text Embeddings",
		"@cf/deepgram/nova-3":                  "Automatic Speech Recognition",
		"@cf/myshell-ai/melotts":               "Text-to-Speech",
		"@cf/black-forest-labs/flux-1-schnell": "Text-to-Image",
	} {
		if got, ok := CloudflareFreeTask(id); !ok || got != want {
			t.Errorf("CloudflareFreeTask(%q) = %q, %v; want %q", id, got, ok, want)
		}
	}
	if _, ok := CloudflareFreeTask("@cf/meta/llama-3.1-70b-instruct"); ok {
		t.Error("a model not on the list must not be free")
	}
}

func TestIsCloudflareFreeChatModel(t *testing.T) {
	const chat, embed = "@cf/openai/gpt-oss-120b", "@cf/baai/bge-m3"
	cases := []struct {
		name, kind, base, model string
		want                    bool
	}{
		{"free chat on workers ai", "openai-compatible", cfBase, chat, true},
		{"free embedding is not a chat hop", "openai-compatible", cfBase, embed, false},
		{"unlisted model", "openai-compatible", cfBase, "@cf/meta/not-listed", false},
		{"http scheme", "openai-compatible", strings.Replace(cfBase, "https", "http", 1), chat, false},
		{"other host", "openai-compatible", "https://evil.example/client/v4/accounts/abc123/ai/v1", chat, false},
		{"not the /ai/v1 prefix", "openai-compatible", "https://api.cloudflare.com/client/v4/accounts/abc123", chat, false},
		{"wrong kind", "anthropic-compatible", cfBase, chat, false},
	}
	for _, tc := range cases {
		if got := IsCloudflareFreeChatModel(tc.kind, tc.base, tc.model); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
