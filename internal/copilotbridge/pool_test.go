package copilotbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A real subprocess that reads requests before responding, unlike a canned
// stdout fixture. One process can serve several independent sessions.
func TestPoolCLIHelper(t *testing.T) {
	if os.Getenv("EZLLM_POOL_HELPER") != "1" {
		return
	}
	mode, record := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	out := func(v any) { b, _ := json.Marshal(v); fmt.Printf("Content-Length: %d\r\n\r\n%s", len(b), b) }
	in := bufio.NewReader(os.Stdin)
	sid := ""
	sessions := 0
	sends := 0
	tools := 0
	event := func(kind string, data any) {
		out(map[string]any{"method": "session.event", "params": map[string]any{"sessionId": sid, "event": map[string]any{"type": kind, "data": data}}})
	}
	for {
		line, err := in.ReadString('\n')
		if err != nil {
			os.Exit(0)
		}
		var n int
		fmt.Sscanf(line, "Content-Length: %d", &n)
		in.ReadString('\n')
		b := make([]byte, n)
		if _, err = io.ReadFull(in, b); err != nil {
			os.Exit(0)
		}
		var req struct {
			ID     int                        `json:"id"`
			Method string                     `json:"method"`
			Params map[string]json.RawMessage `json:"params"`
		}
		if json.Unmarshal(b, &req) != nil {
			os.Exit(3)
		}
		f, _ := os.OpenFile(record, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		fmt.Fprintf(f, "%d %s\n", os.Getpid(), req.Method)
		f.Close()
		reply := func(v any) { out(map[string]any{"id": req.ID, "result": v}) }
		if mode == "hang-"+req.Method {
			time.Sleep(time.Minute)
		}
		switch req.Method {
		case "models.list":
			var models any
			json.Unmarshal([]byte(visionCatalogFixture), &models)
			reply(models)
		case "ping":
			reply(map[string]any{"protocolVersion": channelProtocolVersion})
		case "session.create":
			sessions++
			sid = fmt.Sprintf("s%d-%d", os.Getpid(), sessions)
			if mode == "reused-id" {
				sid = fmt.Sprintf("s%d", os.Getpid())
			}
			var defs []any
			json.Unmarshal(req.Params["tools"], &defs)
			tools = len(defs)
			var allowed []any
			if json.Unmarshal(req.Params["availableTools"], &allowed) != nil || len(allowed) != tools {
				os.Exit(4)
			}
			reply(map[string]any{"sessionId": sid})
		case "session.send":
			sends++
			if mode == "cache" && sends == 2 {
				var prompt string
				json.Unmarshal(req.Params["prompt"], &prompt)
				if prompt != "User: next" {
					os.Exit(9)
				}
			}
			if mode == "image-isolation" {
				var attachments []ImageAttachment
				json.Unmarshal(req.Params["attachments"], &attachments)
				want := sessions % 2
				if len(attachments) != want {
					os.Exit(5)
				}
			}
			if mode == "die" {
				os.Exit(1)
			}
			if mode == "malformed" {
				fmt.Print("Content-Length: bad\r\n\r\n")
				continue
			}
			reply(map[string]any{"messageId": "m1"})
			if mode == "unknown" {
				sid = "other-session"
			}
			if mode == "missing" {
				sid = ""
			}
			if tools > 0 && !(mode == "cache-tools" && sends == 1) {
				var ev map[string]any
				json.Unmarshal([]byte(evBatch), &ev)
				event("assistant.message", ev["data"])
			} else {
				text := "ok"
				if mode == "cache-gate" {
					text = "pong"
				}
				if mode == "empty" {
					text = ""
				}
				event("assistant.message", map[string]any{"content": text})
				if mode != "nousage" {
					var ev map[string]any
					json.Unmarshal([]byte(evUsage), &ev)
					if strings.HasPrefix(mode, "cache") {
						count := sends
						if mode == "cache-reset" && sends > 1 {
							count = 0
						}
						ev["data"] = map[string]any{"totalNanoAiu": 1000 * count, "accountingSnapshot": map[string]any{"totalPremiumRequestCost": count, "tokenDetails": map[string]any{"input": map[string]any{"tokenCount": 10 * count}, "cache_read": map[string]any{"tokenCount": 100 * max(count-1, 0)}, "cache_write": map[string]any{"tokenCount": 90 * min(count, 1)}, "output": map[string]any{"tokenCount": 5 * count}}}}
					}
					event("session.usage_checkpoint", ev["data"])
				}
				event("session.idle", map[string]any{})
			}
		case "session.abort":
			reply(map[string]any{"success": true})
			var ev map[string]any
			json.Unmarshal([]byte(evUsage), &ev)
			if mode == "cache-tools" {
				ev["data"] = map[string]any{"totalNanoAiu": 2000, "accountingSnapshot": map[string]any{"totalPremiumRequestCost": 2, "tokenDetails": map[string]any{"input": map[string]any{"tokenCount": 20}, "cache_read": map[string]any{"tokenCount": 100}, "cache_write": map[string]any{"tokenCount": 90}, "output": map[string]any{"tokenCount": 10}}}}
			}
			event("session.usage_checkpoint", ev["data"])
			event("session.idle", map[string]any{})
		case "session.delete":
			if mode == "negative-delete" {
				reply(map[string]any{"success": false})
			} else if mode == "faildelete" {
				out(map[string]any{"id": req.ID, "error": map[string]any{"code": -1, "message": "delete failed"}})
			} else {
				reply(map[string]any{"success": true})
			}
			if mode == "stale" {
				event("assistant.message", map[string]any{"content": "STALE"})
				event("session.usage_checkpoint", map[string]any{"totalPremiumRequests": 999})
			}
		default:
			reply(map[string]any{})
		}
	}
}

func poolRig(t *testing.T, mode string) (*Runner, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	record := t.TempDir() + "/calls"
	bin := stubCLI(t, "EZLLM_POOL_HELPER=1 exec '"+exe+"' -test.run='^TestPoolCLIHelper$' -- '"+mode+"' '"+record+"'")
	r := &Runner{Bin: bin, WarmPool: true, Timeout: 3 * time.Second}
	t.Cleanup(r.Close)
	return r, record
}
func poolCalls(t *testing.T, path string) (map[string]bool, []string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pids := map[string]bool{}
	lines := strings.FieldsFunc(strings.TrimSpace(string(b)), func(r rune) bool { return r == '\n' })
	for _, line := range lines {
		pids[strings.Fields(line)[0]] = true
	}
	return pids, lines
}
func plainPoolTurn() ChannelTurn { return ChannelTurn{Model: "auto", Prompt: "ok"} }

func TestPoolReusesProcessWithFreshSessionsAndNoStaleUsage(t *testing.T) {
	r, path := poolRig(t, "stale")
	for i := 0; i < 3; i++ {
		turn := plainPoolTurn()
		if i == 1 {
			turn = channelTurn()
		}
		res, err := r.RunChannel(context.Background(), turn, nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(res.Text, "STALE") || res.Usage.PremiumRequests != 1 || res.Usage.PromptTokens != 100 {
			t.Fatalf("contaminated result: %+v", res)
		}
		if i != 1 && len(res.ToolCalls) != 0 {
			t.Fatal("tools leaked")
		}
	}
	pids, lines := poolCalls(t, path)
	if len(pids) != 1 || len(lines) != 13 {
		t.Fatalf("pids=%v calls=%v", pids, lines)
	}
	r.poolMu.Lock()
	w := r.idleWorkers[0]
	if len(w.conn.retired) != 3 {
		t.Error("sessions were reused")
	}
	r.poolMu.Unlock()
	r.Close()
	select {
	case <-w.done:
	default:
		t.Fatal("child not reaped")
	}
	if _, err := r.RunChannel(context.Background(), plainPoolTurn(), nil); err == nil {
		t.Fatal("closed runner accepted work")
	}
}
func TestPoolDiscardsUncertainWorkersAndNeverReplays(t *testing.T) {
	for _, mode := range []string{"nousage", "faildelete", "negative-delete", "empty", "unknown", "missing", "die", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			r, path := poolRig(t, mode)
			for i := 0; i < 2; i++ {
				_, err := r.RunChannel(context.Background(), plainPoolTurn(), nil)
				if mode != "nousage" && mode != "faildelete" && mode != "negative-delete" && err == nil {
					t.Fatal("invalid worker succeeded")
				}
			}
			pids, lines := poolCalls(t, path)
			sends := 0
			for _, l := range lines {
				if strings.HasSuffix(l, " session.send") {
					sends++
				}
			}
			if len(pids) != 2 || sends != 2 {
				t.Fatalf("worker reused or turn replayed: %v %v", pids, lines)
			}
		})
	}
}
func TestPoolCancellationReapsWorkerAtEveryRPC(t *testing.T) {
	for _, method := range []string{"ping", "session.create", "session.send", "session.abort", "session.delete"} {
		t.Run(method, func(t *testing.T) {
			r, path := poolRig(t, "hang-"+method)
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			turn := plainPoolTurn()
			if method == "session.abort" {
				turn = channelTurn()
			}
			if _, err := r.RunChannel(ctx, turn, nil); err == nil {
				t.Fatal("cancelled turn succeeded")
			}
			r.poolMu.Lock()
			n := len(r.workers)
			r.poolMu.Unlock()
			if n != 0 {
				t.Fatal("cancelled child retained")
			}
			_, lines := poolCalls(t, path)
			found := false
			for _, l := range lines {
				found = found || strings.HasSuffix(l, " "+method)
			}
			if !found {
				t.Fatalf("did not reach %s: %v", method, lines)
			}
		})
	}
}
func TestPoolRetiresByTurnLimitAndIdleExpiry(t *testing.T) {
	r, path := poolRig(t, "ok")
	r.PoolMaxTurns = 1
	for i := 0; i < 2; i++ {
		if _, err := r.RunChannel(context.Background(), plainPoolTurn(), nil); err != nil {
			t.Fatal(err)
		}
	}
	pids, _ := poolCalls(t, path)
	if len(pids) != 2 {
		t.Fatal("turn limit ignored")
	}
	r2, _ := poolRig(t, "ok")
	r2.PoolIdleTimeout = 20 * time.Millisecond
	if _, err := r2.RunChannel(context.Background(), plainPoolTurn(), nil); err != nil {
		t.Fatal(err)
	}
	r2.poolMu.Lock()
	w := r2.idleWorkers[0]
	r2.poolMu.Unlock()
	select {
	case <-w.done:
	case <-time.After(2 * time.Second):
		t.Fatal("idle child retained")
	}
}
func TestPoolConcurrentRequestsLeaseDifferentProcesses(t *testing.T) {
	r, path := poolRig(t, "ok")
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			turn := channelTurn()
			turn.OnTools = func(Result) error { entered <- struct{}{}; <-release; return nil }
			if _, err := r.RunChannel(context.Background(), turn, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("requests did not get separate workers")
		}
	}
	close(release)
	wg.Wait()
	pids, _ := poolCalls(t, path)
	if len(pids) != 2 {
		t.Fatal("concurrent requests shared a process")
	}
}

