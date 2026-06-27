package cmd

// RunE coverage for the read commands in reads.go (portfolio, positions, orders,
// fills, funding, ledger, balance, pnl[.attribution], book, bbo, candles, ctx,
// limits, predicted-fundings, historical-orders, markets, builder status, order
// status). `mids` is covered elsewhere and skipped here; `referral` is a separate
// group and excluded.
//
// NOTE ON THE HARNESS: the prescribed shared client-seam harness
// (withFakeClient/runCmd/resetWriteFlags + a core.ClientAPI interface) is NOT
// present on this worktree — the RunE handlers build a concrete *core.Client via
// the unexported newClient (no interface seam to override). So this file stands up
// a self-contained, fully-offline harness instead: an httptest server speaks the
// HL /info protocol on loopback, a seeded meta cache makes core.New construct
// without any network fetch, and the real RunE handlers run end-to-end against it.
// Every identifier added here is prefixed `rd` to avoid collisions when the cmd
// test files are combined. No t.Parallel: Cfg / os.Args / the read-flag globals
// are process-wide.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/core"
	hl "github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/output"
)

// rdEnvelope is a minimal view of the schema-v1 JSON envelope every command emits.
type rdEnvelope struct {
	OK       bool            `json:"ok"`
	Cmd      string          `json:"cmd"`
	Data     json.RawMessage `json:"data"`
	Warnings []string        `json:"warnings"`
	Error    struct {
		Code     string `json:"code"`
		Category string `json:"category"`
	} `json:"error"`
}

const rdMasterAddr = "0x9ccAcA47f0318FaeF9C8175767a15AEe1586177e"

// rdServer is a fake HL /info endpoint. It dispatches on the request body's "type"
// field, records every request body it saw (so a test can assert flags/args
// reached the call), and lets a test force an error for a given type.
type rdServer struct {
	t        *testing.T
	mu       sync.Mutex
	requests []map[string]any
	// responses maps an info "type" to a canned JSON body. A missing type returns
	// an empty-but-valid body for its shape (see defaultBody).
	responses map[string]string
	// failTypes maps an info "type" to true to make the server answer HTTP 500
	// (which the read layer maps to a network error, exit 40).
	failTypes map[string]bool
}

func rdNewServer(t *testing.T) (*rdServer, *httptest.Server) {
	s := &rdServer{t: t, responses: map[string]string{}, failTypes: map[string]bool{}}
	ts := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(ts.Close)
	return s, ts
}

func (s *rdServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	typ, _ := req["type"].(string)

	s.mu.Lock()
	s.requests = append(s.requests, req)
	fail := s.failTypes[typ]
	resp, custom := s.responses[typ]
	s.mu.Unlock()

	if fail {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
		return
	}
	if !custom {
		resp = rdDefaultBody(typ)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, resp)
}

// requestFor returns the most recent recorded request for the given info type.
func (s *rdServer) requestFor(typ string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.requests) - 1; i >= 0; i-- {
		if t, _ := s.requests[i]["type"].(string); t == typ {
			return s.requests[i]
		}
	}
	return nil
}

func (s *rdServer) setResp(typ, body string) { s.mu.Lock(); s.responses[typ] = body; s.mu.Unlock() }
func (s *rdServer) fail(typ string)          { s.mu.Lock(); s.failTypes[typ] = true; s.mu.Unlock() }

// rdDefaultBody is a valid empty-ish response for each info type the read surface
// uses, so a happy-path test only needs to override the one type it asserts on.
func rdDefaultBody(typ string) string {
	switch typ {
	case "clearinghouseState":
		return `{"assetPositions":[],"marginSummary":{"accountValue":"0.0","totalMarginUsed":"0.0","totalNtlPos":"0.0","totalRawUsd":"0.0"},"crossMarginSummary":{"accountValue":"0.0","totalMarginUsed":"0.0","totalNtlPos":"0.0","totalRawUsd":"0.0"},"withdrawable":"0.0"}`
	case "spotClearinghouseState":
		return `{"balances":[]}`
	case "frontendOpenOrders":
		return `[]`
	case "userFills", "userFillsByTime":
		return `[]`
	case "userFunding":
		return `[]`
	case "userNonFundingLedgerUpdates":
		return `[]`
	case "historicalOrders":
		return `[]`
	case "predictedFundings":
		return `[]`
	case "allMids":
		return `{}`
	case "portfolio":
		return `[]`
	case "l2Book":
		return `{"coin":"BTC","time":1,"levels":[[],[]]}`
	case "candleSnapshot":
		return `[]`
	case "userRateLimit":
		return `{"cumVlm":"0","nRequestsUsed":0,"nRequestsCap":0,"nRequestsSurplus":0}`
	case "orderStatus":
		return `{"status":"unknownOid"}`
	case "outcomeMeta":
		return `{"outcomes":[],"questions":[]}`
	default:
		return `{}`
	}
}

