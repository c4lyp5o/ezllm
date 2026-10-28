package server

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func loginAllowed(ip string) bool {
	loginRateLimiter.mu.Lock()
	defer loginRateLimiter.mu.Unlock()
	b := loginRateLimiter.attempts[ip]
	if b == nil {
		return true
	}
	cutoff := time.Now().Add(-loginWindow)
	n := b.inFlight
	for _, t := range b.failures {
		if t.After(cutoff) {
			n++
		}
	}
	return n < maxLoginAttempts
}

func clearLoginAttempts(ip string) {
	loginRateLimiter.mu.Lock()
	defer loginRateLimiter.mu.Unlock()
	if b := loginRateLimiter.attempts[ip]; b != nil {
		b.failures = nil
		if b.inFlight == 0 {
			delete(loginRateLimiter.attempts, ip)
		}
	}
}

func TestLoginLimiterBoundsConcurrentAndExpiredSources(t *testing.T) {
	const ip = "limiter-concurrent-test"
	clearLoginAttempts(ip)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	var releasesMu sync.Mutex
	var releases []func(bool, bool)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			finish, ok := reserveLogin(ip)
			if ok {
				admitted.Add(1)
				releasesMu.Lock()
				releases = append(releases, finish)
				releasesMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if got := admitted.Load(); got != maxLoginAttempts {
		t.Fatalf("admitted %d, want %d", got, maxLoginAttempts)
	}
	for _, finish := range releases {
		finish(true, false)
	}
	if _, ok := reserveLogin(ip); ok {
		t.Fatal("failed guesses must exhaust the limit")
	}
	clearLoginAttempts(ip)

	loginRateLimiter.mu.Lock()
	for i := 0; i < maxLoginSources; i++ {
		loginRateLimiter.attempts[fmt.Sprintf("expired-test-%d", i)] = &loginBucket{failures: []time.Time{time.Now().Add(-2 * loginWindow)}}
	}
	loginRateLimiter.mu.Unlock()
	finish, ok := reserveLogin("new-source-test")
	if !ok {
		t.Fatal("expired sources blocked a new source")
	}
	finish(false, false)
	loginRateLimiter.mu.Lock()
	n := len(loginRateLimiter.attempts)
	loginRateLimiter.mu.Unlock()
	if n >= maxLoginSources {
		t.Fatalf("expired sources not swept: %d", n)
	}
}
