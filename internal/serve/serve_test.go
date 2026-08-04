package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/output"
)

// fakeEngine records what it was asked and returns canned answers, so the
// server is exercised with no venue, no keychain and no network.
type fakeEngine struct {
	placed   []core.OrderReq
	batched  [][]core.OrderReq
	cancels  []core.CancelReq
	dms      []*int64
	bookCoin string
	bookLvls int

	placeErr    error
	ordersMeta  core.ReadMeta
	ordersError error

	// knownCoins is the outcome universe. bookErrUntilRefresh models the DAILY
	// roll: the coin exists on the venue but not in this process's cache until
	// something reloads it.
	knownCoins map[string]bool
	refreshes  int

	guardReloads int
	guardWarns   []string
	guardGen     string
	universeGen  string
}

// guardWarns is returned by the next ReloadGuardsIfChanged; guardReloads counts
// the calls, so a test can prove the freshness check runs on EVERY request.
func (f *fakeEngine) ReloadGuardsIfChanged(string) []string {
	f.guardReloads++
	w := f.guardWarns
	f.guardWarns = nil
	return w
}

func (f *fakeEngine) UniverseGeneration() string {
	if f.universeGen == "" {
		return "universe fetched 2026-08-04T02:00:00Z, 8 outcome legs, fp deadbeef, signer in sync"
	}
	return f.universeGen
}

func (f *fakeEngine) GuardGeneration() string {
	if f.guardGen == "" {
		return "config generation 1700000000000 (mtime 2023-11-14T22:13:20Z)"
	}
	return f.guardGen
}

func (f *fakeEngine) RefreshOutcomes(context.Context) error {
	f.refreshes++
	if f.knownCoins == nil {
		f.knownCoins = map[string]bool{}
	}
	f.knownCoins["#9450"] = true // the roll's successor becomes visible
	return nil
}

func (f *fakeEngine) Book(_ context.Context, coin string, levels int) (*core.BookView, error) {
	f.bookCoin, f.bookLvls = coin, levels
	if f.knownCoins != nil && strings.HasPrefix(coin, "#") && !f.knownCoins[coin] {
		return nil, output.Validation("unknown_coin", "unknown coin "+coin)
	}
	return &core.BookView{Coin: coin}, nil
}
func (f *fakeEngine) Ctx(context.Context, string) (*core.CtxView, error) {
	return &core.CtxView{}, nil
}
func (f *fakeEngine) Balance(context.Context) (*core.BalanceView, error) {
	return &core.BalanceView{}, nil
}
func (f *fakeEngine) Portfolio(context.Context) (*core.PortfolioView, error) {
	return &core.PortfolioView{}, nil
}
func (f *fakeEngine) Orders(context.Context, string) ([]hl.FrontendOpenOrder, core.ReadMeta, error) {
	if f.ordersError != nil {
		return nil, core.ReadMeta{}, f.ordersError
	}
	return []hl.FrontendOpenOrder{}, f.ordersMeta, nil
}
func (f *fakeEngine) Fills(context.Context, *int64, int) ([]hl.Fill, core.ReadMeta, error) {
	return []hl.Fill{}, core.ReadMeta{}, nil
}
func (f *fakeEngine) Place(_ context.Context, r core.OrderReq) (*core.PlaceResult, []string, error) {
	if f.placeErr != nil {
		return nil, nil, f.placeErr
	}
	f.placed = append(f.placed, r)
	return &core.PlaceResult{Coin: r.Coin, Cloid: "0xabc"}, []string{"a warning"}, nil
}
func (f *fakeEngine) PlaceBatch(_ context.Context, rs []core.OrderReq) ([]*core.PlaceResult, []string, error) {
	f.batched = append(f.batched, rs)
	out := make([]*core.PlaceResult, len(rs))
	for i := range rs {
		out[i] = &core.PlaceResult{Coin: rs[i].Coin}
	}
	return out, nil, nil
}
func (f *fakeEngine) Cancel(_ context.Context, r core.CancelReq) (*core.CancelResult, error) {
	f.cancels = append(f.cancels, r)
	return &core.CancelResult{}, nil
}
func (f *fakeEngine) ScheduleCancel(_ context.Context, d *int64) error {
	f.dms = append(f.dms, d)
	return nil
}