// rdMeta / rdSpotMeta are the seeded meta cache: a tiny perp+spot universe so
// core.New constructs without any network fetch and BTC / @1 resolve.
func rdSeedMetaCache(t *testing.T, dir string) {
	t.Helper()
	meta := &hl.Meta{
		Universe: []hl.AssetInfo{
			{Name: "BTC", SzDecimals: 5, MaxLeverage: 50},
			{Name: "ETH", SzDecimals: 4, MaxLeverage: 50},
		},
	}
	spot := &hl.SpotMeta{
		Universe: []hl.SpotAssetInfo{{Name: "PURR/USDC", Tokens: []int{1, 0}, Index: 0}},
		Tokens: []hl.SpotTokenInfo{
			{Name: "USDC", Index: 0, SzDecimals: 2},
			{Name: "PURR", Index: 1, SzDecimals: 2},
		},
	}
	cache := struct {
		Network   string       `json:"network"`
		FetchedAt int64        `json:"fetched_at_ms"`
		Meta      *hl.Meta     `json:"meta"`
		SpotMeta  *hl.SpotMeta `json:"spot_meta"`
	}{
		Network:   config.NetworkTestnet,
		FetchedAt: time.Now().UnixMilli(),
		Meta:      meta,
		SpotMeta:  spot,
	}
	b, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// rdSetup wires a fresh DELIVERATOR_HOME + seeded meta + a fake /info server, points
// Cfg at it, and returns the server. It resets the read-flag globals and os.Args so
// each command starts from a clean slate. All globals are restored on cleanup.
func rdSetup(t *testing.T) *rdServer {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DELIVERATOR_HOME", home)
	rdSeedMetaCache(t, home)

	srv, ts := rdNewServer(t)

	cfg := config.Default()
	cfg.Network = config.NetworkTestnet
	cfg.Wallet.MasterAddress = rdMasterAddr
	cfg.Endpoints.InfoURL = ts.URL // http loopback; only the in-memory struct, no Validate
	cfg.State.Audit = false        // keep tests side-effect free

	saveCfg, saveArgs := Cfg, os.Args
	saveCoin, saveSince, saveLimit, saveLevels := rCoin, rSince, rLimit, rLevels
	saveInterval, saveClass, saveStatus := rInterval, rClass, rStatus
	saveOid, saveCloid := rOid, rCloid
	Cfg = cfg
	rCoin, rSince, rLimit, rLevels = "", 0, 0, 0
	rInterval, rClass, rStatus, rOid, rCloid = "", "", "", 0, ""
	os.Args = []string{"deliverator", "test"} // benign: argsReferenceOutcomes()==false

	output.Configure(true, io.Discard)
	t.Cleanup(func() {
		Cfg, os.Args = saveCfg, saveArgs
		rCoin, rSince, rLimit, rLevels = saveCoin, saveSince, saveLimit, saveLevels
		rInterval, rClass, rStatus = saveInterval, saveClass, saveStatus
		rOid, rCloid = saveOid, saveCloid
		output.Configure(true, nil)
	})
	return srv
}

// rdRun runs a command's RunE with the given args, captures the emitted JSON
// envelope, and returns it alongside the RunE error.
func rdRun(t *testing.T, c *cobra.Command, args []string) (rdEnvelope, error) {
	t.Helper()
	var buf strings.Builder
	output.Configure(true, &buf)
	err := c.RunE(c, args)
	var env rdEnvelope
	if s := buf.String(); s != "" {
		if e := json.Unmarshal([]byte(s), &env); e != nil {
			t.Fatalf("%s: expected one JSON envelope, got %q: %v", c.Name(), s, e)
		}
	}
	return env, err
}

// rdExitCode returns the exit code carried by a RunE error (0 for nil).
func rdExitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return output.ExitOK
	}
	ce, ok := err.(*output.CmdError)
	if !ok {
		t.Fatalf("expected *output.CmdError, got %T: %v", err, err)
	}
	return ce.Code
}

