package proxy

import (
	"context"
	"crypto/sha256"
	"net"
	"net/http"
	"net/url"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

type callerKey struct{}
type callerBridge struct {
	origin     string
	credential [32]byte
}

// WithCaller records identity only after gateway authentication. External
// headers never establish this context and never pass through to a provider.
func WithCaller(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callerKey{}, id)
}
func Caller(ctx context.Context) string { id, _ := ctx.Value(callerKey{}).(string); return id }

// RegisterCallerBridge permits identity stamping only for this authenticated
// loopback destination. Remote providers never receive gateway caller identity.
func (d *Dispatcher) RegisterCallerBridge(base, token string) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || token == "" {
		panic("proxy: invalid caller bridge registration")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		panic("proxy: caller bridge must be loopback")
	}
	d.callerBridge.Store(&callerBridge{origin: u.Scheme + "://" + u.Host, credential: sha256.Sum256([]byte(token))})
}
func (d *Dispatcher) stampCaller(ctx context.Context, r *http.Request, key string) {
	r.Header.Del(provider.BridgeCallerHeader)
	target := d.callerBridge.Load()
	if target != nil && target.origin == r.URL.Scheme+"://"+r.URL.Host && target.credential == sha256.Sum256([]byte(key)) {
		if id := Caller(ctx); id != "" {
			r.Header.Set(provider.BridgeCallerHeader, id)
		}
	}
}
