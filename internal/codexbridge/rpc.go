package codexbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// The app-server speaks JSON-RPC 2.0 over JSONL on stdio (measured on 0.163.0:
// `codex app-server --stdio`). Three message shapes arrive on stdout: responses
// to our requests (id + result|error), notifications (method, no id) and
// server→client requests (method + id) that must be answered — approvals and
// dynamic tool calls.

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return "app-server: " + e.Message + " (" + strconv.Itoa(e.Code) + ")"
}

type rpcMsg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

// RequestHandler answers one server→client request. A nil error result is sent
// as the JSON-RPC result. Deferred (errDeferred) means the handler kept id and
// will answer later with reply — a client tool call waits for the client's
// next HTTP request.
type RequestHandler func(id json.RawMessage, method string, params json.RawMessage) (any, *rpcError)

// errDeferred marks a request the handler will answer later.
var errDeferred = &rpcError{Code: 0, Message: "deferred"}

type rpcConn struct {
	w io.Writer

	mu      sync.Mutex // guards w, next, pending
	next    int64
	pending map[int64]chan rpcMsg
	closed  error

	onNotify  func(method string, params json.RawMessage)
	onRequest RequestHandler
}

func newConn(w io.Writer, onNotify func(string, json.RawMessage), onRequest RequestHandler) *rpcConn {
	return &rpcConn{w: w, pending: map[int64]chan rpcMsg{}, onNotify: onNotify, onRequest: onRequest}
}

func (c *rpcConn) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed != nil {
		return c.closed
	}
	_, err = c.w.Write(append(b, '\n'))
	return err
}

// call sends one request and waits for its response.
func (c *rpcConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ch := make(chan rpcMsg, 1)
	c.mu.Lock()
	if c.closed != nil {
		err := c.closed
		c.mu.Unlock()
		return nil, err
	}
	c.next++
	id := c.next
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.drop(id)
		return nil, err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	case <-ctx.Done():
		c.drop(id)
		return nil, ctx.Err()
	}
}

func (c *rpcConn) drop(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *rpcConn) notify(method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	return c.write(msg)
}

// readLoop dispatches stdout until it ends; pending calls then fail with err.
func (c *rpcConn) readLoop(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var m rpcMsg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue // a non-JSON line (stray log) is not protocol
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			c.answer(m)
		case m.Method != "":
			if c.onNotify != nil {
				c.onNotify(m.Method, m.Params)
			}
		case len(m.ID) > 0:
			id, err := strconv.ParseInt(string(m.ID), 10, 64)
			if err != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	c.fail(fmt.Errorf("app-server output ended: %w", err))
	return err
}

func (c *rpcConn) answer(m rpcMsg) {
	var (
		result any
		rerr   = &rpcError{Code: -32601, Message: "codex-bridge does not serve " + m.Method}
	)
	if c.onRequest != nil {
		result, rerr = c.onRequest(m.ID, m.Method, m.Params)
	}
	if rerr == errDeferred {
		return
	}
	c.reply(m.ID, result, rerr)
}

// reply answers a server→client request by id.
func (c *rpcConn) reply(id json.RawMessage, result any, rerr *rpcError) {
	reply := map[string]any{"jsonrpc": "2.0", "id": id}
	if rerr != nil {
		reply["error"] = rerr
	} else {
		reply["result"] = result
	}
	_ = c.write(reply)
}

func (c *rpcConn) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed == nil {
		c.closed = err
	}
	for id, ch := range c.pending {
		ch <- rpcMsg{Error: &rpcError{Code: -32000, Message: err.Error()}}
		delete(c.pending, id)
	}
}
