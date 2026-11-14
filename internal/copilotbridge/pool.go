package copilotbridge

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// A worker is exclusively leased under the existing admission slots. Its
// process lifetime is independent of requests; cancellation destroys the lease.
type channelWorker struct {
	cmd            *exec.Cmd
	conn           *rpcConn
	stdin          io.WriteCloser
	stdout         io.ReadCloser
	stderr         lockedDetail
	done           chan struct{}
	once           sync.Once
	born           time.Time
	turns          int
	binary         os.FileInfo
	idleTimer      *time.Timer
	idleGeneration uint64
}

type lockedDetail struct {
	mu   sync.Mutex
	text strings.Builder
}

func (b *lockedDetail) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if left := 8192 - b.text.Len(); left > 0 {
		if len(p) > left {
			p = p[:left]
		}
		_, _ = b.text.Write(p)
	}
	return n, nil
}
func (b *lockedDetail) Reset() { b.mu.Lock(); defer b.mu.Unlock(); b.text.Reset() }

func (b *lockedDetail) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.text.String() }
func (w *channelWorker) stop() {
	w.once.Do(func() { _ = w.cmd.Process.Kill(); _ = w.stdin.Close(); _ = w.stdout.Close(); <-w.done })
}
func (r *Runner) accepting(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.poolMu.Lock()
	defer r.poolMu.Unlock()
	if r.closed {
		return errors.New("copilot-bridge: runner is closed")
	}
	return nil
}
func (r *Runner) poolLimits() (time.Duration, time.Duration, int) {
	idle, age, turns := r.PoolIdleTimeout, r.PoolMaxAge, r.PoolMaxTurns
	if idle <= 0 {
		idle = time.Minute
	}
	if age <= 0 {
		age = 15 * time.Minute
	}
	if turns <= 0 {
		turns = 100
	}
	return idle, age, turns
}
func (r *Runner) freshWorker(w *channelWorker) bool {
	_, age, turns := r.poolLimits()
	if time.Since(w.born) >= age || w.turns >= turns {
		return false
	}
	st, err := os.Stat(r.Bin)
	if err != nil || !os.SameFile(st, w.binary) || st.Size() != w.binary.Size() || !st.ModTime().Equal(w.binary.ModTime()) {
		return false
	}
	select {
	case <-w.done:
		return false
	default:
		return true
	}
}
func (r *Runner) takeWorker() (*channelWorker, error) { return r.takeWorkerFor(nil) }
func (w *channelWorker) stopIdle() {
	if w.conn != nil && w.conn.completed != nil {
		w.conn.deleteSession(w.conn.session)
	}
	w.stop()
}

// takeWorkerFor hands out a warm or new worker. Stale idle workers are
// detached under poolMu but reaped after it is released: their session.delete
// can block for deleteTimeout and must not stall every other pool operation.
func (r *Runner) takeWorkerFor(turn *ChannelTurn) (*channelWorker, error) {
	w, stale, err := r.takeWorkerLocked(turn)
	for _, s := range stale {
		go s.stopIdle()
	}
	return w, err
}