// rdAssertOK asserts a success envelope for cmd, with Data present.
func rdAssertOK(t *testing.T, env rdEnvelope, err error, wantCmd string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: unexpected RunE error: %v", wantCmd, err)
	}
	if !env.OK {
		t.Fatalf("%s: env.OK = false, want true (%+v)", wantCmd, env)
	}
	if env.Cmd != wantCmd {
		t.Fatalf("%s: env.Cmd = %q, want %q", wantCmd, env.Cmd, wantCmd)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		t.Fatalf("%s: env.Data is empty/null", wantCmd)
	}
}

// rdAssertFail asserts a failure envelope with the given category + exit code.
func rdAssertFail(t *testing.T, env rdEnvelope, err error, wantCat string, wantExit int) {
	t.Helper()
	if env.OK {
		t.Fatalf("expected failure envelope, got OK (%+v)", env)
	}
	if env.Error.Category != wantCat {
		t.Fatalf("error category = %q, want %q (%+v)", env.Error.Category, wantCat, env)
	}
	if got := rdExitCode(t, err); got != wantExit {
		t.Fatalf("exit code = %d, want %d", got, wantExit)
	}
}

// ---- happy + error paths, per command ----

func TestReadPortfolioRunE(t *testing.T) {
	srv := rdSetup(t)
	srv.setResp("clearinghouseState", `{"assetPositions":[],"marginSummary":{"accountValue":"123.45","totalMarginUsed":"10.0","totalNtlPos":"50.0","totalRawUsd":"123.45"},"crossMarginSummary":{"accountValue":"123.45","totalMarginUsed":"10.0","totalNtlPos":"50.0","totalRawUsd":"123.45"},"withdrawable":"100.0"}`)
	env, err := rdRun(t, portfolioCmd, nil)
	rdAssertOK(t, env, err, "portfolio")
	var pv core.PortfolioView
	if e := json.Unmarshal(env.Data, &pv); e != nil {
		t.Fatalf("decode PortfolioView: %v", e)
	}
	if pv.AccountValue != "123.45" || pv.Address != rdMasterAddr {
		t.Fatalf("portfolio data wrong: %+v", pv)
	}

	srv.fail("clearinghouseState")
	env, err = rdRun(t, portfolioCmd, nil)
	rdAssertFail(t, env, err, "network", output.ExitNetwork)
}

func TestReadPositionsRunE(t *testing.T) {
	srv := rdSetup(t)
	srv.setResp("clearinghouseState", `{"assetPositions":[{"type":"oneWay","position":{"coin":"BTC","szi":"0.5","entryPx":"60000","positionValue":"30000","unrealizedPnl":"100","returnOnEquity":"0.01","marginUsed":"600","liquidationPx":"50000","leverage":{"type":"cross","value":50}}}],"marginSummary":{"accountValue":"1","totalMarginUsed":"0","totalNtlPos":"0","totalRawUsd":"0"},"crossMarginSummary":{"accountValue":"1","totalMarginUsed":"0","totalNtlPos":"0","totalRawUsd":"0"},"withdrawable":"0"}`)

	// --coin filter must reach the read (we filter to BTC and keep the row).
	rCoin = "BTC"
	env, err := rdRun(t, positionsCmd, nil)
	rdAssertOK(t, env, err, "positions")
	var pvs []core.PositionView
	if e := json.Unmarshal(env.Data, &pvs); e != nil {
		t.Fatalf("decode positions: %v", e)
	}
	if len(pvs) != 1 || pvs[0].Coin != "BTC" || pvs[0].Side != "long" {
		t.Fatalf("positions data wrong: %+v", pvs)
	}

	srv.fail("clearinghouseState")
	env, err = rdRun(t, positionsCmd, nil)
	rdAssertFail(t, env, err, "network", output.ExitNetwork)
}

