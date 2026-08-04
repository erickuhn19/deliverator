package hl

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wsServer is a fake venue socket. behave decides what it does with each frame.
func wsServer(t *testing.T, behave func(conn *websocket.Conn, id int64, raw []byte)) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var f struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(msg, &f)
			behave(conn, f.ID, msg)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestWS(t *testing.T, srv *httptest.Server) *wsTransport {
	t.Helper()
	ht := newTransport(srv.URL)
	w := newWSTransport(srv.URL, ht)
	t.Cleanup(w.Close)
	return w
}

func reply(conn *websocket.Conn, id int64, payload string) {
	_ = conn.WriteJSON(map[string]any{
		"channel": "post",
		"data":    map[string]any{"id": id, "response": map[string]any{"payload": json.RawMessage(payload)}},
	})
}

func TestWSPostRoundTrips(t *testing.T) {
	srv := wsServer(t, func(c *websocket.Conn, id int64, _ []byte) {
		reply(c, id, `{"status":"ok"}`)
	})
	w := newTestWS(t, srv)
	got, err := w.post(context.Background(), "/exchange", map[string]any{"action": "x"})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if !strings.Contains(string(got), `"status":"ok"`) {
		t.Errorf("payload not unwrapped, got %s", got)
	}
}

// THE reason the correlator exists. Replies are not ordered, so answering the
// next-message-off-the-wire would attribute one order's result to another.
func TestRepliesAreMatchedByIDNotArrivalOrder(t *testing.T) {
	var mu sync.Mutex
	var held []int64
	srv := wsServer(t, func(c *websocket.Conn, id int64, _ []byte) {
		mu.Lock()
		held = append(held, id)
		n := len(held)
		ids := append([]int64(nil), held...)
		mu.Unlock()
		if n < 2 {
			return // hold the first request's answer back
		}
		// Answer in REVERSE order.
		for i := len(ids) - 1; i >= 0; i-- {
			reply(c, ids[i], `{"for":`+jsonInt(ids[i])+`}`)
		}
	})
	w := newTestWS(t, srv)

	type res struct {
		body string
		err  error
	}
	out := make(chan res, 2)
	for i := 0; i < 2; i++ {
		go func() {
			b, err := w.post(context.Background(), "/exchange", map[string]any{"n": i})
			out <- res{string(b), err}
		}()
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case r := <-out:
			if r.err != nil {
				t.Fatalf("post %d: %v", i, r.err)
			}
			if seen[r.body] {
				t.Fatalf("two callers got the same reply %q — the correlator is not correlating", r.body)
			}
			seen[r.body] = true
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for correlated replies")
		}
	}
}

func jsonInt(i int64) string { b, _ := json.Marshal(i); return string(b) }

// THE double-fill rule. A reply that never comes is an UNKNOWN outcome, not a
// failure — the frame went out, the exchange does not dedup cloids, and a
// caller that retries on this fills twice.
func TestATimedOutReplyIsSentTrueSoItIsNeverRetried(t *testing.T) {
	srv := wsServer(t, func(*websocket.Conn, int64, []byte) { /* never answers */ })
	w := newTestWS(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := w.post(ctx, "/exchange", map[string]any{"action": "x"})
	if err == nil {
		t.Fatal("a silent venue must produce an error")
	}
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("want *TransportError, got %T", err)
	}
	if !te.Sent {
		t.Fatal("a written frame with no reply MUST be Sent=true — Sent=false invites a resubmit that double-fills")
	}
}

// A socket that dies after our frame went out is the same shape: unknown, not
// safe to retry.
func TestAConnectionDroppedAfterTheWriteIsUnknownNotFailed(t *testing.T) {
	srv := wsServer(t, func(c *websocket.Conn, _ int64, _ []byte) { c.Close() })
	w := newTestWS(t, srv)
	_, err := w.post(context.Background(), "/exchange", map[string]any{"action": "x"})
	if err == nil {
		t.Fatal("a dropped socket must produce an error")
	}
	var te *TransportError
	if !errors.As(err, &te) || !te.Sent {
		t.Fatalf("a drop AFTER the write must be Sent=true, got %+v", te)
	}
}

// The one case that IS provably pre-send, and therefore the only one where
// falling back to HTTP would be legitimate.
func TestADialFailureIsProvablyPreSend(t *testing.T) {
	ht := newTransport("http://127.0.0.1:1")
	w := newWSTransport("http://127.0.0.1:1", ht)
	defer w.Close()
	_, err := w.post(context.Background(), "/exchange", map[string]any{"action": "x"})
	if err == nil {
		t.Fatal("dialling a dead port must fail")
	}
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("want *TransportError, got %T", err)
	}
	if te.Sent {
		t.Error("a failed dial never put bytes on the wire and must be Sent=false")
	}
}

// Reads stay on HTTP: they are idempotent, they are not what the weight budget
// needs protecting from, and a socket fault must never make a read lie.
func TestOnlyExchangeGoesOverTheSocket(t *testing.T) {
	var wsCalls int
	srv := wsServer(t, func(c *websocket.Conn, id int64, _ []byte) {
		wsCalls++
		reply(c, id, `{}`)
	})
	httpHit := make(chan string, 1)
	rest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case httpHit <- r.URL.Path:
		default:
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer rest.Close()

	w := newWSTransport(srv.URL, newTransport(rest.URL))
	defer w.Close()
	if _, err := w.post(context.Background(), "/info", map[string]any{"type": "meta"}); err != nil {
		t.Fatalf("info over http: %v", err)
	}
	select {
	case p := <-httpHit:
		if p != "/info" {
			t.Errorf("unexpected http path %q", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("/info did not go over HTTP")
	}
	if wsCalls != 0 {
		t.Errorf("a read went over the socket (%d calls)", wsCalls)
	}
}

func TestWSURLDerivation(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.hyperliquid.xyz":  "wss://api.hyperliquid.xyz/ws",
		"https://api.hyperliquid.xyz/": "wss://api.hyperliquid.xyz/ws",
		"http://127.0.0.1:8080":        "ws://127.0.0.1:8080/ws",
	} {
		if got := wsURLFor(in); got != want {
			t.Errorf("wsURLFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// A frame with no correlation id (subscription traffic, pongs) must not be
// handed to a waiting caller as if it were their answer.
func TestUncorrelatedFramesAreIgnored(t *testing.T) {
	srv := wsServer(t, func(c *websocket.Conn, id int64, _ []byte) {
		_ = c.WriteJSON(map[string]any{"channel": "subscriptionResponse", "data": map[string]any{}})
		_ = c.WriteJSON(map[string]any{"channel": "pong"})
		reply(c, id, `{"status":"ok"}`)
	})
	w := newTestWS(t, srv)
	got, err := w.post(context.Background(), "/exchange", map[string]any{"action": "x"})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if !strings.Contains(string(got), "ok") {
		t.Errorf("noise frames leaked into the reply: %s", got)
	}
}

// The transport is opt-in. A default Exchange must still be on HTTP.
func TestHTTPIsTheDefaultTransport(t *testing.T) {
	ex := &Exchange{}
	ExchangeOptWebsocket(false)(ex)
	if ex.useWS {
		t.Error("websocket must be off unless explicitly enabled")
	}
	ex2 := &Exchange{}
	if ex2.useWS {
		t.Error("the zero value must be HTTP")
	}
}
