package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/registration"
	"github.com/c4lyp5o/ezllm/internal/router"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// The five service endpoints added together (images edits, moderations,
// rerank, videos, music) must each reach the upstream on THEIR path with the
// namespaced model swapped for the upstream id, and land in the ledger under
// their own surface. The model's chat probe is recorded FALSE — as it is for
// every real rerank / image / moderation model — which must not refuse it on
// its own endpoint.
func TestServiceEndpointsForwardEndToEnd(t *testing.T) {
	type hit struct{ path, model string }
	var mu sync.Mutex
	var hits []hit
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		model := ""
		if mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && strings.HasPrefix(mt, "multipart/") {
			mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
			for {
				p, err := mr.NextPart()
				if err != nil {
					break
				}
				if p.FormName() == "model" {
					v, _ := io.ReadAll(p)
					model = string(v)
				}
			}
		} else {
			var m struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(body, &m)
			model = m.Model
		}
		mu.Lock()
		hits = append(hits, hit{r.URL.Path, model})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(t.TempDir(), "t.sqlite"), BatchSize: 1, BatchWait: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := db.UpsertAccount(ctx, provider.Account{Name: "up", Namespace: "up", Kind: provider.KindOpenAICompatible, BaseURL: up.URL + "/v1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AddKey(ctx, id, "primary", "sk-up-key"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModels(ctx, id, []provider.ModelInfo{{ID: "svc-model"}}); err != nil {
		t.Fatal(err)
	}
	no := false
	if err := db.SetModelProto(ctx, id, "svc-model", provider.SurfaceOpenAI, &no); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertClientToken(ctx, "hermes", "tok-infer", "infer"); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	registry := provider.NewRegistry(client)
	s := New(Options{
		Auth: db, Resolver: router.New(db), Dispatcher: proxy.NewDispatcher(client, registry),
		DB: db, MaxBodyMiB: 1, Registry: registry, Tester: registration.NewTester(client, registry, db),
	})

	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	_ = mw.WriteField("model", "up/svc-model")
	_ = mw.WriteField("prompt", "make it red")
	fw, _ := mw.CreateFormFile("image", "a.png")
	_, _ = fw.Write(tinyPNG())
	_ = mw.Close()

	cases := []struct {
		route, upPath, surface, ctype string
		body                          []byte
	}{
		{"/v1/images/edits", "/v1/images/edits", "images_edit", mw.FormDataContentType(), mp.Bytes()},
		{"/v1/moderations", "/v1/moderations", "moderations", "application/json", []byte(`{"model":"up/svc-model","input":"hi"}`)},
		{"/v1/rerank", "/v1/rerank", "rerank", "application/json", []byte(`{"model":"up/svc-model","query":"q","documents":["a","b"]}`)},
		{"/v1/videos/generations", "/v1/videos/generations", "videos_gen", "application/json", []byte(`{"model":"up/svc-model","prompt":"p"}`)},
		{"/v1/music/generations", "/v1/music/generations", "music_gen", "application/json", []byte(`{"model":"up/svc-model","prompt":"p"}`)},
	}
	for i, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, tc.route, bytes.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer tok-infer")
		req.Header.Set("Content-Type", tc.ctype)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d — %s", tc.route, rec.Code, rec.Body.String())
			continue
		}
		mu.Lock()
		got := hits[len(hits)-1]
		n := len(hits)
		mu.Unlock()
		if n != i+1 || got.path != tc.upPath || got.model != "svc-model" {
			t.Errorf("%s: upstream saw %+v (hit %d), want path %s model svc-model", tc.route, got, n, tc.upPath)
		}
	}

	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	calls, err := db.RecentCalls(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range calls {
		seen[string(c.Surface)] = true
	}
	for _, tc := range cases {
		if !seen[tc.surface] {
			t.Errorf("ledger has no %s row (surfaces seen: %v)", tc.surface, seen)
		}
	}
}
