package geminiweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// genPart builds one wrb.fr frame payload for StreamGenerate:
//
//	["wrb.fr", <inner json string>, null, ...]
//
// where the inner JSON is the part_body array with metadata@1,
// candidates@4, final-chunk@25.
func genPart(metadata []any, candidates []any, finalChunk any) string {
	body := make([]any, 26)
	if metadata != nil {
		body[1] = metadata
	}
	if candidates != nil {
		body[4] = candidates
	}
	if finalChunk != nil {
		body[25] = finalChunk
	}
	bodyJSON, _ := json.Marshal(body)
	// ["wrb.fr", rpcid, <body json string>, ...] — the body sits at slot 2,
	// exactly where the stream parser reads it on the real wire.
	part, _ := json.Marshal([]any{"wrb.fr", "haW8u6", string(bodyJSON), nil, nil, nil, nil, "generic"})
	return wrbFrame(string(part))
}

// errPart builds a frame carrying an upstream error code at
// part[5][2][0][1][0].
func errPart(code int) string {
	part := []any{
		"wrb.fr",
		"null",
		nil,
		nil,
		nil,
		// part[5][2][0][1][0]: [5][2] = [ [nil, [code]] ] so the path
		// 5 -> 2 -> 0 -> 1 -> 0 lands on the code.
		[]any{nil, nil, []any{[]any{nil, []any{code}}}},
		"generic",
	}
	b, _ := json.Marshal(part)
	return wrbFrame(string(b))
}

// candidate builds one entry of part_json[4].
func candidate(rcid, text, thoughts string, completed bool) []any {
	c := make([]any, 38)
	c[0] = rcid
	c[1] = []any{text}
	if thoughts != "" {
		c[37] = []any{[]any{thoughts}}
	}
	if completed {
		c[8] = []any{2}
	}
	return c
}

func testModel() *Model {
	m := Model{ID: "e6fa609f3b649346", Name: "gemini-pro", ModelNumber: 9, Capacity: 4, CapacityField: 12}
	return &m
}

func newGenerateServer(t *testing.T, frames ...string) *httptest.Server {
	t.Helper()
	body := strings.Join(frames, "")
	mux := http.NewServeMux()
	mux.HandleFunc(endpointInit, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, initPageFixture)
	})
	mux.HandleFunc(endpointGenerate, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("at") != "tok-abc123" {
			t.Errorf("missing access token")
		}
		if !strings.HasPrefix(r.Form.Get("f.req"), "[null,") {
			t.Errorf("f.req must be the double-encoded envelope, got %q", r.Form.Get("f.req"))
		}
		if r.Header.Get(modelHeaderKey) == "" {
			t.Errorf("model header missing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, body)
	})
	return httptest.NewServer(mux)
}

func initGenerateClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c := New(cookie{psid: "abc", psidts: "def"})
	c.base = srv.URL
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGenerateStreamDeltas(t *testing.T) {
	srv := newGenerateServer(t,
		genPart([]any{"cid-1", "rid-1", "rcid-1"}, []any{candidate("rcid-1", "Hel", "", false)}, nil),
		genPart(nil, []any{candidate("rcid-1", "Hello ", "", false)}, nil),
		genPart(nil, []any{candidate("rcid-1", "Hello world", "", true)}, nil),
		genPart(nil, []any{candidate("rcid-1", "Hello world", "", true)}, "cid-1"),
	)
	defer srv.Close()

	c := New(cookie{psid: "abc", psidts: "def"})
	c.base = srv.URL
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	var deltas []string
	var thoughtDeltas []string
	res, err := c.Generate(context.Background(), GenerateOptions{
		Model:  testModel(),
		Prompt: "hi",
	}, func(d Delta) {
		deltas = append(deltas, d.Text)
		if d.Thoughts != "" {
			thoughtDeltas = append(thoughtDeltas, d.Thoughts)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	// "Hel" -> "Hello " -> "Hello world" is prefix growth: the deltas are the
	// appended tails, not the cumulative values.
	if want := "Hel|lo |world"; strings.Join(deltas, "|") != want {
		t.Errorf("deltas = %q, want %q", strings.Join(deltas, "|"), want)
	}
	if res.Text != "Hello world" {
		t.Errorf("final text = %q", res.Text)
	}
	if res.CID != "cid-1" || res.RID != "rid-1" || res.RCID != "rcid-1" {
		t.Errorf("metadata = %+v", res)
	}
	if !res.Completed {
		t.Error("expected Completed")
	}
}

func TestGenerateThoughtsDelta(t *testing.T) {
	srv := newGenerateServer(t,
		genPart(nil, []any{candidate("r", "", "think1", false)}, nil),
		genPart(nil, []any{candidate("r", "", "think12", true)}, "c"),
	)
	defer srv.Close()
	c := New(cookie{psid: "x"})
	c.base = srv.URL
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	var thoughts []string
	res, err := c.Generate(context.Background(), GenerateOptions{Model: testModel(), Prompt: "p"}, func(d Delta) {
		if d.Thoughts != "" {
			thoughts = append(thoughts, d.Thoughts)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(thoughts, "|") != "think1|2" {
		t.Errorf("thought deltas = %q", thoughts)
	}
	if res.Thoughts != "think12" {
		t.Errorf("final thoughts = %q", res.Thoughts)
	}
}

// TestGenerateSplitDelivery feeds the identical stream one byte at a time:
// boundaries land mid-prefix, mid-rune (emoji) and mid-frame.
func TestGenerateSplitDelivery(t *testing.T) {
	stream := genPart([]any{"c", "r", "rc"}, []any{candidate("rc", "héllo 🌈", "", true)}, "c")
	mux := http.NewServeMux()
	mux.HandleFunc(endpointInit, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, initPageFixture)
	})
	mux.HandleFunc(endpointGenerate, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for i := 0; i < len(stream); i++ {
			w.Write([]byte{stream[i]})
			if fl != nil {
				fl.Flush()
			}
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(cookie{psid: "x"})
	c.base = srv.URL
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	var deltas []string
	res, err := c.Generate(context.Background(), GenerateOptions{Model: testModel(), Prompt: "p"}, func(d Delta) {
		if d.Text != "" {
			deltas = append(deltas, d.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "héllo 🌈" {
		t.Errorf("split-delivery text = %q (emoji rune split mid-stream)", res.Text)
	}
	if len(deltas) != 1 || deltas[0] != "héllo 🌈" {
		t.Errorf("deltas = %q", deltas)
	}
}

func TestGenerateUpstreamError(t *testing.T) {
	srv := newGenerateServer(t, errPart(1037))
	defer srv.Close()
	c := New(cookie{psid: "x"})
	c.base = srv.URL
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := c.Generate(context.Background(), GenerateOptions{Model: testModel(), Prompt: "p"}, func(Delta) {})
	var ue ErrUpstream
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
	if ue.Code != 1037 || !strings.Contains(ue.Error(), "usage limit") {
		t.Errorf("upstream err = %+v", ue)
	}
}

func TestGenerateArtifactCleanup(t *testing.T) {
	// A googleusercontent artifact link embedded mid-text must be stripped.
	raw := "before https://googleusercontent.com/image_collection/image_retrieval/42 after"
	srv := newGenerateServer(t, genPart(nil, []any{candidate("r", raw, "", true)}, "c"))
	defer srv.Close()
	c := New(cookie{psid: "x"})
	c.base = srv.URL
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := c.Generate(context.Background(), GenerateOptions{Model: testModel(), Prompt: "p"}, func(Delta) {})
	if err != nil {
		t.Fatal(err)
	}
	// Faithful to the reference: ARTIFACTS_RE.sub("") removes the URL and
	// nothing else — the surrounding spaces stay (double space included).
	if res.Text != "before  after" {
		t.Errorf("cleaned text = %q, want %q", res.Text, "before  after")
	}
}

func TestBuildInnerReqListGolden(t *testing.T) {
	c := New(cookie{psid: "x"})
	m := testModel()
	inner, err := c.buildInnerReqList(GenerateOptions{
		Model:    m,
		Prompt:   "hello",
		Language: "en",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inner) != 81 {
		t.Fatalf("len = %d, want 81", len(inner))
	}
	// message content
	mc, ok := inner[slotMessageContent].([]any)
	if !ok || mc[0] != "hello" || mc[1] != 0 || mc[3].([]any) == nil {
		t.Errorf("message_content = %+v", inner[slotMessageContent])
	}
	if inner[slotStreaming] != 1 {
		t.Errorf("streaming slot = %v", inner[slotStreaming])
	}
	if inner[slotLanguage].([]any)[0] != "en" {
		t.Errorf("language = %v", inner[slotLanguage])
	}
	if inner[slotSeventyNine] != m.ModelNumber {
		t.Errorf("model number slot = %v, want %d", inner[slotSeventyNine], m.ModelNumber)
	}
	if inner[slotEighty] != 1 {
		t.Errorf("thinking slot = %v, want 1", inner[slotEighty])
	}
	// Metadata default.
	meta := inner[slotMetadata].([]any)
	if len(meta) != 10 || meta[0] != "" {
		t.Errorf("default metadata = %+v", meta)
	}
	// Temporary flag off by default, on when asked.
	inner2, _ := c.buildInnerReqList(GenerateOptions{Model: m, Prompt: "p", Temporary: true})
	if inner2[slotTemporary] != 1 {
		t.Error("temporary flag not set")
	}
	// Extended thinking lands in slot 80 AND the model header.
	inner3, _ := c.buildInnerReqList(GenerateOptions{Model: m, Prompt: "p", ExtendedThinking: true})
	if inner3[slotEighty] != 2 {
		t.Errorf("extended thinking slot = %v, want 2", inner3[slotEighty])
	}
	// Chat metadata passthrough.
	inner4, _ := c.buildInnerReqList(GenerateOptions{Model: m, Prompt: "p", ChatMetadata: []string{"cid", "rid", "rcid"}})
	m4 := inner4[slotMetadata].([]any)
	if m4[0] != "cid" || m4[1] != "rid" || m4[2] != "rcid" {
		t.Errorf("chat metadata = %+v", m4)
	}
	// uuid slot populated, upper-case.
	u, _ := inner[slotFiftyNine].(string)
	if len(u) != 36 || u != strings.ToUpper(u) {
		t.Errorf("uuid slot = %q", u)
	}
}

func TestModelHeaderTurnMutation(t *testing.T) {
	m := testModel()
	h := (&Client{}).modelHeadersWithTurn(*m, "sid-77", false)
	// header = catalog header + [effort=1, sessionID]
	var arr []any
	if err := json.Unmarshal([]byte(h[modelHeaderKey]), &arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != 17 { // 15 + 2 appended
		t.Fatalf("header len = %d, want 17", len(arr))
	}
	if arr[15] != float64(1) || arr[16] != "sid-77" {
		t.Errorf("appended = %v %v", arr[15], arr[16])
	}
	h2 := (&Client{}).modelHeadersWithTurn(*m, "", true)
	var arr2 []any
	json.Unmarshal([]byte(h2[modelHeaderKey]), &arr2)
	if arr2[15] != float64(2) || arr2[16] != "" {
		t.Errorf("extended-thinking append = %v %v", arr2[15], arr2[16])
	}
}

func TestDoubleEncodeShape(t *testing.T) {
	got := doubleEncode([]any{"x", 1})
	var outer []any
	if err := json.Unmarshal([]byte(got), &outer); err != nil {
		t.Fatal(err)
	}
	if len(outer) != 2 || outer[0] != nil {
		t.Fatalf("envelope = %v", outer)
	}
	innerStr, _ := outer[1].(string)
	if !strings.Contains(innerStr, `"x"`) {
		t.Errorf("inner = %q", innerStr)
	}
}

// TestGenerateNonStreamingFailIs200Check ensures a non-200 generate is
// surfaced with the body (operator visibility), not a bare error.
func TestGenerateNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, "upstream exploded")
	}))
	defer srv.Close()
	c := New(cookie{psid: "x"})
	c.base = srv.URL
	_, err := c.Generate(context.Background(), GenerateOptions{Model: testModel(), Prompt: "p"}, func(Delta) {})
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "exploded") {
		t.Fatalf("err = %v", err)
	}
}

// TestGenerateContextCancel: cancelling the context mid-stream aborts.
func TestGenerateContextCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, genPart(nil, []any{candidate("r", "partial", "", false)}, nil))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	defer close(release)
	c := New(cookie{psid: "x"})
	c.base = srv.URL
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Generate(ctx, GenerateOptions{Model: testModel(), Prompt: "p"}, func(Delta) {})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected context error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not abort Generate")
	}
}