// shortSock returns a socket path inside the kernel's sun_path limit. t.TempDir
// embeds the test NAME, which on darwin's /var/folders prefix blows past 104
// bytes and fails with a bare "bind: invalid argument".
func shortSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dsrv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "d.sock")
}

// start brings up a server on a temp socket and returns a connected client.
func start(t *testing.T, eng Engine) (*bufio.Scanner, net.Conn, *Server) {
	t.Helper()
	sock := shortSock(t)
	srv := New(eng, sock, func() output.Meta { return output.Meta{} })
	if err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("server did not stop")
		}
	})
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return bufio.NewScanner(conn), conn, srv
}

func call(t *testing.T, sc *bufio.Scanner, conn net.Conn, req Request) Response {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !sc.Scan() {
		t.Fatalf("no response: %v", sc.Err())
	}
	var resp Response
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", sc.Text(), err)
	}
	return resp
}

func TestPingReportsTheServedSurface(t *testing.T) {
	sc, conn, _ := start(t, &fakeEngine{})
	r := call(t, sc, conn, Request{ID: "1", Method: "ping"})
	if !r.OK || r.ID != "1" {
		t.Fatalf("ping should succeed and echo the id, got ok=%v id=%q", r.OK, r.ID)
	}
	if r.Schema != output.SchemaVersion {
		t.Errorf("schema should match the CLI envelope, got %q", r.Schema)
	}
}

// The id exists so a caller can pipeline. If it were dropped, a bot could not
// tell which order a reply belonged to.
func TestIDIsEchoedOnEveryReply(t *testing.T) {
	sc, conn, _ := start(t, &fakeEngine{})
	for _, id := range []string{"a", "b", "c"} {
		if r := call(t, sc, conn, Request{ID: id, Method: "balance"}); r.ID != id {
			t.Errorf("id %q came back as %q", id, r.ID)
		}
	}
}

func TestParamsReachTheEngine(t *testing.T) {
	eng := &fakeEngine{}
	sc, conn, _ := start(t, eng)
	call(t, sc, conn, Request{ID: "1", Method: "book",
		Params: json.RawMessage(`{"coin":"#9370","levels":20}`)})
	if eng.bookCoin != "#9370" || eng.bookLvls != 20 {
		t.Errorf("params did not reach the engine: coin=%q levels=%d", eng.bookCoin, eng.bookLvls)
	}
}

func TestPlaceCarriesTheOrderAndItsWarnings(t *testing.T) {
	eng := &fakeEngine{}
	sc, conn, _ := start(t, eng)
	r := call(t, sc, conn, Request{ID: "1", Method: "place",
		Params: json.RawMessage(`{"coin":"BTC","side":"buy","size":"0.001","limit":"60000"}`)})
	if !r.OK {
		t.Fatalf("place failed: %+v", r.Error)
	}
	if len(eng.placed) != 1 || eng.placed[0].Coin != "BTC" {
		t.Fatalf("order did not reach the engine: %+v", eng.placed)
	}
	if len(r.Warnings) != 1 {
		t.Errorf("engine warnings must survive the hop, got %v", r.Warnings)
	}
}

// A gate rejection must arrive with its code, category and retryability intact.
// Flattening it to a string would break the status-check protocol an agent runs
// on an ambiguous write.
func TestGateErrorsArePassedThroughStructured(t *testing.T) {
	eng := &fakeEngine{placeErr: output.Validation("min_notional", "order below the floor").
		WithHint("raise the size")}
	sc, conn, _ := start(t, eng)
	r := call(t, sc, conn, Request{ID: "1", Method: "place",
		Params: json.RawMessage(`{"coin":"BTC","side":"buy","size":"0.0000001"}`)})
	if r.OK {
		t.Fatal("a rejected order must not report ok")
	}
	if r.Error == nil || r.Error.Code != "min_notional" {
		t.Fatalf("error code lost across the socket: %+v", r.Error)
	}
	if r.Error.Category != output.CatValidation {
		t.Errorf("category lost: %v", r.Error.Category)
	}
	if r.Error.Hint == "" {
		t.Error("hint lost")
	}
}

