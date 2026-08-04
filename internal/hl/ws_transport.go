package hl

// Websocket transport for signed exchange actions.
//
// The SIGNING PATH DOES NOT CHANGE. msgpack hashing, EIP-712, the nonce and the
// envelope are all identical; only the pipe differs. Hyperliquid accepts the
// same signed body over the socket:
//
//	{"method":"post","id":<n>,"request":{"type":"action","payload":{ ...body... }}}
//
// WHY: REST order entry shares a per-IP weight budget with market data, so
// placements compete with the book feed that decides where to place them. One
// 429 during a fast move turned into 3,180 retry attempts over four hours in a
// downstream bot, each retry spending the budget whose exhaustion caused the
// rejection.
//
// THE RULE THAT MATTERS MORE THAN THE SPEED
//
// A reply that never arrives is NOT a failed request. The exchange does not
// dedup cloids, so resending a write whose outcome is unknown is how you fill
// twice — and the moment that is most likely (a slow or dropped socket) is
// exactly the moment the market is moving.
//
// So `Sent` is set by the same conservative rule the HTTP transport uses, and
// it is the ONLY thing a caller may act on:
//
//	Sent=false  the frame provably never left this host — dial failed, or the
//	            write errored before any byte was flushed. Safe to retry, and
//	            the only case where falling back to HTTP is legitimate.
//	Sent=true   everything else, including a write that succeeded and a reply
//	            that timed out. Outcome UNKNOWN. Resolve it by READING (order
//	            status by cloid), never by resending.
//
// There is deliberately no automatic HTTP fallback in here. The ticket that
// asked for this proposed "timeout falls back to HTTP"; that is a double-fill
// generator and it is not implemented. Fallback lives one layer up and is gated
// on Sent=false only.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// wsPostTimeout bounds how long we wait for a reply before giving up on it. On
// expiry the request is Sent=true/unknown — never retried here.
const wsPostTimeout = 10 * time.Second

// wsTransport is a request/response socket, deliberately NOT the subscription
// stream in internal/core. Different shape (correlated replies, no channels),
// different lifetime, and internal/hl cannot import internal/core anyway — core
// imports hl, so reusing that plumbing is not available at this layer.
type wsTransport struct {
	url  string
	http *httpTransport // used ONLY for non-/exchange paths and pre-send fallback

	mu      sync.Mutex
	conn    *websocket.Conn
	pending map[int64]chan wsReply
	nextID  atomic.Int64
	closed  bool
}

type wsReply struct {
	data []byte
	err  error
}

func newWSTransport(baseURL string, ht *httpTransport) *wsTransport {
	return &wsTransport{url: wsURLFor(baseURL), http: ht, pending: map[int64]chan wsReply{}}
}

// wsURLFor derives the socket endpoint from the REST base, matching the venue's
// convention (https://api… -> wss://api…/ws).
func wsURLFor(base string) string {
	if base == "" {
		base = MainnetAPIURL
	}
	u, err := url.Parse(base)
	if err != nil {
		return strings.TrimSuffix(base, "/") + "/ws"
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/ws"
	return u.String()
}

func (t *wsTransport) base() string { return t.http.base() }

// post satisfies the same contract as httpTransport.post.
//
// Only /exchange goes over the socket. Info reads stay on HTTP: they are
// idempotent, they are not what the weight budget is being protected from, and
// keeping them off this path means a socket problem can never make a read lie.
func (t *wsTransport) post(ctx context.Context, path string, payload any) ([]byte, error) {
	if path != "/exchange" {
		return t.http.post(ctx, path, payload)
	}

	conn, err := t.ensureConn(ctx)
	if err != nil {
		// A dial that never connected is provably pre-send.
		return nil, &TransportError{Sent: false, Err: fmt.Errorf("websocket dial: %w", err)}
	}

	id := t.nextID.Add(1)
	frame, err := json.Marshal(map[string]any{
		"method":  "post",
		"id":      id,
		"request": map[string]any{"type": "action", "payload": payload},
	})
	if err != nil {
		return nil, &TransportError{Sent: false, Err: fmt.Errorf("failed to marshal payload: %w", err)}
	}

	ch := make(chan wsReply, 1)
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, &TransportError{Sent: false, Err: errors.New("websocket transport is closed")}
	}
	t.pending[id] = ch
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.pending, id)
		t.mu.Unlock()
	}()

	// The write is the point of no return. gorilla serialises writes internally
	// per connection only if callers do; this lock is what makes that true.
	t.mu.Lock()
	werr := conn.WriteMessage(websocket.TextMessage, frame)
	t.mu.Unlock()
	if werr != nil {
		// A failed write MAY still have put bytes on the wire — gorilla does not
		// tell us how far it got. Conservative by design: unknown, not retryable.
		t.dropConn(conn)
		return nil, &TransportError{Sent: true, Err: fmt.Errorf("websocket write: %w", werr)}
	}

	select {
	case r := <-ch:
		if r.err != nil {
			// The socket died after our frame went out. The action may well have
			// executed; the caller must resolve by reading, not resending.
			return nil, &TransportError{Sent: true, Err: r.err}
		}
		return r.data, nil
	case <-ctx.Done():
		return nil, &TransportError{Sent: true, Err: ctx.Err()}
	case <-time.After(wsPostTimeout):
		return nil, &TransportError{Sent: true,
			Err: fmt.Errorf("no websocket reply within %s: the action's outcome is UNKNOWN", wsPostTimeout)}
	}
}

