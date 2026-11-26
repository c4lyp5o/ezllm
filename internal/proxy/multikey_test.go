package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// keyStub answers per key: the status in byKey for that bearer token, 200
// otherwise. It counts the calls each key made.
func keyStub(t *testing.T, byKey map[string]int) (*httptest.Server, func(string) int) {
	t.Helper()
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		hits[key]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if st, ok := byKey[key]; ok {
			w.WriteHeader(st)
			_, _ = io.WriteString(w, `{"error":{"message":"no"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"key":"`+key+`"}`)
	}))
	return srv, func(k string) int { mu.Lock(); defer mu.Unlock(); return hits[k] }
}

// keyRoutes is one hop (same account, endpoint and model) once per key.
func keyRoutes(url string, keys ...string) []Route {
	out := make([]Route, len(keys))
	for i, k := range keys {
		rt := textRoute(url, "day")
		rt.KeyID, rt.KeyPlain, rt.KeyHint = int64(i+1), k, "hint-"+k
		out[i] = rt
	}
	return out
}

// A 429 on one key parks only that key for that model: the account's next key
// serves this request and the next, and the parked key is not dialed again.
func TestRateLimitedKeyParksOnlyThatKey(t *testing.T) {
	srv, hits := keyStub(t, map[string]int{"ka": http.StatusTooManyRequests})
	defer srv.Close()
	d := newDispatcher()
	d.SetRetries(0)
	d.ConfigureCooldown(time.Minute, time.Minute, 1)
	routes := keyRoutes(srv.URL, "ka", "kb")
	for i := 0; i < 2; i++ {
		req, body := reqWithBody(`{"model":"day","messages":[]}`)
		rec := httptest.NewRecorder()
		served, _, err := d.ForwardCandidates(context.Background(), rec, req, routes, body)
		if err != nil || rec.Code != http.StatusOK || served.KeyID != 2 {
			t.Fatalf("request %d: status=%d err=%v served key=%d", i, rec.Code, err, served.KeyID)
		}
	}
	if hits("ka") != 1 || hits("kb") != 2 {
		t.Fatalf("hits ka=%d kb=%d, want 1 and 2", hits("ka"), hits("kb"))
	}
	parks := d.Cooldowns()
	if len(parks) != 1 || parks[0].KeyID != 1 || parks[0].KeyHint != "hint-ka" {
		t.Fatalf("parks = %+v, want only key 1", parks)
	}
	if allowed, _ := d.acquireHop(routes[1], time.Now()); !allowed {
		t.Fatal("the account's other key is parked too")
	}
}

// A 5xx is the endpoint's fault, not the key's: the account's other keys for
// that model are not dialed, and the trail says why.
func TestServerErrorSkipsTheOtherKeys(t *testing.T) {
	srv, hits := keyStub(t, map[string]int{"ka": http.StatusBadGateway, "kb": http.StatusBadGateway})
	defer srv.Close()
	d := newDispatcher()
	d.SetRetries(0)
	routes := keyRoutes(srv.URL, "ka", "kb")
	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	rec := httptest.NewRecorder()
	_, res, _ := d.ForwardCandidates(context.Background(), rec, req, routes, body)
	if hits("kb") != 0 {
		t.Fatalf("second key was dialed after a 502 on the first (%d calls)", hits("kb"))
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("client saw %d, want the upstream's 502", rec.Code)
	}
	var noted bool
	for _, a := range res.Attempts {
		if strings.Contains(a.Note, "not tried on another key") {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("attempts = %+v, want a 'not tried on another key' note", res.Attempts)
	}
}

// A refused key (401) is the key's fault: the next key is tried.
func TestRefusedKeyFailsOverToNextKey(t *testing.T) {
	srv, hits := keyStub(t, map[string]int{"ka": http.StatusUnauthorized})
	defer srv.Close()
	d := newDispatcher()
	d.SetRetries(0)
	req, body := reqWithBody(`{"model":"day","messages":[]}`)
	rec := httptest.NewRecorder()
	if _, _, err := d.ForwardCandidates(context.Background(), rec, req, keyRoutes(srv.URL, "ka", "kb"), body); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("status=%d err=%v", rec.Code, err)
	}
	if hits("kb") != 1 {
		t.Fatalf("next key calls = %d, want 1", hits("kb"))
	}
}