// A degraded read must stay degraded across the socket. #30 removed exactly this
// failure from reconcile; a server that flattened it would put it back.
func TestDegradedReadsStayVisible(t *testing.T) {
	eng := &fakeEngine{ordersMeta: core.ReadMeta{Degraded: []string{"xyz"}}}
	sc, conn, _ := start(t, eng)
	r := call(t, sc, conn, Request{ID: "1", Method: "orders"})
	if !r.OK {
		t.Fatalf("a degraded read still succeeds: %+v", r.Error)
	}
	if len(r.DegradedDexs) != 1 || r.DegradedDexs[0] != "xyz" {
		t.Errorf("degraded dexs lost: %v", r.DegradedDexs)
	}
	if len(r.Warnings) == 0 {
		t.Error("a degraded read must carry a warning a human would see")
	}
}

func TestUnknownMethodIsRejectedWithTheMenu(t *testing.T) {
	sc, conn, _ := start(t, &fakeEngine{})
	r := call(t, sc, conn, Request{ID: "1", Method: "rm-rf"})
	if r.OK {
		t.Fatal("an unknown method must fail")
	}
	if r.Error == nil || r.Error.Code != "unknown_method" {
		t.Fatalf("wrong error: %+v", r.Error)
	}
	if r.Error.Hint == "" {
		t.Error("the rejection should say what IS served")
	}
}

// Streams are deliberately not served — they are already one long-lived process
// each. This pins the boundary so nobody adds them without deciding to.
func TestStreamsAreNotServed(t *testing.T) {
	sc, conn, _ := start(t, &fakeEngine{})
	if r := call(t, sc, conn, Request{ID: "1", Method: "stream"}); r.OK {
		t.Error("stream must not be a served method")
	}
}

func TestMalformedLineDoesNotKillTheConnection(t *testing.T) {
	sc, conn, _ := start(t, &fakeEngine{})
	if _, err := conn.Write([]byte("this is not json\n")); err != nil {
		t.Fatal(err)
	}
	if !sc.Scan() {
		t.Fatal("server should answer a malformed line rather than hang up")
	}
	// The connection must still serve the next request.
	if r := call(t, sc, conn, Request{ID: "2", Method: "ping"}); !r.OK {
		t.Error("connection died after one bad line")
	}
}

// THE security property. This socket reaches the signing key; group- or
// world-readable would hand the account to every user on the box.
func TestSocketIsOwnerOnly(t *testing.T) {
	sock := shortSock(t)
	srv := New(&fakeEngine{}, sock, nil)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(sock)
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket is %o, must be 0600 — it can sign orders", perm)
	}
}

// A second server on the same socket must refuse rather than silently steal it:
// two engines on one account is two nonce holders.
func TestASecondServerOnTheSameSocketRefuses(t *testing.T) {
	_, _, srv := start(t, &fakeEngine{})
	second := New(&fakeEngine{}, srv.Addr(), nil)
	if err := second.Listen(); err == nil {
		t.Fatal("a second server must not bind over a live one")
	}
}

// A socket left behind by a killed process must not block startup forever.
func TestAStaleSocketIsReclaimed(t *testing.T) {
	sock := shortSock(t)
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	srv := New(&fakeEngine{}, sock, nil)
	if err := srv.Listen(); err != nil {
		t.Fatalf("a stale socket file must be reclaimed, got %v", err)
	}
	os.Remove(sock)
}

func TestEngineErrorsThatAreNotEnvelopesStillReport(t *testing.T) {
	eng := &fakeEngine{ordersError: errors.New("boom")}
	sc, conn, _ := start(t, eng)
	r := call(t, sc, conn, Request{ID: "1", Method: "orders"})
	if r.OK || r.Error == nil {
		t.Fatal("a plain error must still produce a failure envelope")
	}
}