func TestReadOrdersRunE(t *testing.T) {
	srv := rdSetup(t)
	srv.setResp("frontendOpenOrders", `[{"coin":"BTC","oid":42,"side":"B","limitPx":"60000","sz":"0.1","origSz":"0.1","orderType":"Limit","tif":"Gtc","timestamp":1,"triggerPx":"0"}]`)
	env, err := rdRun(t, ordersCmd, nil)
	rdAssertOK(t, env, err, "orders")
	var oo []hl.FrontendOpenOrder
	if e := json.Unmarshal(env.Data, &oo); e != nil {
		t.Fatalf("decode orders: %v", e)
	}
	if len(oo) != 1 || oo[0].Oid != 42 {
		t.Fatalf("orders data wrong: %+v", oo)
	}

	srv.fail("frontendOpenOrders")
	env, err = rdRun(t, ordersCmd, nil)
	rdAssertFail(t, env, err, "network", output.ExitNetwork)
}

func TestReadFillsRunE(t *testing.T) {
	srv := rdSetup(t)
	rLimit = 100
	srv.setResp("userFills", `[{"coin":"BTC","px":"60000","sz":"0.1","side":"B","time":2,"oid":1,"dir":"Open Long","closedPnl":"0","fee":"0.5","feeToken":"USDC","hash":"0x","startPosition":"0","tid":7}]`)
	env, err := rdRun(t, fillsCmd, nil)
	rdAssertOK(t, env, err, "fills")
	var fills []hl.Fill
	if e := json.Unmarshal(env.Data, &fills); e != nil {
		t.Fatalf("decode fills: %v", e)
	}
	if len(fills) != 1 || fills[0].Coin != "BTC" {
		t.Fatalf("fills data wrong: %+v", fills)
	}

	srv.fail("userFills")
	env, err = rdRun(t, fillsCmd, nil)
	rdAssertFail(t, env, err, "network", output.ExitNetwork)
}

func TestReadFundingRunE(t *testing.T) {
	srv := rdSetup(t)
	srv.setResp("userFunding", `[{"hash":"0x","time":3,"delta":{"coin":"BTC","fundingRate":"0.0001","size":"0.1"}}]`)
	env, err := rdRun(t, fundingCmd, nil)
	rdAssertOK(t, env, err, "funding")

	srv.fail("userFunding")
	env, err = rdRun(t, fundingCmd, nil)
	rdAssertFail(t, env, err, "network", output.ExitNetwork)
}

func TestReadLedgerRunE(t *testing.T) {
	srv := rdSetup(t)
	srv.setResp("userNonFundingLedgerUpdates", `[{"hash":"0x","time":4,"delta":{"type":"deposit","usdc":"100"}}]`)
	env, err := rdRun(t, ledgerCmd, nil)
	rdAssertOK(t, env, err, "ledger")

	srv.fail("userNonFundingLedgerUpdates")
	env, err = rdRun(t, ledgerCmd, nil)
	rdAssertFail(t, env, err, "network", output.ExitNetwork)
}

func TestReadBalanceRunE(t *testing.T) {
	srv := rdSetup(t)
	srv.setResp("clearinghouseState", `{"assetPositions":[],"marginSummary":{"accountValue":"500.0","totalMarginUsed":"0","totalNtlPos":"0","totalRawUsd":"500"},"crossMarginSummary":{"accountValue":"500.0","totalMarginUsed":"0","totalNtlPos":"0","totalRawUsd":"500"},"withdrawable":"500.0"}`)
	srv.setResp("spotClearinghouseState", `{"balances":[{"coin":"USDC","token":0,"hold":"0","total":"250","entryNtl":"0"}],"tokenToAvailableAfterMaintenance":[[0,"250.0"]]}`)
	env, err := rdRun(t, balanceCmd, nil)
	rdAssertOK(t, env, err, "balance")
	var bv core.BalanceView
	if e := json.Unmarshal(env.Data, &bv); e != nil {
		t.Fatalf("decode balance: %v", e)
	}
	if bv.Perp.AccountValue != "500.0" || bv.AvailableCollateral != "250.0" {
		t.Fatalf("balance data wrong: %+v", bv)
	}

	srv.fail("clearinghouseState")
	env, err = rdRun(t, balanceCmd, nil)
	rdAssertFail(t, env, err, "network", output.ExitNetwork)
}

