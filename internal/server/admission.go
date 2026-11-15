package server

import (
	"context"
	"net/http"
	"sync"
)

// RequestLimits bounds inference work before request-body allocation. These
// limits include active streams; zero fields select bounded defaults.
type RequestLimits struct {
	GlobalInflight    int
	PerClientInflight int
	BodyMemoryMiB     int
}

type admission struct {
	mu                    sync.Mutex
	limits                RequestLimits
	maxBody, budget, used int64
	active                int
	clients               map[string]int
}
type requestLease struct {
	gate   *admission
	client string
	bytes  int64
	once   sync.Once
}

func newAdmission(limits RequestLimits, maxBody int64) *admission {
	if limits.GlobalInflight == 0 {
		limits.GlobalInflight = 32
	}
	if limits.PerClientInflight == 0 {
		limits.PerClientInflight = 8
		if limits.PerClientInflight > limits.GlobalInflight {
			limits.PerClientInflight = limits.GlobalInflight
		}
	}
	if limits.BodyMemoryMiB == 0 {
		limits.BodyMemoryMiB = 256
	}
	if limits.GlobalInflight < 1 || limits.GlobalInflight > 4096 || limits.PerClientInflight < 1 || limits.PerClientInflight > limits.GlobalInflight || limits.BodyMemoryMiB < 1 || limits.BodyMemoryMiB > 4096 || int64(limits.BodyMemoryMiB)<<20 < maxBody {
		panic("server: invalid inference request limits")
	}
	return &admission{limits: limits, maxBody: maxBody, budget: int64(limits.BodyMemoryMiB) << 20, clients: make(map[string]int)}
}

// admit does not queue or read the body. A declared Content-Length is reserved
// exactly: net/http never reads past it, so it bounds the allocation. A body of
// unknown length (chunked, or HTTP/2 without the header) reserves the whole
// request cap. Once read, shrink keeps only the actual input bytes charged until
// the handler and stream finish.
func (s *Server) admit(w http.ResponseWriter, r *http.Request) *requestLease {
	if r.ContentLength > s.maxBody {
		refuseBody(w, r)
		writeErr(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body exceeds the configured limit")
		return nil
	}
	client := clientName(r)
	gate := s.admission
	gate.mu.Lock()
	reserve := gate.maxBody
	if r.ContentLength >= 0 {
		reserve = r.ContentLength
	}
	if gate.active >= gate.limits.GlobalInflight || gate.clients[client] >= gate.limits.PerClientInflight || reserve > gate.budget-gate.used {
		gate.mu.Unlock()
		refuseBody(w, r)
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "inference request capacity reached; retry shortly")
		return nil
	}
	gate.active++
	gate.clients[client]++
	gate.used += reserve
	gate.mu.Unlock()
	return &requestLease{gate: gate, client: client, bytes: reserve}
}

// Reject without draining an attacker-controlled HTTP/1 body. HTTP/2 ends only
// this stream; net/http handles the connection-specific response header there.
func refuseBody(w http.ResponseWriter, r *http.Request) {
	r.Close = true
	if r.ProtoMajor == 1 {
		w.Header().Set("Connection", "close")
	}
}
func (l *requestLease) shrink(size int64) {
	if size < 0 || size >= l.bytes {
		return
	}
	l.gate.mu.Lock()
	l.gate.used -= l.bytes - size
	l.bytes = size
	l.gate.mu.Unlock()
}
func (l *requestLease) release() {
	l.once.Do(func() {
		g := l.gate
		g.mu.Lock()
		defer g.mu.Unlock()
		g.used -= l.bytes
		g.active--
		g.clients[l.client]--
		if g.clients[l.client] == 0 {
			delete(g.clients, l.client)
		}
	})
}

type requestLeaseKey struct{}

func withRequestLease(ctx context.Context, l *requestLease) context.Context {
	return context.WithValue(ctx, requestLeaseKey{}, l)
}
func requestLeaseOf(ctx context.Context) *requestLease {
	l, _ := ctx.Value(requestLeaseKey{}).(*requestLease)
	return l
}