// THE gate-laundering test. core.OrderReq.Closing exempts an order from the
// NEW-exposure guards (allowlist, limit_only, notional caps) and its own comment
// says "never from CLI/JSON input". If the wire decoded straight into that
// struct, any socket caller could set it and walk an opening order past the risk
// envelope — making this server strictly more powerful than the CLI it mirrors.
func TestClosingCannotBeSetFromTheWire(t *testing.T) {
	eng := &fakeEngine{}
	sc, conn, _ := start(t, eng)
	r := call(t, sc, conn, Request{ID: "1", Method: "place",
		Params: json.RawMessage(`{"coin":"BTC","side":"buy","size":"1","limit":"1",
			"Closing":true,"closing":true,"panicFlatten":true}`)})
	if !r.OK {
		t.Fatalf("place should succeed: %+v", r.Error)
	}
	if len(eng.placed) != 1 {
		t.Fatal("expected one order")
	}
	if eng.placed[0].Closing {
		t.Error("a wire request set Closing — the risk gates can be bypassed from the socket")
	}
}

// core.Side is an int enum whose zero value is Buy, so a missing or misspelled
// side would silently become a BUY. That is the worst available default for the
// field that decides which way money moves.
func TestSideHasNoSafeDefault(t *testing.T) {
	eng := &fakeEngine{}
	sc, conn, _ := start(t, eng)
	for _, params := range []string{
		`{"coin":"BTC","size":"1","limit":"1"}`,
		`{"coin":"BTC","side":"","size":"1","limit":"1"}`,
		`{"coin":"BTC","side":"byu","size":"1","limit":"1"}`,
	} {
		r := call(t, sc, conn, Request{ID: "1", Method: "place", Params: json.RawMessage(params)})
		if r.OK {
			t.Errorf("params %s should be rejected, not defaulted to BUY", params)
		}
	}
	if len(eng.placed) != 0 {
		t.Fatalf("an order reached the engine without an explicit side: %+v", eng.placed)
	}
}

func TestSideAcceptsBothSpellings(t *testing.T) {
	eng := &fakeEngine{}
	sc, conn, _ := start(t, eng)
	call(t, sc, conn, Request{ID: "1", Method: "place",
		Params: json.RawMessage(`{"coin":"BTC","side":"SELL","size":"1","limit":"1"}`)})
	if len(eng.placed) != 1 || eng.placed[0].Side != core.Sell {
		t.Fatalf("SELL did not map to core.Sell: %+v", eng.placed)
	}
}

// Every leg of a batch goes through the same narrowing, and a bad leg must fail
// the whole request rather than being silently dropped — a ladder missing a rung
// is a different strategy than the one the caller asked for.
func TestABadBatchLegRejectsTheWholeBatch(t *testing.T) {
	eng := &fakeEngine{}
	sc, conn, _ := start(t, eng)
	r := call(t, sc, conn, Request{ID: "1", Method: "batch", Params: json.RawMessage(
		`{"orders":[{"coin":"BTC","side":"buy","size":"1"},{"coin":"BTC","size":"1"}]}`)})
	if r.OK {
		t.Fatal("a batch with an invalid leg must not be placed")
	}
	if len(eng.batched) != 0 {
		t.Error("a partial batch reached the engine")
	}
}

// A socket path over the kernel's sun_path limit fails with a bare
// "bind: invalid argument". Say what is actually wrong.
func TestAnOverlongSocketPathSaysWhy(t *testing.T) {
	long := "/tmp/" + string(make([]byte, sunPathMax)) + ".sock"
	err := New(&fakeEngine{}, long, nil).Listen()
	if err == nil {
		t.Fatal("an overlong path must fail")
	}
	if !strings.Contains(err.Error(), "kernel limit") {
		t.Errorf("error should name the length problem, got: %v", err)
	}
}

