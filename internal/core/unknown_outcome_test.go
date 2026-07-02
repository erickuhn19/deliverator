package core

// Tests for the unknown-outcome / anti-double-fill cluster (review P0-B): a
// write-path transport failure AFTER the signed action may have reached the
// exchange must be exit 42 (outcome UNKNOWN, run §5.4), never exit 50
// ("exchange-rejected", which sanctions a blind resubmit → double-fill); a
// provably-never-sent failure is exit 40; a write-path 429 is exit 41. The
// (possibly auto-generated) cloid must be surfaced on failure — in error.cloids
// and the exit-42 hint — and persisted to the audit trail BEFORE the round trip
// (intent row), so a crash/SIGKILL mid-flight can't lose the idempotency key.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/erickuhn19/deliverator/internal/config"
	hl "github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/output"
	"github.com/erickuhn19/deliverator/internal/state"
)

// newClientAt builds a Client wired to an arbitrary URL — a raw handler the
// test owns (hijack/RST) or a dead address (connection refused) — mirroring
// newTestClient's wiring without its canned respFn server.
func newClientAt(t *testing.T, cfg *config.Config, opts Options, url string) (*Client, context.Context) {
	t.Helper()
	testHome(t)
	meta := testMeta()
	key, err := crypto.HexToECDSA(testAgentKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c := &Client{
		cfg:       cfg,
		opts:      opts,
		network:   "testnet",
		infoURL:   url,
		signURL:   url,
		httpc:     &http.Client{},
		meta:      meta,
		info:      hl.NewInfo(ctx, url, true, meta.Meta(), meta.SpotMeta(), nil),
		ex:        hl.NewExchange(ctx, key, url, meta.Meta(), "", testMaster, meta.SpotMeta(), nil),
		queryAddr: testMaster,
		nonce:     state.NewNonceLock(filepath.Join(config.Dir(), "nonce.lock")),
		audit:     state.NewAudit(filepath.Join(config.Dir(), "audit.jsonl"), true),
	}
	return c, ctx
}

// serveInfo answers the standard /info reads the write gauntlet touches.
func serveInfo(w http.ResponseWriter, typ string) {
	switch typ {
	case "allMids":
		_, _ = io.WriteString(w, `{"BTC":"64000","ETH":"3000"}`)
	case "clearinghouseState":
		_, _ = io.WriteString(w, emptyState)
	case "frontendOpenOrders":
		_, _ = io.WriteString(w, `[]`)
	case "spotClearinghouseState":
		_, _ = io.WriteString(w, `{"balances":[]}`)
	default:
		_, _ = io.WriteString(w, `{}`)
	}
}

// rstServer serves /info normally and kills every /exchange connection with a
// TCP RST after fully reading the signed request — the post-send transport
// failure (LB/NAT reset) where the order may well have landed. onExchange (if
// non-nil) sees each /exchange body before the reset.
func rstServer(t *testing.T, onExchange func(body map[string]any)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/info" {
			if onExchange != nil {
				onExchange(body)
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			if tc, ok := conn.(*net.TCPConn); ok {
				_ = tc.SetLinger(0) // RST, not FIN
			}
			_ = conn.Close()
			return
		}
		typ, _ := body["type"].(string)
		serveInfo(w, typ)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// wireCloids extracts every order cloid ("c") from a posted /exchange body.
func wireCloids(body map[string]any) []string {
	action, _ := body["action"].(map[string]any)
	orders, _ := action["orders"].([]any)
	var out []string
	for _, o := range orders {
		om, _ := o.(map[string]any)
		if c, _ := om["c"].(string); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// A connection reset AFTER the signed order request was fully read is outcome
// UNKNOWN: exit 42 (category timeout), with the generated cloid surfaced in
// error.cloids and a concrete §5.4 hint — never exit 50 "rejected" (finding 1).
func TestPlaceTransportResetIsOutcomeUnknown(t *testing.T) {
	var sent atomic.Value
	srv := rstServer(t, func(body map[string]any) {
		if cl := wireCloids(body); len(cl) == 1 {
			sent.Store(cl[0])
		}
	})
	c, ctx := newClientAt(t, config.Default(), Options{}, srv.URL)
	_, _, err := c.Place(ctx, limitBuy())
	assertErr(t, err, output.CatTimeout, output.ExitTimeout)

	cloid, _ := sent.Load().(string)
	if cloid == "" {
		t.Fatal("test harness never saw the signed order on the wire")
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("not an *output.Error: %v", err)
	}
	if len(oe.Cloids) != 1 || oe.Cloids[0] != cloid {
		t.Fatalf("error.cloids = %v, want the generated cloid %s", oe.Cloids, cloid)
	}
	if !strings.Contains(oe.Hint, cloid) {
		t.Fatalf("exit-42 hint must carry the concrete cloid, got %q", oe.Hint)
	}
}

// A truncated 200 body (connection died while the response streamed) is the
// same hazard: the exchange answered, so the action landed — exit 42, not 50.
func TestPlaceTruncated200IsOutcomeUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/info" {
			_, _ = io.WriteString(w, `{"status":"ok","respo`) // truncated envelope
			return
		}
		typ, _ := body["type"].(string)
		serveInfo(w, typ)
	}))
	t.Cleanup(srv.Close)
	c, ctx := newClientAt(t, config.Default(), Options{}, srv.URL)
	_, _, err := c.Place(ctx, limitBuy())
	assertErr(t, err, output.CatTimeout, output.ExitTimeout)
}