func TestReadPnlRunE(t *testing.T) {
	srv := rdSetup(t)
	srv.setResp("portfolio", `[["day",{"accountValueHistory":[[1,"100"]],"pnlHistory":[[1,"5"]],"vlm":"1000"}]]`)
	env, err := rdRun(t, pnlCmd, nil)
	rdAssertOK(t, env, err, "pnl")
	var windows []core.PnlWindow
	if e := json.Unmarshal(env.Data, &windows); e != nil {
		t.Fatalf("decode pnl: %v", e)
	}
	if len(windows) != 1 || windows[0].Window != "day" || windows[0].Vlm != "1000" {
		t.Fatalf("pnl data wrong: %+v", windows)
	}

	srv.fail("portfolio")
	env, err = rdRun(t, pnlCmd, nil)
	rdAssertFail(t, env, err, "network", output.ExitNetwork)
}

func TestReadPnlAttributionRunE(t *testing.T) {
	srv := rdSetup(t)
	srv.setResp("userFills", `[{"coin":"BTC","px":"60000","sz":"0.1","side":"B","time":2,"oid":1,"dir":"Close Long","closedPnl":"12.5","fee":"0.5","feeToken":"USDC","builderFee":"0.1","hash":"0x","startPosition":"0.1","tid":7}]`)
	srv.setResp("userFunding", `[{"hash":"0x","time":3,"delta":{"coin":"BTC","fundingRate":"0.0001","size":"0.1"}}]`)
	env, err := rdRun(t, pnlAttributionCmd, nil)
	rdAssertOK(t, env, err, "pnl.attribution")

	srv.fail("userFills")
	env, err = rdRun(t, pnlAttributionCmd, nil)
	rdAssertFail(t, env, err, "network", output.ExitNetwork)
}

func TestReadBookRunE(t *testing.T) {
	srv := rdSetup(t)
	srv.setResp("l2Book", `{"coin":"BTC","time":99,"levels":[[{"px":"59999","sz":"1","n":2},{"px":"59998","sz":"2","n":1}],[{"px":"60001","sz":"1","n":1},{"px":"60002","sz":"3","n":2}]]}`)

	// --levels must reach Book(): with 1 level we keep exactly one per side.
	rLevels = 1
	env, err := rdRun(t, bookCmd, []string{"BTC"})
	rdAssertOK(t, env, err, "book")
	var bv core.BookView
	if e := json.Unmarshal(env.Data, &bv); e != nil {
		t.Fatalf("decode book: %v", e)
	}
	if len(bv.Bids) != 1 || len(bv.Asks) != 1 || bv.Bids[0].Px != "59999" {
		t.Fatalf("book levels not trimmed to --levels=1: %+v", bv)
	}
	// The coin arg reached the /info request.
	if req := srv.requestFor("l2Book"); req == nil || req["coin"] != "BTC" {
		t.Fatalf("l2Book request coin wrong: %+v", req)
	}

	// Unknown coin is rejected offline (no network) as a validation error.
	env, err = rdRun(t, bookCmd, []string{"NOPE"})
	rdAssertFail(t, env, err, "validation", output.ExitValidation)
	if env.Error.Code != "unknown_coin" {
		t.Fatalf("want unknown_coin, got %q", env.Error.Code)
	}

	srv.fail("l2Book")
	env, err = rdRun(t, bookCmd, []string{"BTC"})
	rdAssertFail(t, env, err, "network", output.ExitNetwork)
}

func TestReadBboRunE(t *testing.T) {
	srv := rdSetup(t)
	srv.setResp("l2Book", `{"coin":"ETH","time":7,"levels":[[{"px":"2999","sz":"1","n":1}],[{"px":"3001","sz":"1","n":1}]]}`)
	env, err := rdRun(t, bboCmd, []string{"ETH"})
	rdAssertOK(t, env, err, "bbo")
	var bbo core.BboView
	if e := json.Unmarshal(env.Data, &bbo); e != nil {
		t.Fatalf("decode bbo: %v", e)
	}
	if bbo.Bid != "2999" || bbo.Ask != "3001" || bbo.Mid != "3000" {
		t.Fatalf("bbo data wrong: %+v", bbo)
	}
	if req := srv.requestFor("l2Book"); req == nil || req["coin"] != "ETH" {
		t.Fatalf("l2Book request coin wrong: %+v", req)
	}

	env, err = rdRun(t, bboCmd, []string{"NOPE"})
	rdAssertFail(t, env, err, "validation", output.ExitValidation)
}

