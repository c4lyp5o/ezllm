package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

func TestAccountingSourceUsageMatrix(t *testing.T) {
	surfaces := []provider.Surface{provider.SurfaceOpenAI, provider.SurfaceAnthropic, provider.SurfaceResponses}
	usage := map[provider.Surface]string{
		provider.SurfaceOpenAI:    `{"prompt_tokens":1050,"completion_tokens":42,"prompt_tokens_details":{"cached_tokens":900,"cache_write_tokens":50},"completion_tokens_details":{"reasoning_tokens":7}}`,
		provider.SurfaceAnthropic: `{"input_tokens":1050,"output_tokens":42,"cache_read_input_tokens":900,"cache_creation_input_tokens":50,"output_tokens_details":{"thinking_tokens":7}}`,
		provider.SurfaceResponses: `{"input_tokens":1050,"output_tokens":42,"input_tokens_details":{"cached_tokens":900,"cache_write_tokens":50},"output_tokens_details":{"reasoning_tokens":7}}`,
	}
	for _, stream := range []bool{false, true} {
		for _, client := range surfaces {
			for _, wire := range surfaces {
				name := fmt.Sprintf("%s_from_%s_stream_%t", client, wire, stream)
				t.Run(name, func(t *testing.T) {
					up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						io.Copy(io.Discard, r.Body)
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							src := upstreamStream(wire)
							src = strings.ReplaceAll(src, `{"prompt_tokens":10,"completion_tokens":5}`, usage[wire])
							src = strings.ReplaceAll(src, `{"input_tokens":10,"output_tokens":5}`, usage[wire])
							src = strings.ReplaceAll(src, `{"input_tokens":10,"output_tokens":1}`, usage[wire])
							src = strings.ReplaceAll(src, `"output_tokens":5`, `"output_tokens":42`)
							io.WriteString(w, src)
						} else {
							w.Header().Set("Content-Type", "application/json")
							var obj map[string]json.RawMessage
							json.Unmarshal([]byte(upstreamResponse(wire)), &obj)
							obj["usage"] = json.RawMessage(usage[wire])
							json.NewEncoder(w).Encode(obj)
						}
					}))
					defer up.Close()
					body := clientBody(client, false)
					if stream {
						var m map[string]any
						json.Unmarshal([]byte(body), &m)
						m["stream"] = true
						b, _ := json.Marshal(m)
						body = string(b)
					}
					d := newDispatcher()
					d.SetRetries(0)
					rec := httptest.NewRecorder()
					req := httptest.NewRequest("POST", "/", strings.NewReader(body))
					_, res, err := d.ForwardCandidates(context.Background(), rec, req, []Route{wireRoute(up.URL, client, wire)}, []byte(body))
					if err != nil || res.Usage == nil || rec.Code != 200 {
						t.Fatalf("route failed: err=%v result=%+v body=%s", err, res, rec.Body.String())
					}
					u := res.Usage
					var expected map[string]any
					if err := json.Unmarshal([]byte(usage[wire]), &expected); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(u.Raw, expected) {
						t.Fatalf("source evidence changed: %s", store.RawJSON(u.Raw))
					}
					mismatch := u.In != 100 || u.Out != 42 || u.CachedRead != 900 || u.CachedWrite != 50 || u.Reasoning != 7
					t.Logf("cold=%d out=%d read=%d write=%d reasoning=%d mismatch=%t raw=%s", u.In, u.Out, u.CachedRead, u.CachedWrite, u.Reasoning, mismatch, store.RawJSON(u.Raw))
					if mismatch {
						t.Fatal("source usage not preserved")
					}
				})
			}
		}
	}
}

func TestAccountingUsageSurvivesConversionFailure(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_c","type":"message","role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"call","name":"f","input":[]}],"usage":{"input_tokens":100,"output_tokens":42}}`)
	}))
	defer up.Close()
	body := chatBody(false)
	rec := httptest.NewRecorder()
	_, res, err := newDispatcher().ForwardCandidates(context.Background(), rec, httptest.NewRequest("POST", "/", strings.NewReader(body)), []Route{wireRoute(up.URL, provider.SurfaceOpenAI, provider.SurfaceAnthropic)}, []byte(body))
	if err != nil || rec.Code != 502 || res.Usage == nil || res.Usage.In != 100 || res.Usage.Out != 42 {
		t.Fatalf("conversion fault lost spend: status=%d usage=%+v err=%v", rec.Code, res.Usage, err)
	}
}