func TestCancelCarriesEveryTargetingForm(t *testing.T) {
	eng := &fakeEngine{}
	sc, conn, _ := start(t, eng)

	oid := int64(77)
	r := call(t, sc, conn, Request{ID: "1", Method: "cancel",
		Params: json.RawMessage(`{"oid":77,"coin":"BTC"}`)})
	if !r.OK {
		t.Fatalf("cancel by oid failed: %+v", r.Error)
	}
	if len(eng.cancels) != 1 || eng.cancels[0].Oid == nil || *eng.cancels[0].Oid != oid {
		t.Fatalf("oid did not reach the engine: %+v", eng.cancels)
	}
	if eng.cancels[0].Coin != "BTC" {
		t.Errorf("coin lost: %q", eng.cancels[0].Coin)
	}

	// The batch forms matter most: a ladder is cancelled by cloid list in one
	// signed action, and a dropped list would silently leave rungs resting.
	call(t, sc, conn, Request{ID: "2", Method: "cancel",
		Params: json.RawMessage(`{"cloids":["0xa","0xb","0xc"]}`)})
	if got := eng.cancels[1].Cloids; len(got) != 3 {
		t.Errorf("cloid list lost: %v", got)
	}

	call(t, sc, conn, Request{ID: "3", Method: "cancel",
		Params: json.RawMessage(`{"all":true,"coin":"#9370"}`)})
	if !eng.cancels[2].All || eng.cancels[2].Coin != "#9370" {
		t.Errorf("cancel-all lost: %+v", eng.cancels[2])
	}
}

// The dead-man switch is the rail that saves the account if this process dies
// holding orders, and a persistent server is exactly the caller that should be
// heartbeating one. Nil CLEARS; a value arms.
func TestDMSArmsAndClears(t *testing.T) {
	eng := &fakeEngine{}
	sc, conn, _ := start(t, eng)

	if r := call(t, sc, conn, Request{ID: "1", Method: "dms",
		Params: json.RawMessage(`{"deadline_ms":1785000000000}`)}); !r.OK {
		t.Fatalf("arm failed: %+v", r.Error)
	}
	if len(eng.dms) != 1 || eng.dms[0] == nil || *eng.dms[0] != 1785000000000 {
		t.Fatalf("deadline did not reach the engine: %+v", eng.dms)
	}
	if r := call(t, sc, conn, Request{ID: "2", Method: "dms", Params: json.RawMessage(`{}`)}); !r.OK {
		t.Fatalf("clear failed: %+v", r.Error)
	}
	if len(eng.dms) != 2 || eng.dms[1] != nil {
		t.Errorf("a nil deadline must CLEAR, got %+v", eng.dms[1])
	}
}

func TestFillsPassesSinceAndLimit(t *testing.T) {
	sc, conn, _ := start(t, &fakeEngine{})
	if r := call(t, sc, conn, Request{ID: "1", Method: "fills",
		Params: json.RawMessage(`{"since":1785000000000,"limit":50}`)}); !r.OK {
		t.Fatalf("fills failed: %+v", r.Error)
	}
}

// A truncated history read must be as visible as a degraded one: the caller
// stopped at a safety cap and there are more rows, which changes what a
// reconcile means.
func TestTruncatedReadsStayVisible(t *testing.T) {
	eng := &fakeEngine{ordersMeta: core.ReadMeta{Truncated: true}}
	sc, conn, _ := start(t, eng)
	r := call(t, sc, conn, Request{ID: "1", Method: "orders"})
	if !r.Truncated {
		t.Error("truncation lost across the socket")
	}
	if len(r.Warnings) == 0 {
		t.Error("a truncated read needs a warning a human would see")
	}
}

func TestAMissingMethodIsRejected(t *testing.T) {
	sc, conn, _ := start(t, &fakeEngine{})
	r := call(t, sc, conn, Request{ID: "1"})
	if r.OK || r.Error == nil || r.Error.Code != "no_method" {
		t.Fatalf("an empty method must be rejected: %+v", r.Error)
	}
}