func TestPoolDeadIdleWorkerAndBinaryReplacement(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(strconv.FormatBool(replace), func(t *testing.T) {
			r, path := poolRig(t, "ok")
			if _, err := r.RunChannel(context.Background(), plainPoolTurn(), nil); err != nil {
				t.Fatal(err)
			}
			r.poolMu.Lock()
			w := r.idleWorkers[0]
			r.poolMu.Unlock()
			if replace {
				b, _ := os.ReadFile(r.Bin)
				replacement := r.Bin + ".new"
				os.WriteFile(replacement, b, 0700)
				if err := os.Rename(replacement, r.Bin); err != nil {
					t.Fatal(err)
				}
			} else {
				w.stop()
			}
			if _, err := r.RunChannel(context.Background(), plainPoolTurn(), nil); err != nil {
				t.Fatal(err)
			}
			pids, _ := poolCalls(t, path)
			if len(pids) != 2 {
				t.Fatal("worker not replaced")
			}
		})
	}
}

func TestPoolAgeLimitAndCloseUnblocksActiveAndQueued(t *testing.T) {
	t.Run("age", func(t *testing.T) {
		r, path := poolRig(t, "ok")
		if _, err := r.RunChannel(context.Background(), plainPoolTurn(), nil); err != nil {
			t.Fatal(err)
		}
		r.poolMu.Lock()
		r.idleWorkers[0].born = time.Now().Add(-time.Hour)
		r.poolMu.Unlock()
		if _, err := r.RunChannel(context.Background(), plainPoolTurn(), nil); err != nil {
			t.Fatal(err)
		}
		pids, _ := poolCalls(t, path)
		if len(pids) != 2 {
			t.Fatal("age limit ignored")
		}
	})
	t.Run("shutdown", func(t *testing.T) {
		r, path := poolRig(t, "hang-session.send")
		r.MaxConcurrent = 1
		r.MaxQueue = 1
		results := make(chan error, 2)
		go func() { _, err := r.RunChannel(context.Background(), plainPoolTurn(), nil); results <- err }()
		deadline := time.After(2 * time.Second)
		for {
			b, _ := os.ReadFile(path)
			if strings.Contains(string(b), "session.send") {
				break
			}
			select {
			case <-deadline:
				t.Fatal("send not reached")
			case <-time.After(time.Millisecond):
			}
		}
		go func() { _, err := r.RunChannel(context.Background(), plainPoolTurn(), nil); results <- err }()
		for r.queued.Load() == 0 {
			select {
			case <-deadline:
				t.Fatal("request not queued")
			case <-time.After(time.Millisecond):
			}
		}
		if _, err := r.RunChannel(context.Background(), plainPoolTurn(), nil); failureCode(err) != "queue_full" {
			t.Fatalf("queue full=%v", err)
		}
		r.Close()
		for i := 0; i < 2; i++ {
			select {
			case err := <-results:
				if err == nil {
					t.Fatal("shutdown work succeeded")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("shutdown left waiter")
			}
		}
	})
}

func TestBoundedCleanupJoinsTimerAndTaintsConnection(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	c := &rpcConn{reusable: true, closeRead: func() { close(started); <-release }}
	go func() { c.bounded(time.Millisecond, func() { <-started }); close(done) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timer did not run")
	}
	select {
	case <-done:
		t.Fatal("cleanup returned before its close callback ended")
	default:
	}
	unblock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timer join hung")
	}
	if c.reusable || !c.tainted {
		t.Fatal("timed-out connection could be reused")
	}
}

