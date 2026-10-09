package provider

import (
	"net/http"
	"testing"
)

func TestGeminiOpenAIKindAndRejectStatuses(t *testing.T) {
	a := NewGeminiOpenAI(http.DefaultClient)
	if a.Kind() != KindGeminiOpenAI {
		t.Fatalf("kind = %q, want %q", a.Kind(), KindGeminiOpenAI)
	}
	r, ok := a.(AuthRejectStatuser)
	if !ok {
		t.Fatal("gemini adapter must implement AuthRejectStatuser")
	}
	got := map[int]bool{}
	for _, s := range r.AuthRejectStatuses() {
		got[s] = true
	}
	for _, want := range []int{400, 401, 403} {
		if !got[want] {
			t.Errorf("missing reject status %d", want)
		}
	}
}

func TestOpenAICompatibleKeepsDefaultRejectSet(t *testing.T) {
	a := NewOpenAICompatible(http.DefaultClient)
	if _, ok := a.(AuthRejectStatuser); ok {
		t.Fatal("openai-compatible must not opt into 400-as-auth")
	}
}

func TestGeminiKindParses(t *testing.T) {
	k, err := ParseKind("gemini-openai")
	if err != nil || k != KindGeminiOpenAI {
		t.Fatalf("ParseKind = %q, %v", k, err)
	}
}
