package chromium

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

// CDPConn is a minimal Chrome DevTools Protocol client over a single websocket connection.
// It multiplexes request/response via a monotonic id, running one reader goroutine that dispatches
// responses to per-call channels. Events (frames without an id) are dropped.
//
// One CDPConn can serve both browser-level calls and per-target calls: pass an empty sessionID for
// browser-level methods, or the sessionID returned by Target.attachToTarget for target-scoped methods.
// Use flatten mode (flatten=true in attachToTarget) so responses ride the same websocket.
type CDPConn struct {
	ws     *websocket.Conn
	nextID atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan cdpResponse
	closed  bool
	closeCh chan struct{}
	readErr error
}

type cdpResponse struct {
	result json.RawMessage
	err    *CDPError
}

// CDPError is the error payload from a failed CDP call.
type CDPError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *CDPError) Error() string {
	return fmt.Sprintf("CDP error %d: %s", e.Code, e.Message)
}

// rawFrame matches any inbound CDP frame: either a response (has id) or an event (has method).
type rawFrame struct {
	ID     *int64          `json:"id,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *CDPError       `json:"error,omitempty"`
}

// DialCDP opens a websocket to the given CDP endpoint and starts the reader goroutine.
func DialCDP(ctx context.Context, url string) (*CDPConn, error) {
	ws, _, err := websocket.DefaultDialer.DialContext(ctx, url, http.Header{})
	if err != nil {
		return nil, fmt.Errorf("dialing CDP websocket: %w", err)
	}

	c := &CDPConn{
		ws:      ws,
		pending: map[int64]chan cdpResponse{},
		closeCh: make(chan struct{}),
	}

	go c.readLoop()

	return c, nil
}

// Close shuts down the reader and closes the websocket.
// Pending calls will unblock with an error.
func (c *CDPConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	close(c.closeCh)
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()

	err := c.ws.Close()

	for _, ch := range pending {
		close(ch)
	}

	return err
}

// Call sends a CDP method call and waits for the matching response.
// sessionID may be empty for browser-level methods, or the sessionId from Target.attachToTarget.
// The result is json-decoded into out (may be nil to discard).
func (c *CDPConn) Call(ctx context.Context, sessionID, method string, params, out any) error {
	id := c.nextID.Add(1)

	msg := map[string]any{
		"id":     id,
		"method": method,
	}
	if params != nil {
		msg["params"] = params
	}
	if sessionID != "" {
		msg["sessionId"] = sessionID
	}

	ch := make(chan cdpResponse, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("CDP connection closed")
	}
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.ws.WriteJSON(msg); err != nil {
		return fmt.Errorf("writing %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closeCh:
		if c.readErr != nil {
			return fmt.Errorf("CDP connection closed: %w", c.readErr)
		}
		return errors.New("CDP connection closed")
	case resp, ok := <-ch:
		if !ok {
			return errors.New("CDP connection closed while waiting for response")
		}
		if resp.err != nil {
			return resp.err
		}
		if out != nil && len(resp.result) > 0 {
			if err := json.Unmarshal(resp.result, out); err != nil {
				return fmt.Errorf("decoding %s response: %w", method, err)
			}
		}
		return nil
	}
}

func (c *CDPConn) readLoop() {
	for {
		_, raw, err := c.ws.ReadMessage()
		if err != nil {
			c.mu.Lock()
			c.readErr = err
			if !c.closed {
				c.closed = true
				close(c.closeCh)
				for _, ch := range c.pending {
					close(ch)
				}
				c.pending = nil
			}
			c.mu.Unlock()
			return
		}

		var frame rawFrame
		if err := json.Unmarshal(raw, &frame); err != nil {
			continue
		}
		if frame.ID == nil {
			// Event frame — we don't subscribe to events yet, drop it.
			continue
		}

		c.mu.Lock()
		ch, ok := c.pending[*frame.ID]
		c.mu.Unlock()
		if !ok {
			continue
		}

		select {
		case ch <- cdpResponse{result: frame.Result, err: frame.Error}:
		default:
			// Buffered chan, this shouldn't happen unless a caller sent twice on same id.
		}
	}
}
