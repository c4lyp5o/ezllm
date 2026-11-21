package bridgeutil

import "testing"

func TestOpenAIErrorEnvelopeShape(t *testing.T) {
	got := string(OpenAIErrorEnvelope("invalid_request_error", "bad \"model\"", "unknown_model"))
	want := `{"error":{"code":"unknown_model","message":"bad \"model\"","type":"invalid_request_error"}}`
	if got != want {
		t.Fatalf("envelope = %s, want %s", got, want)
	}
}