func TestReadCandlesRunE(t *testing.T) {
	srv := rdSetup(t)
	srv.setResp("candleSnapshot", `[{"t":1,"T":2,"i":"5m","n":3,"o":"1","h":"2","l":"0.5","c":"1.5","s":"BTC","v":"10"}]`)

	// --interval (rInterval) + coin arg must reach the candleSnapshot req.
	rInterval = "5m"
	env, err := rdRun(t, candlesCmd, []string{"BTC"})
	rdAssertOK(t, env, err, "candles")
	var cs []hl.Candle
	if e := json.Unmarshal(env.Data, &cs); e != nil {
		t.Fatalf("decode candles: %v", e)
	}
	if len(cs) != 1 || cs[0].Interval != "5m" {
		t.Fatalf("candles data wrong: %+v", cs)
	}
	if req := srv.requestFor("candleSnapshot"); req != nil {
		if inner, ok := req["req"].(map[string]any); !ok || inner["coin"] != "BTC" || inner["interval"] != "5m" {
			t.Fatalf("candleSnapshot req wrong: %+v", req)
		}
	} else {
		t.Fatal("no candleSnapshot request recorded")
	}

	env, err = rdRun(t, candlesCmd, []string{"NOPE"})
	rdAssertFail(t, env, err, "validation", output.ExitValidation)

	srv.fail("candleSnapshot")
	env, err = rdRun(t, candlesCmd, []string{"BTC"})
	rdAssertFail(t, env, err, "network", output.ExitNetwork)
}

func TestReadCtxRunE(t *testing.T) {
	srv := rdSetup(t)
	srv.setResp("metaAndAssetCtxs", `[{"universe":[{"name":"BTC","szDecimals":5,"maxLeverage":50}],"marginTables":[]},[{"funding":"0.0001","openInterest":"100","prevDayPx":"59000","dayNtlVlm":"1000000","premium":"0.0","oraclePx":"60000","markPx":"60010","midPx":"60005","impactPxs":["59990","60020"]}]]`)
	env, err := rdRun(t, ctxCmd, []string{"BTC"})
	rdAssertOK(t, env, err, "ctx")
	var cv core.CtxView
	if e := json.Unmarshal(env.Data, &cv); e != nil {
		t.Fatalf("decode ctx: %v", e)
	}
	if cv.Coin != "BTC" || cv.MarkPx != "60010" || cv.Funding != "0.0001" {
		t.Fatalf("ctx data wrong: %+v", cv)
	}

	env, err = rdRun(t, ctxCmd, []string{"NOPE"})
	rdAssertFail(t, env, err, "validation", output.ExitValidation)

	srv.fail("metaAndAssetCtxs")
	env, err = rdRun(t, ctxCmd, []string{"BTC"})
	rdAssertFail(t, env, err, "network", output.ExitNetwork)
}

func TestReadLimitsRunE(t *testing.T) {
	srv := rdSetup(t)
	srv.setResp("userRateLimit", `{"cumVlm":"123456","nRequestsUsed":40,"nRequestsCap":1000,"nRequestsSurplus":0}`)
	env, err := rdRun(t, limitsCmd, nil)
	rdAssertOK(t, env, err, "limits")
	var lv core.LimitsView
	if e := json.Unmarshal(env.Data, &lv); e != nil {
		t.Fatalf("decode limits: %v", e)
	}
	if lv.Used != 40 || lv.Cap != 1000 || lv.Remaining != 960 {
		t.Fatalf("limits data wrong: %+v", lv)
	}
	if req := srv.requestFor("userRateLimit"); req == nil || req["user"] != rdMasterAddr {
		t.Fatalf("userRateLimit request user wrong: %+v", req)
	}

	// Limits goes through Client.InfoPost (not the hl transport): a non-200 maps to
	// an exchange error (info_http, exit 50), not a network error.
	srv.fail("userRateLimit")
	env, err = rdRun(t, limitsCmd, nil)
	rdAssertFail(t, env, err, "exchange", output.ExitExchange)
}