func TestPoolClientDeliveryFailureKeepsUsageButDiscardsWorker(t *testing.T) {
	r, path := poolRig(t, "ok")
	deliveryErr := fmt.Errorf("client write failed")
	turn := channelTurn()
	turn.OnTools = func(Result) error { return deliveryErr }
	res, err := r.RunChannel(context.Background(), turn, nil)
	if err != deliveryErr || res.Usage.PromptTokens != 100 {
		t.Fatalf("err=%v usage=%+v", err, res.Usage)
	}
	if _, err := r.RunChannel(context.Background(), plainPoolTurn(), nil); err != nil {
		t.Fatal(err)
	}
	pids, _ := poolCalls(t, path)
	if len(pids) != 2 {
		t.Fatal("worker reused after client delivery failure")
	}
}

func TestPoolRejectsReusedSessionIDBeforeSend(t *testing.T) {
	r, path := poolRig(t, "reused-id")
	if _, err := r.RunChannel(context.Background(), plainPoolTurn(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunChannel(context.Background(), plainPoolTurn(), nil); err == nil {
		t.Fatal("reused session accepted")
	}
	_, lines := poolCalls(t, path)
	sends := 0
	for _, l := range lines {
		if strings.HasSuffix(l, " session.send") {
			sends++
		}
	}
	if sends != 1 {
		t.Fatal("sent inference into an old session")
	}
	if _, err := r.RunChannel(context.Background(), plainPoolTurn(), nil); err != nil {
		t.Fatal(err)
	}
	pids, _ := poolCalls(t, path)
	if len(pids) != 2 {
		t.Fatal("reused-id worker was not discarded")
	}
}

func TestPoolHTTPImageRequestsDoNotLeakAttachmentsIntoNextSession(t *testing.T) {
	r, path := poolRig(t, "image-isolation")
	s := &Server{Runner: r, Token: "t", Models: []Model{{ID: "auto"}, {ID: "gpt-6-luna"}}}
	for _, stream := range []bool{false, true} {
		var body map[string]any
		json.Unmarshal(imageRequest([]string{imageFixture(t, false)}, "user", ""), &body)
		body["stream"] = stream
		b, _ := json.Marshal(body)
		rec := postChat(s, string(b))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ok") {
			t.Fatalf("image HTTP status=%d", rec.Code)
		}
		plain := fmt.Sprintf(`{"model":"auto","stream":%v,"messages":[{"role":"user","content":"plain"}]}`, stream)
		rec = postChat(s, plain)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ok") {
			t.Fatalf("plain HTTP status=%d", rec.Code)
		}
	}
	pids, _ := poolCalls(t, path)
	if len(pids) != 1 {
		t.Fatal("image isolation not exercised on a reused worker")
	}
}
