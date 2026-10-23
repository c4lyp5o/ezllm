package provider

import (
	"errors"
	"fmt"
	"net/http"
)

// SameOriginRedirect is the CheckRedirect policy for every client that carries
// provider credentials. net/http drops Authorization and Cookie on a hop to
// another host but keeps provider-specific key headers (x-api-key,
// x-goog-api-key, ...), and a 307/308 replays the whole prompt body, so a
// redirect may only stay on the origin the request was sent to. Same-origin
// hops (a trailing-slash fix, a path move) keep working.
func SameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	first := via[0].URL
	if req.URL.Scheme != first.Scheme || req.URL.Host != first.Host {
		return fmt.Errorf("refused redirect from %s://%s to another origin %s://%s",
			first.Scheme, first.Host, req.URL.Scheme, req.URL.Host)
	}
	return nil
}