func (r *Runner) takeWorkerLocked(turn *ChannelTurn) (_ *channelWorker, stale []*channelWorker, _ error) {
	r.poolMu.Lock()
	defer r.poolMu.Unlock()
	if r.closed {
		return nil, nil, errors.New("copilot-bridge: runner is closed")
	}
	if turn != nil && r.SessionCacheEnabled() {
		for i, w := range r.idleWorkers {
			if _, ok := w.conn.completed.continuation(*turn); ok {
				last := len(r.idleWorkers) - 1
				r.idleWorkers[i], r.idleWorkers[last] = r.idleWorkers[last], w
				break
			}
		}
	}
	for len(r.idleWorkers) > 0 {
		n := len(r.idleWorkers) - 1
		w := r.idleWorkers[n]
		r.idleWorkers = r.idleWorkers[:n]
		w.idleGeneration++
		if w.idleTimer != nil {
			w.idleTimer.Stop()
		}
		if r.freshWorker(w) {
			w.stderr.Reset()
			return w, stale, nil
		}
		delete(r.workers, w)
		stale = append(stale, w)
	}
	st, err := os.Stat(r.Bin)
	if err != nil {
		return nil, stale, toolsRefused("the CLI could not start", err)
	}
	w := &channelWorker{born: time.Now(), binary: st, done: make(chan struct{})}
	// No request context: an explicit lease cancellation or Close owns termination.
	w.cmd = exec.Command(r.Bin, "--headless", "--stdio", "--no-auto-update", "--log-level", "error", "--disable-builtin-mcps")
	w.cmd.Dir = os.TempDir()
	w.cmd.Env = childEnv(os.Environ(), r.Home)
	w.cmd.WaitDelay = 5 * time.Second
	w.cmd.Stderr = &w.stderr
	w.stdin, err = w.cmd.StdinPipe()
	if err != nil {
		return nil, stale, err
	}
	w.stdout, err = w.cmd.StdoutPipe()
	if err != nil {
		w.stdin.Close()
		return nil, stale, err
	}
	began := time.Now()
	if err = w.cmd.Start(); err != nil {
		w.stdin.Close()
		w.stdout.Close()
		return nil, stale, toolsRefused("the CLI could not start", err)
	}
	r.observeStage("spawn", time.Since(began))
	go func() { _ = w.cmd.Wait(); close(w.done) }()
	w.conn = &rpcConn{w: w.stdin, r: bufio.NewReader(w.stdout), closeRead: func() { _ = w.stdout.Close() }, observe: r.observeStage, strict: true}
	if r.workers == nil {
		r.workers = make(map[*channelWorker]bool)
	}
	r.workers[w] = true
	return w, stale, nil
}
func (r *Runner) retireWorker(w *channelWorker) {
	r.poolMu.Lock()
	delete(r.workers, w)
	for i, idle := range r.idleWorkers {
		if idle == w {
			r.idleWorkers = append(r.idleWorkers[:i], r.idleWorkers[i+1:]...)
			break
		}
	}
	r.poolMu.Unlock()
	w.stop()
}
func (r *Runner) withPooledChannel(ctx context.Context, f func(*rpcConn) error) error {
	return r.withPooledTurn(ctx, nil, f)
}
func (r *Runner) withPooledTurn(ctx context.Context, turn *ChannelTurn, f func(*rpcConn) error) error {
	w, err := r.takeWorkerFor(turn)
	if err != nil {
		return err
	}
	cancelled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { w.stop(); close(cancelled) })
	err = f(w.conn)
	// Join a running callback before the worker can be leased again.
	if !stop() {
		<-cancelled
	}
	if ctx.Err() != nil {
		r.retireWorker(w)
		return ctx.Err()
	}
	if err != nil || !w.conn.reusable {
		r.retireWorker(w)
		var typed *Failure
		if errors.Is(err, errChannelClosed) && !errors.As(err, &typed) {
			return ClassifyExit(errors.New("the channel closed mid-turn"), w.stderr.String())
		}
		return err
	}
	r.poolMu.Lock()
	w.turns++
	if r.closed || !r.freshWorker(w) {
		delete(r.workers, w)
		r.poolMu.Unlock()
		w.stopIdle()
		return nil
	}
	r.idleWorkers = append(r.idleWorkers, w)
	idle, _, _ := r.poolLimits()
	w.idleGeneration++
	generation := w.idleGeneration
	w.idleTimer = time.AfterFunc(idle, func() {
		r.poolMu.Lock()
		// An old timer may race with acquisition; retire only if still idle.
		found := false
		for i, v := range r.idleWorkers {
			if v == w && w.idleGeneration == generation {
				r.idleWorkers = append(r.idleWorkers[:i], r.idleWorkers[i+1:]...)
				delete(r.workers, w)
				found = true
				break
			}
		}
		r.poolMu.Unlock()
		if found {
			w.stopIdle()
		}
	})
	r.poolMu.Unlock()
	return nil
}

// Close rejects new work, terminates active leases and reaps all warm children.
// The owner first drains HTTP requests within its shutdown grace period.
func (r *Runner) Close() {
	r.poolMu.Lock()
	r.closed = true
	idle := make(map[*channelWorker]bool)
	for _, w := range r.idleWorkers {
		idle[w] = true
	}
	workers := make([]*channelWorker, 0, len(r.workers))
	for w := range r.workers {
		workers = append(workers, w)
		if w.idleTimer != nil {
			w.idleTimer.Stop()
		}
	}
	r.workers = nil
	r.idleWorkers = nil
	r.poolMu.Unlock()
	for _, w := range workers {
		if idle[w] {
			w.stopIdle()
		} else {
			w.stop()
		}
	}
}