// A connection refused means the signed action provably never left this host:
// exit 40 (network, retryable), not 50 (finding 1, pre-send half).
func TestPlaceConnectionRefusedIsNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := srv.URL
	srv.Close() // nothing listens here anymore
	c, ctx := newClientAt(t, config.Default(), Options{}, deadURL)
	// A limit order needs no /info read before signing, so the dead endpoint is
	// first contacted by the signed write itself.
	_, _, err := c.Place(ctx, limitBuy())
	assertErr(t, err, output.CatNetwork, output.ExitNetwork)
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("not an *output.Error: %v", err)
	}
	if !oe.Retryable {
		t.Fatalf("a never-sent write must be retryable: %+v", oe)
	}
}

// A write-path HTTP 429 is exit 41 with retry_after_ms — mapExchangeErr was
// missing the APIError.Status check mapNetwork has (finding 2).
func TestPlaceRateLimited429(t *testing.T) {
	resp := func(path, typ string, _ map[string]any) (int, string) {
		if path == "/exchange" {
			return 429, `{"code":429,"msg":""}` // no "rate limit" words in the message
		}
		return 200, `{}`
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	_, _, err := c.Place(ctx, limitBuy())
	assertErr(t, err, output.CatRateLimit, output.ExitRateLimit)
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("not an *output.Error: %v", err)
	}
	if !oe.Retryable || oe.RetryAfterMs == nil || *oe.RetryAfterMs <= 0 {
		t.Fatalf("write 429 must set a positive retry_after_ms: %+v", oe)
	}
}

// A gateway/edge 5xx with a non-JSON body (Cloudflare/nginx HTML in front of
// the API) is emitted AFTER the signed order may have been forwarded upstream:
// outcome UNKNOWN — exit 42 with the cloid surfaced, never exit 50, which the
// contract documents as a definitive exchange rejection (fix-up round, P0-B).
func TestPlaceGateway502HTMLIsOutcomeUnknown(t *testing.T) {
	resp := func(path, _ string, _ map[string]any) (int, string) {
		if path == "/exchange" {
			return 502, `<html><body><h1>502 Bad Gateway</h1>cloudflare</body></html>`
		}
		return 200, `{}`
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	_, _, err := c.Place(ctx, limitBuy())
	assertErr(t, err, output.CatTimeout, output.ExitTimeout)
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("not an *output.Error: %v", err)
	}
	if len(oe.Cloids) != 1 || oe.Cloids[0] == "" {
		t.Fatalf("the generated cloid must be surfaced for the §5.4 check: %+v", oe)
	}
}

// A JSON-bodied 5xx APIError ("504 Gateway Time-out" — hyphenated, so no
// timeout substring rescues it) is the same post-send hazard: the LB timed out
// waiting on the upstream that may have accepted the order. Exit 42, not 50.
func TestPlaceGatewayJSON504IsOutcomeUnknown(t *testing.T) {
	resp := func(path, _ string, _ map[string]any) (int, string) {
		if path == "/exchange" {
			return 504, `{"code":504,"msg":"Gateway Time-out"}`
		}
		return 200, `{}`
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	_, _, err := c.Place(ctx, limitBuy())
	assertErr(t, err, output.CatTimeout, output.ExitTimeout)
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("not an *output.Error: %v", err)
	}
	if len(oe.Cloids) != 1 || oe.Cloids[0] == "" {
		t.Fatalf("the generated cloid must be surfaced for the §5.4 check: %+v", oe)
	}
}

// Reads must NOT regress: a post-send transport failure on a read is still a
// plain retryable network error (reads are side-effect-free — no §5.4 hazard).
func TestReadTransportResetStaysNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	c, ctx := newClientAt(t, config.Default(), Options{}, srv.URL)
	_, err := c.Mids(ctx)
	assertErr(t, err, output.CatNetwork, output.ExitNetwork)
}