func TestBadParamsAreRejectedNotIgnored(t *testing.T) {
	eng := &fakeEngine{}
	sc, conn, _ := start(t, eng)
	r := call(t, sc, conn, Request{ID: "1", Method: "place",
		Params: json.RawMessage(`{"coin":123}`)})
	if r.OK {
		t.Fatal("malformed params must not place an order")
	}
	if len(eng.placed) != 0 {
		t.Error("an order was placed from undecodable params")
	}
}

func TestEmptySocketPathIsRejected(t *testing.T) {
	if err := New(&fakeEngine{}, "", nil).Listen(); err == nil {
		t.Error("an empty socket path must fail rather than bind somewhere surprising")
	}
}

// A nil meta provider must not panic — the server is constructed that way in
// several tests and could be by an embedder.
func TestNilMetaIsSafe(t *testing.T) {
	sock := shortSock(t)
	srv := New(&fakeEngine{}, sock, nil)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if r := call(t, bufio.NewScanner(conn), conn, Request{ID: "1", Method: "ping"}); !r.OK {
		t.Error("a nil meta provider should still answer")
	}
}

// THE roll regression. Outcome markets are DAILY and this process outlives the
// roll, so a universe cached at startup stops recognising the coin the moment
// the binary rolls — and every order fails "unknown coin" while the decision
// layer, reading a stream that resolves coins independently, believes it is
// quoting normally.
//
// It is not hypothetical: it cost a full session. A downstream maker sat at 398
// consecutive placement failures against a market it could see perfectly well.
func TestAnUnknownOutcomeCoinTriggersOneReloadAndRetry(t *testing.T) {
	eng := &fakeEngine{knownCoins: map[string]bool{"#9370": true}}
	sc, conn, _ := start(t, eng)

	// The coin that existed before the roll still works, and must NOT refresh.
	if r := call(t, sc, conn, Request{ID: "1", Method: "book",
		Params: json.RawMessage(`{"coin":"#9370","levels":2}`)}); !r.OK {
		t.Fatalf("a known coin should not fail: %+v", r.Error)
	}
	if eng.refreshes != 0 {
		t.Errorf("a successful call must not reload the universe (%d reloads)", eng.refreshes)
	}

	// The roll's successor: unknown, so reload once and retry.
	r := call(t, sc, conn, Request{ID: "2", Method: "book",
		Params: json.RawMessage(`{"coin":"#9450","levels":2}`)})
	if !r.OK {
		t.Fatalf("a rolled-to coin must succeed after the reload: %+v", r.Error)
	}
	if eng.refreshes != 1 {
		t.Errorf("want exactly one reload, got %d", eng.refreshes)
	}
}

// A genuinely bad coin must not turn every request into an API fetch.
func TestAGenuinelyUnknownCoinReloadsAtMostOnce(t *testing.T) {
	eng := &fakeEngine{knownCoins: map[string]bool{"#9370": true}}
	sc, conn, _ := start(t, eng)
	for i := 0; i < 4; i++ {
		if r := call(t, sc, conn, Request{ID: "x", Method: "book",
			Params: json.RawMessage(`{"coin":"#0000","levels":2}`)}); r.OK {
			t.Fatal("a coin that does not exist must still fail")
		}
	}
	if eng.refreshes > 1 {
		t.Errorf("the reload must be rate-limited, got %d fetches", eng.refreshes)
	}
}

// Only unknown_coin gets the retry. Anything else could have reached the venue.
func TestOtherErrorsAreNotRetried(t *testing.T) {
	eng := &fakeEngine{placeErr: output.Validation("min_notional", "too small")}
	sc, conn, _ := start(t, eng)
	r := call(t, sc, conn, Request{ID: "1", Method: "place",
		Params: json.RawMessage(`{"coin":"BTC","side":"buy","size":"1","limit":"1","tif":"Alo"}`)})
	if r.OK || r.Error.Code != "min_notional" {
		t.Fatalf("unexpected: %+v", r.Error)
	}
	if eng.refreshes != 0 {
		t.Error("only unknown_coin may trigger a reload — other failures may have reached the venue")
	}
}
