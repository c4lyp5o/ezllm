package provider

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A provider that answers 307 to another origin must not receive the
// key header or the prompt there; a same-origin redirect still follows.
func TestSameOriginRedirectKeepsKeysAndBodyHome(t *testing.T) {
	var leakedKey, leakedBody string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leakedKey = r.Header.Get("X-Api-Key")
		b, _ := io.ReadAll(r.Body)
		leakedBody = string(b)
	}))
	defer collector.Close()
	var finalKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, collector.URL+"/steal", http.StatusTemporaryRedirect)
		case "/moved":
			http.Redirect(w, r, "/v1/messages", http.StatusPermanentRedirect)
		default:
			finalKey = r.Header.Get("X-Api-Key")
		}
	}))
	defer upstream.Close()

	client := &http.Client{CheckRedirect: SameOriginRedirect}
	post := func(path string) error {
		req, _ := http.NewRequest("POST", upstream.URL+path, strings.NewReader("PRIVATE_PROMPT_MARKER"))
		req.Header.Set("X-Api-Key", "dummy-provider-key")
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := post("/away"); err == nil || !strings.Contains(err.Error(), "another origin") {
		t.Errorf("cross-origin redirect err = %v, want a refusal", err)
	}
	if leakedKey != "" || leakedBody != "" {
		t.Errorf("collector received key=%q body=%q", leakedKey, leakedBody)
	}
	if err := post("/moved"); err != nil || finalKey != "dummy-provider-key" {
		t.Errorf("same-origin redirect: err=%v key=%q", err, finalKey)
	}
}