// The generated cloid must be persisted BEFORE the exchange round trip (intent
// row), so a SIGKILL/crash mid-flight can never lose the only copy of the
// idempotency key (finding 4). The existing post-outcome row still follows.
func TestPlaceWritesIntentRowBeforeSend(t *testing.T) {
	var sent atomic.Value
	srv := rstServer(t, func(body map[string]any) {
		if cl := wireCloids(body); len(cl) == 1 {
			sent.Store(cl[0])
		}
	})
	c, ctx := newClientAt(t, config.Default(), Options{}, srv.URL)
	_, _, err := c.Place(ctx, limitBuy())
	if err == nil {
		t.Fatal("place against an RST server must fail")
	}
	cloid, _ := sent.Load().(string)
	rows := readAudit(t)
	var intent map[string]any
	for _, m := range rows {
		if m["action"] == "intent" {
			intent = m
		}
	}
	if intent == nil {
		t.Fatalf("no intent row in the trail: %+v", rows)
	}
	if intent["cmd"] != "order" || intent["coin"] != "BTC" || intent["cloid"] != cloid {
		t.Fatalf("intent row must carry cmd/coin/cloid, got %+v (wire cloid %s)", intent, cloid)
	}
	// The post-outcome "error" row is still appended after the failed round trip.
	last := rows[len(rows)-1]
	if last["action"] != "order" || last["status"] != "error" {
		t.Fatalf("post-outcome error row missing, trail ends with %+v", last)
	}
}

// A batch post-send failure must surface EVERY generated leg cloid in
// error.cloids and record them in a pre-send intent row — today both are
// discarded (findings 3+4, PlaceBatch error path).
func TestPlaceBatchFailureSurfacesLegCloids(t *testing.T) {
	var mu atomic.Value
	srv := rstServer(t, func(body map[string]any) {
		if cl := wireCloids(body); len(cl) > 0 {
			mu.Store(cl)
		}
	})
	c, ctx := newClientAt(t, config.Default(), Options{}, srv.URL)
	_, _, err := c.PlaceBatch(ctx, []OrderReq{
		{Coin: "BTC", Side: Buy, Size: "0.001", Limit: "60000"},
		{Coin: "ETH", Side: Buy, Size: "0.01", Limit: "2900"},
	})
	assertErr(t, err, output.CatTimeout, output.ExitTimeout)

	wire, _ := mu.Load().([]string)
	if len(wire) != 2 {
		t.Fatalf("harness saw %d wire cloids, want 2", len(wire))
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("not an *output.Error: %v", err)
	}
	if len(oe.Cloids) != 2 || oe.Cloids[0] != wire[0] || oe.Cloids[1] != wire[1] {
		t.Fatalf("error.cloids = %v, want all wire cloids %v", oe.Cloids, wire)
	}

	// The intent row carries the same cloids so reconcile can attribute the legs.
	var intent map[string]any
	for _, m := range readAudit(t) {
		if m["action"] == "intent" && m["cmd"] == "batch" {
			intent = m
		}
	}
	if intent == nil {
		t.Fatal("no batch intent row in the trail")
	}
	legs, _ := intent["legs"].([]any)
	if len(legs) != 2 {
		t.Fatalf("batch intent row must carry both legs, got %+v", intent)
	}
	for i, l := range legs {
		lm, _ := l.(map[string]any)
		if lm["cloid"] != wire[i] {
			t.Fatalf("intent leg %d cloid = %v, want %s", i, lm["cloid"], wire[i])
		}
	}
}

// Reconcile must tolerate intent rows: skip the unknown action kind gracefully
// and treat an intent-recorded cloid as this instance's own — a live order it
// covers is NOT a foreign orphan (finding 4 pre-check).
func TestReconcileToleratesIntentRows(t *testing.T) {
	const cl = "0x00000000000000000000000000000abc"
	live := `[{"coin":"BTC","oid":7,"limitPx":"60000","origSz":"0.01","sz":"0.01","side":"B","reduceOnly":false,"orderType":"Limit","tif":"Gtc","timestamp":1,"isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0","cloid":"` + cl + `"}]`
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", live, `{}`))
	c.audit.Append(map[string]any{"action": "intent", "cmd": "order", "coin": "BTC", "cloid": cl})
	view, err := c.Reconcile(ctx, ReconcileOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.OrphanOrders) != 0 || !view.Clean {
		t.Fatalf("an intent-recorded order is not a foreign orphan: %+v", view)
	}
	if view.AuditRows == 0 {
		t.Fatal("the intent row should have been scanned")
	}
}