// ensureConn returns a live connection, dialling once and reusing it. Callers
// racing on the first dial all wait on the same attempt.
func (t *wsTransport) ensureConn(ctx context.Context) (*websocket.Conn, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, errors.New("transport closed")
	}
	if t.conn != nil {
		c := t.conn
		t.mu.Unlock()
		return c, nil
	}
	t.mu.Unlock()

	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := d.DialContext(ctx, t.url, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(16 << 20)

	t.mu.Lock()
	if t.conn != nil { // someone else won the race; keep theirs
		other := t.conn
		t.mu.Unlock()
		conn.Close()
		return other, nil
	}
	t.conn = conn
	t.mu.Unlock()

	go t.readLoop(conn)
	return conn, nil
}

// readLoop fans replies out by id. Replies are NOT guaranteed to arrive in
// request order, which is the whole reason the correlator exists — reading the
// next message and assuming it answers the last request would mis-attribute an
// order result to the wrong order.
func (t *wsTransport) readLoop(conn *websocket.Conn) {
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.failAllPending(conn, fmt.Errorf("websocket closed: %w", err))
			return
		}
		var env struct {
			Channel string `json:"channel"`
			Data    struct {
				ID       int64           `json:"id"`
				Response json.RawMessage `json:"response"`
			} `json:"data"`
		}
		if err := json.Unmarshal(msg, &env); err != nil {
			continue // an unparseable frame is not an answer to anything
		}
		if env.Channel != "post" || env.Data.ID == 0 {
			continue // subscription traffic, pongs, errors without a correlation id
		}
		// The venue wraps the action result one level deeper.
		var inner struct {
			Payload json.RawMessage `json:"payload"`
		}
		data := env.Data.Response
		if err := json.Unmarshal(env.Data.Response, &inner); err == nil && len(inner.Payload) > 0 {
			data = inner.Payload
		}

		t.mu.Lock()
		ch := t.pending[env.Data.ID]
		t.mu.Unlock()
		if ch != nil {
			select {
			case ch <- wsReply{data: data}:
			default:
			}
		}
	}
}

// failAllPending wakes every in-flight caller with Sent=true semantics — their
// frames were written, so their outcomes are unknown.
func (t *wsTransport) failAllPending(conn *websocket.Conn, err error) {
	t.mu.Lock()
	if t.conn == conn {
		t.conn = nil
	}
	waiting := make([]chan wsReply, 0, len(t.pending))
	for _, ch := range t.pending {
		waiting = append(waiting, ch)
	}
	t.mu.Unlock()
	for _, ch := range waiting {
		select {
		case ch <- wsReply{err: err}:
		default:
		}
	}
	conn.Close()
}

func (t *wsTransport) dropConn(conn *websocket.Conn) {
	t.mu.Lock()
	if t.conn == conn {
		t.conn = nil
	}
	t.mu.Unlock()
	conn.Close()
}

// Close releases the socket. Pending callers are woken as unknown-outcome.
func (t *wsTransport) Close() {
	t.mu.Lock()
	t.closed = true
	conn := t.conn
	t.conn = nil
	t.mu.Unlock()
	if conn != nil {
		t.failAllPending(conn, errors.New("transport closed"))
	}
}