func TestReadPredictedFundingsRunE(t *testing.T) {
	srv := rdSetup(t)
	srv.setResp("predictedFundings", `[["BTC",[["HlPerp",{"fundingRate":"0.0001","nextFundingTime":1700000000000,"fundingIntervalHours":8}]]]]`)

	// --coin filter (rCoin) keeps only BTC.
	rCoin = "BTC"
	env, err := rdRun(t, predictedFundingsCmd, nil)
	rdAssertOK(t, env, err, "predicted-fundings")
	var pf []hl.PredictedFunding
	if e := json.Unmarshal(env.Data, &pf); e != nil {
		t.Fatalf("decode predicted-fundings: %v", e)
	}
	if len(pf) != 1 || pf[0].Coin != "BTC" {
		t.Fatalf("predicted-fundings data wrong: %+v", pf)
	}

	srv.fail("predictedFundings")
	env, err = rdRun(t, predictedFundingsCmd, nil)
	rdAssertFail(t, env, err, "network", output.ExitNetwork)
}

func TestReadHistoricalOrdersRunE(t *testing.T) {
	srv := rdSetup(t)
	rLimit = 100
	srv.setResp("historicalOrders", `[{"status":"filled","statusTimestamp":10,"order":{"coin":"BTC","side":"B","limitPx":"60000","sz":"0","oid":99,"timestamp":9,"triggerCondition":"N/A","isTrigger":false,"triggerPx":"0","children":[],"isPositionTpsl":false,"reduceOnly":false,"orderType":"Limit","origSz":"0.1","tif":"Gtc","cloid":null}}]`)
	env, err := rdRun(t, historicalOrdersCmd, nil)
	rdAssertOK(t, env, err, "historical-orders")
	var ho []hl.OrderQueryResponse
	if e := json.Unmarshal(env.Data, &ho); e != nil {
		t.Fatalf("decode historical-orders: %v", e)
	}
	if len(ho) != 1 || ho[0].Order.Oid != 99 {
		t.Fatalf("historical-orders data wrong: %+v", ho)
	}

	srv.fail("historicalOrders")
	env, err = rdRun(t, historicalOrdersCmd, nil)
	rdAssertFail(t, env, err, "network", output.ExitNetwork)
}

func TestReadMarketsRunE(t *testing.T) {
	rdSetup(t)
	// Default class: perps + spot, from the seeded meta — no network call.
	env, err := rdRun(t, marketsCmd, nil)
	rdAssertOK(t, env, err, "markets")
	var mk []core.Market
	if e := json.Unmarshal(env.Data, &mk); e != nil {
		t.Fatalf("decode markets: %v", e)
	}
	if len(mk) != 3 { // BTC, ETH (perp) + PURR/USDC (spot)
		t.Fatalf("default markets count = %d, want 3: %+v", len(mk), mk)
	}

	// --class perp filters out spot.
	rClass = "perp"
	env, err = rdRun(t, marketsCmd, nil)
	rdAssertOK(t, env, err, "markets")
	if e := json.Unmarshal(env.Data, &mk); e != nil {
		t.Fatalf("decode markets perp: %v", e)
	}
	if len(mk) != 2 {
		t.Fatalf("--class perp count = %d, want 2: %+v", len(mk), mk)
	}

	// A bad --class is a validation error, decided locally.
	rClass = "bogus"
	env, err = rdRun(t, marketsCmd, nil)
	rdAssertFail(t, env, err, "validation", output.ExitValidation)
	if env.Error.Code != "bad_class" {
		t.Fatalf("want bad_class, got %q", env.Error.Code)
	}

	// --coin selects one market (also exercised here).
	rClass = ""
	rCoin = "BTC"
	env, err = rdRun(t, marketsCmd, nil)
	rdAssertOK(t, env, err, "markets")
	var one core.Market
	if e := json.Unmarshal(env.Data, &one); e != nil {
		t.Fatalf("decode single market: %v", e)
	}
	if one.Coin != "BTC" {
		t.Fatalf("--coin BTC wrong: %+v", one)
	}

	// --coin unknown is a validation error.
	rCoin = "NOPE"
	env, err = rdRun(t, marketsCmd, nil)
	rdAssertFail(t, env, err, "validation", output.ExitValidation)
}