// A post-send transport failure on a copy leg is outcome-UNKNOWN: the cycle
// must STOP (no further legs placed) and surface the leg's cloid in
// unknown_cloids — today it is marked "rejected" and the cycle continues
// (finding 5). The TOOLS.md copy contract keys the stop on exit 42.
func TestCopyExecutePostSendFailureStopsCycle(t *testing.T) {
	var exchangeCalls int32
	srv := rstServer(t, func(map[string]any) { atomic.AddInt32(&exchangeCalls, 1) })
	c, ctx := newClientAt(t, config.Default(), Options{}, srv.URL)
	diff := &CopyDiff{Leader: "0xL", Diff: []DiffLeg{openLeg("BTC", "buy", "0.01"), openLeg("ETH", "buy", "0.1")}}
	res, err := c.CopyExecute(ctx, diff, CopyParams{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Executed != 1 || len(res.Legs) != 1 || res.Legs[0].Status != "unknown" {
		t.Fatalf("post-send failure must stop after the unknown leg (second NOT attempted): %+v", res)
	}
	if len(res.UnknownCloids) != 1 || res.UnknownCloids[0] != res.Legs[0].Cloid {
		t.Fatalf("unknown_cloids must carry the leg's cloid: %+v", res)
	}
	if res.Complete {
		t.Fatal("an unknown leg must mark the cycle incomplete")
	}
	if n := atomic.LoadInt32(&exchangeCalls); n != 1 {
		t.Fatalf("second leg must not reach the exchange, saw %d /exchange calls", n)
	}
}

// Chase whose INITIAL placement fails post-send (outcome unknown) must not
// orphan a possibly-live pegged order: it status-checks the cloid, cancels a
// resting order, and surfaces the cloid in the NDJSON event and the returned
// error (finding 6). Today it bare-returns with no sweep and no cloid.
func TestChaseInitialPlacementUnknownSweepsAndSurfacesCloid(t *testing.T) {
	old := chaseUnknownRecheckDelay
	chaseUnknownRecheckDelay = 10 * time.Millisecond // don't sit through HL's real ~2s indexing lag
	defer func() { chaseUnknownRecheckDelay = old }()
	const book = `{"coin":"BTC","time":1,"levels":[[{"px":"63000","sz":"1","n":1}],[{"px":"63005","sz":"1","n":1}]]}`
	// orderStatus: the order landed and rests ("open") despite the reset.
	status := `{"status":"order","order":{"order":{"coin":"BTC","side":"B","limitPx":"63000","sz":"0.001","oid":71,"origSz":"0.001","reduceOnly":false,"timestamp":1,"isTrigger":false,"triggerPx":"0","triggerCondition":"N/A","isPositionTpsl":false,"orderType":"Limit","tif":"Alo","children":[]},"status":"open","statusTimestamp":1}}`

	var mu sync.Mutex
	var exchangeActions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/info" {
			action, _ := body["action"].(map[string]any)
			typ, _ := action["type"].(string)
			mu.Lock()
			exchangeActions = append(exchangeActions, typ)
			mu.Unlock()
			if typ == "order" {
				// RST the initial placement after fully reading it.
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				if tc, ok := conn.(*net.TCPConn); ok {
					_ = tc.SetLinger(0)
				}
				_ = conn.Close()
				return
			}
			_, _ = io.WriteString(w, cancelOk)
			return
		}
		typ, _ := body["type"].(string)
		switch typ {
		case "l2Book":
			_, _ = io.WriteString(w, book)
		case "orderStatus":
			_, _ = io.WriteString(w, status)
		default:
			serveInfo(w, typ)
		}
	}))
	t.Cleanup(srv.Close)

	c, ctx := newClientAt(t, config.Default(), Options{}, srv.URL)
	const cloid = "0x000000000000000000000000000000cd"
	var events []ChaseEvent
	err := c.Chase(ctx, ChaseParams{Coin: "BTC", Side: Buy, Size: "0.001", Cloid: cloid},
		func(e ChaseEvent) { events = append(events, e) })
	assertErr(t, err, output.CatTimeout, output.ExitTimeout)

	// The possibly-resting order was swept: a cancel action reached the exchange.
	mu.Lock()
	actions := append([]string(nil), exchangeActions...)
	mu.Unlock()
	canceled := false
	for _, a := range actions {
		if strings.HasPrefix(a, "cancel") {
			canceled = true
		}
	}
	if !canceled {
		t.Fatalf("no cancel sweep after an unknown-outcome placement; exchange saw %v", actions)
	}
	// The cloid is surfaced in the NDJSON event stream…
	found := false
	for _, e := range events {
		if e.Cloid == cloid {
			found = true
		}
	}
	if !found {
		t.Fatalf("no event carries the cloid: %+v", events)
	}
	// …and on the returned error (structured, not prose).
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("not an *output.Error: %v", err)
	}
	if len(oe.Cloids) != 1 || oe.Cloids[0] != cloid {
		t.Fatalf("error.cloids = %v, want [%s]", oe.Cloids, cloid)
	}
}
