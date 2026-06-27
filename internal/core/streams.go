package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// We own the Hyperliquid WebSocket protocol directly (deliverator has no
// third-party client dependency). The wire protocol is trivial: we forward
// every data frame as NDJSON, with full reconnect/resubscribe control. Talking
// the protocol ourselves also avoids the typed-subscription dispatch bug some
// clients have for orderUpdates/notification/openOrders/webData2, where a
// message's bare channel key ("orderUpdates") never matches a user-namespaced
// subscriber id ("orderUpdates:<user>").

// Stream channel/type constants (Hyperliquid wire `subscription.type`).
const (
	ChanL2Book         = "l2Book"
	ChanBbo            = "bbo"
	ChanTrades         = "trades"
	ChanCandle         = "candle"
	ChanAllMids        = "allMids"
	ChanUserFills      = "userFills"
	ChanOrderUpdates   = "orderUpdates"
	ChanNotification   = "notification"
	ChanActiveAssetCtx = "activeAssetCtx"
	// Per-user aggregate snapshot (positions + open orders + margin + more).
	ChanWebData2 = "webData2"
	// Per-user-per-coin: leverage, margin, and available-to-trade (needs coin+user).
	ChanActiveAssetData = "activeAssetData"
	// TWAP slice executions — live progress of a running TWAP.
	ChanUserTwapSliceFills = "userTwapSliceFills"
)

// StreamSub is one subscription request.
type StreamSub struct {
	Type     string
	Coin     string
	Interval string
	User     string
	NSigFigs int
}

func (s StreamSub) payload() map[string]any {
	m := map[string]any{"type": s.Type}
	if s.Coin != "" {
		m["coin"] = s.Coin
	}
	if s.Interval != "" {
		m["interval"] = s.Interval
	}
	if s.User != "" {
		m["user"] = s.User
	}
	if s.NSigFigs > 0 {
		m["nSigFigs"] = s.NSigFigs
	}
	return m
}

// StreamEvent is one decoded frame forwarded to the caller. Channel "reconnect"
// is a control event the streamer injects when the socket drops.
type StreamEvent struct {
	Channel string
	Data    json.RawMessage
}

// wsURL derives the WebSocket endpoint from the configured network (or override).
func (c *Client) wsURL() string {
	if c.cfg.Endpoints.WSURL != "" {
		return c.cfg.Endpoints.WSURL
	}
	u := strings.Replace(c.signURL, "https://", "wss://", 1)
	u = strings.Replace(u, "http://", "ws://", 1)
	return strings.TrimRight(u, "/") + "/ws"
}

// Stream connects, subscribes, and calls onEvent for every data frame until ctx
// is cancelled. It reconnects with capped exponential backoff and resubscribes.
//
// onEvent is invoked serially from a single goroutine — data frames from the read
// loop, plus the reconnect marker fired between reconnects — never concurrently.
// Callers rely on this invariant: e.g. Watch's cooldownGate is intentionally
// lock-free. Do not move onEvent dispatch onto multiple goroutines without
// revisiting those callers.
func (c *Client) Stream(ctx context.Context, subs []StreamSub, onEvent func(StreamEvent)) error {
	url := c.wsURL()
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		err := c.streamOnce(ctx, url, subs, onEvent)
		if ctx.Err() != nil {
			return nil // clean shutdown (signal / cancel)
		}
		// A drop means a gap may have occurred — the socket has no replay, so any
		// user stream (fills/orders) must be reconciled against a fresh snapshot.
		// resync flags that; the consumer dedups by the per-event keys (tid for
		// fills, oid+status for order updates). See TOOLS.md "Streams".
		onEvent(StreamEvent{
			Channel: "reconnect",
			Data: json.RawMessage(fmt.Sprintf(
				`{"reason":%q,"backoff_ms":%d,"resync":true,"hint":"gap possible — re-snapshot (portfolio/orders/fills --since) and dedup by tid (fills) / oid+status (orders)"}`,
				errString(err), backoff.Milliseconds(),
			)),
		})
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

const wsReadTimeout = 90 * time.Second

// maxWSMessageBytes bounds a single WebSocket frame (audit #91 / S8). The feed
// never sends frames this large; an oversized one is treated as a read error.
const maxWSMessageBytes = 16 << 20

func (c *Client) streamOnce(ctx context.Context, url string, subs []StreamSub, onEvent func(StreamEvent)) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Bound a single frame so a malicious/buggy feed can't OOM the long-running
	// watch loop; an oversized frame surfaces as a read error → reconnect.
	conn.SetReadLimit(maxWSMessageBytes)

	var wmu sync.Mutex
	writeJSON := func(v any) error {
		wmu.Lock()
		defer wmu.Unlock()
		return conn.WriteJSON(v)
	}

	for _, s := range subs {
		if err := writeJSON(map[string]any{"method": "subscribe", "subscription": s.payload()}); err != nil {
			return err
		}
	}

	// Close the conn when ctx is cancelled to unblock ReadMessage.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	// Keepalive ping every 50s (server pong resets the read deadline).
	go func() {
		t := time.NewTicker(50 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if err := writeJSON(map[string]any{"method": "ping"}); err != nil {
					return
				}
			}
		}
	}()

	_ = conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(wsReadTimeout))

		var env struct {
			Channel string          `json:"channel"`
			Data    json.RawMessage `json:"data"`
		}
		if json.Unmarshal(msg, &env) != nil {
			continue
		}
		switch env.Channel {
		case "", "pong", "subscriptionResponse", "error":
			continue // control frames
		}
		onEvent(StreamEvent{Channel: env.Channel, Data: env.Data})
	}
}

func errString(err error) string {
	if err == nil {
		return "closed"
	}
	return err.Error()
}