func TestReadMarketsOutcomeClassRunE(t *testing.T) {
	srv := rdSetup(t)
	// `--class outcome` triggers the lazy outcome load in newClient, which only fires
	// when argsReferenceOutcomes() sees the selector on os.Args.
	os.Args = []string{"deliverator", "markets", "--class", "outcome"}
	srv.setResp("outcomeMeta", `{"outcomes":[{"outcome":641,"name":"BTC above 60k","description":"class:priceBinary|underlying:BTC|expiry:20260625-0600|targetPrice:60000|period:1d","sideSpecs":[{"name":"Yes"},{"name":"No"}],"quoteToken":"USDC"}],"questions":[]}`)

	rClass = "outcome"
	env, err := rdRun(t, marketsCmd, nil)
	rdAssertOK(t, env, err, "markets")
	var mk []core.Market
	if e := json.Unmarshal(env.Data, &mk); e != nil {
		t.Fatalf("decode outcome markets: %v", e)
	}
	if len(mk) != 2 { // Yes (#6410) + No (#6411)
		t.Fatalf("--class outcome count = %d, want 2: %+v", len(mk), mk)
	}
	if !mk[0].IsOutcome || mk[0].Coin != "#6410" {
		t.Fatalf("outcome market wrong: %+v", mk[0])
	}
}

func TestReadBuilderStatusRunE(t *testing.T) {
	srv := rdSetup(t)
	// The builder fee ships ON by default (address + attach_mode=all); builder status
	// reports that posture plus the master-approved max (0 = unapproved here).
	srv.setResp("maxBuilderFee", `0`)
	env, err := rdRun(t, builderStatusCmd, nil)
	rdAssertOK(t, env, err, "builder.status")
	var bv core.BuilderView
	if e := json.Unmarshal(env.Data, &bv); e != nil {
		t.Fatalf("decode builder status: %v", e)
	}
	if bv.AttachMode != config.AttachAll {
		t.Fatalf("builder attach_mode = %q, want %q", bv.AttachMode, config.AttachAll)
	}
	if bv.Address != config.DefaultBuilderAddress || bv.FeeTenthsBps != config.DefaultBuilderFeeTenthsBps {
		t.Fatalf("builder status should reflect the shipped default, got %+v", bv)
	}
	_ = srv
}

func TestReadOrderStatusRunE(t *testing.T) {
	srv := rdSetup(t)
	srv.setResp("orderStatus", `{"status":"order","order":{"status":"open","statusTimestamp":11,"order":{"coin":"BTC","side":"B","limitPx":"60000","sz":"0.1","oid":555,"timestamp":10,"triggerCondition":"N/A","isTrigger":false,"triggerPx":"0","children":[],"isPositionTpsl":false,"reduceOnly":false,"orderType":"Limit","origSz":"0.1","tif":"Gtc","cloid":null}}}`)

	// --oid must reach the orderStatus request as "oid".
	rOid = 555
	env, err := rdRun(t, orderStatusCmd, nil)
	rdAssertOK(t, env, err, "order.status")
	var res hl.OrderQueryResult
	if e := json.Unmarshal(env.Data, &res); e != nil {
		t.Fatalf("decode order status: %v", e)
	}
	if res.Order.Order.Oid != 555 {
		t.Fatalf("order status oid wrong: %+v", res)
	}
	if req := srv.requestFor("orderStatus"); req == nil || req["oid"] == nil {
		t.Fatalf("orderStatus request missing oid: %+v", req)
	}

	// Missing both --oid and --cloid is rejected before any network call.
	rOid, rCloid = 0, ""
	env, err = rdRun(t, orderStatusCmd, nil)
	rdAssertFail(t, env, err, "validation", output.ExitValidation)
	if env.Error.Code != "missing_id" {
		t.Fatalf("want missing_id, got %q", env.Error.Code)
	}

	rOid = 555
	srv.fail("orderStatus")
	env, err = rdRun(t, orderStatusCmd, nil)
	rdAssertFail(t, env, err, "network", output.ExitNetwork)
}
