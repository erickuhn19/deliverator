package cmd

// RunE coverage for the read-grab-bag command group (#102): info, reconcile,
// preview, leaderboard, and connect. Each test swaps the newClient seam for a
// tiny fake that overrides only the methods its handler calls, so the handlers
// run fully offline (no network, no keychain). Identifiers are prefixed `iq` to
// avoid collisions when every coverage file is compiled together.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
)

// iqUnmarshalData decodes the envelope's data payload into v (it is RawMessage),
// failing the test on a decode error.
func iqUnmarshalData(t *testing.T, env envelope, v any) {
	t.Helper()
	if err := json.Unmarshal(env.Data, v); err != nil {
		t.Fatalf("decode data %s: %v", env.Data, err)
	}
}

// --- info: raw /info passthrough ---

// iqInfoClient records the body it received so the test can assert that
// buildInfoBody's parsed params reach RawInfo verbatim.
type iqInfoClient struct {
	core.ClientAPI
	gotBody map[string]any
}

func (c *iqInfoClient) RawInfo(_ context.Context, body map[string]any) (any, error) {
	c.gotBody = body
	return map[string]any{"echo": body["type"]}, nil
}

func TestInfoCmdHappy(t *testing.T) {
	fc := &iqInfoClient{}
	withFakeClient(t, fc)
	env, err := runCmd(t, infoCmd, []string{"fundingHistory", "coin=BTC", "startTime=1750000000000"})
	if err != nil {
		t.Fatalf("info should succeed, got %v", err)
	}
	if !env.OK || env.Cmd != "info" {
		t.Fatalf("envelope wrong: %+v", env)
	}
	// The parsed params must reach the client: a string stays a string, an integer
	// becomes a number.
	if fc.gotBody["type"] != "fundingHistory" || fc.gotBody["coin"] != "BTC" {
		t.Fatalf("body not threaded to RawInfo: %#v", fc.gotBody)
	}
	if n, ok := fc.gotBody["startTime"].(int64); !ok || n != 1750000000000 {
		t.Fatalf("integer param must be sent as a number, got %#v", fc.gotBody["startTime"])
	}
}

// iqInfoAtClient resolves the "@" sentinel via RequireQueryAddr + QueryAddr.
type iqInfoAtClient struct {
	core.ClientAPI
	requireCalled bool
	gotBody       map[string]any
}

func (c *iqInfoAtClient) RequireQueryAddr() error { c.requireCalled = true; return nil }
func (c *iqInfoAtClient) QueryAddr() string       { return "0xQUERY" }
func (c *iqInfoAtClient) RawInfo(_ context.Context, body map[string]any) (any, error) {
	c.gotBody = body
	return map[string]any{"ok": true}, nil
}

// "@" must expand to the configured query address before the call.
func TestInfoCmdAtExpandsToQueryAddr(t *testing.T) {
	fc := &iqInfoAtClient{}
	withFakeClient(t, fc)
	env, err := runCmd(t, infoCmd, []string{"clearinghouseState", "user=@"})
	if err != nil {
		t.Fatalf("info with @ should succeed, got %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope wrong: %+v", env)
	}
	if !fc.requireCalled {
		t.Fatal("@ must trigger RequireQueryAddr before sending user= to the exchange")
	}
	if fc.gotBody["user"] != "0xQUERY" {
		t.Fatalf("@ must expand to the query address, got %#v", fc.gotBody["user"])
	}
}

// iqInfoNoAddrClient has no query address: RequireQueryAddr fails with an auth
// error, which the handler must surface as exit 30 WITHOUT sending anything.
type iqInfoNoAddrClient struct{ core.ClientAPI }

func (iqInfoNoAddrClient) RequireQueryAddr() error {
	return output.Auth("no_query_addr", "set wallet.master_address")
}

func TestInfoCmdAtNoQueryAddrAuthError(t *testing.T) {
	withFakeClient(t, iqInfoNoAddrClient{})
	env, err := runCmd(t, infoCmd, []string{"openOrders", "user=@"})
	ce, ok := err.(*output.CmdError)
	if !ok || ce.Code != output.ExitAuth {
		t.Fatalf("want auth CmdError (exit %d), got %T %v", output.ExitAuth, err, err)
	}
	if env.OK || env.Error.Category != "auth" || env.Error.Code != "no_query_addr" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

// A bad key=value param is rejected before any client is built (exit 10).
func TestInfoCmdBadParamValidation(t *testing.T) {
	withFakeClient(t, &iqInfoClient{}) // RawInfo must never be reached
	env, err := runCmd(t, infoCmd, []string{"openOrders", "noequals"})
	ce, ok := err.(*output.CmdError)
	if !ok || ce.Code != output.ExitValidation {
		t.Fatalf("want validation CmdError, got %T %v", err, err)
	}
	if env.OK || env.Error.Code != "bad_param" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

// iqInfoErrClient errors from RawInfo → network failure envelope (exit 40).
type iqInfoErrClient struct{ core.ClientAPI }

func (iqInfoErrClient) RawInfo(context.Context, map[string]any) (any, error) {
	return nil, output.Network("info_unreachable", "endpoint down")
}

func TestInfoCmdMethodError(t *testing.T) {
	withFakeClient(t, iqInfoErrClient{})
	env, err := runCmd(t, infoCmd, []string{"meta"})
	ce, ok := err.(*output.CmdError)
	if !ok || ce.Code != output.ExitNetwork {
		t.Fatalf("want network CmdError, got %T %v", err, err)
	}
	if env.OK || env.Error.Category != "network" || env.Error.Code != "info_unreachable" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

// --- reconcile: diff local audit vs live (exit 60 on divergence) ---

// iqReconcileClient records the opts and returns a canned view.
type iqReconcileClient struct {
	core.ClientAPI
	view    *core.ReconcileView
	gotOpts core.ReconcileOpts
}

func (c *iqReconcileClient) Reconcile(_ context.Context, opts core.ReconcileOpts) (*core.ReconcileView, error) {
	c.gotOpts = opts
	return c.view, nil
}

// A clean reconcile emits the success envelope and exits 0.
func TestReconcileCmdClean(t *testing.T) {
	saveSince, saveCloids := rcSince, rcCloids
	t.Cleanup(func() { rcSince, rcCloids = saveSince, saveCloids })
	rcSince, rcCloids = 1700000000000, nil

	fc := &iqReconcileClient{view: &core.ReconcileView{Address: "0xabc", Clean: true}}
	withFakeClient(t, fc)
	env, err := runCmd(t, reconcileCmd, nil)
	if err != nil {
		t.Fatalf("clean reconcile should be exit 0, got %v", err)
	}
	if !env.OK || env.Cmd != "reconcile" {
		t.Fatalf("envelope wrong: %+v", env)
	}
	// The --since flag global must reach the opts.
	if fc.gotOpts.SinceMs != 1700000000000 {
		t.Fatalf("--since not threaded, got %d", fc.gotOpts.SinceMs)
	}
}

// A divergence (Clean=false) still emits the success envelope but returns
// exit 60 (ExitPartial) so the agent inspects before resuming. The --cloid
// flag (comma-separated) must be split into suspect cloids.
func TestReconcileCmdDivergenceExitPartial(t *testing.T) {
	saveSince, saveCloids := rcSince, rcCloids
	t.Cleanup(func() { rcSince, rcCloids = saveSince, saveCloids })
	rcSince, rcCloids = 0, []string{"0x01, 0x02", "0x03"}

	fc := &iqReconcileClient{view: &core.ReconcileView{Clean: false, Divergences: []string{"orphan"}}}
	withFakeClient(t, fc)
	env, err := runCmd(t, reconcileCmd, nil)
	ce, ok := err.(*output.CmdError)
	if !ok || ce.Code != output.ExitPartial {
		t.Fatalf("divergence must return exit %d, got %T %v", output.ExitPartial, err, err)
	}
	if !env.OK {
		t.Fatalf("divergence must still emit the success envelope, got %+v", env)
	}
	want := []string{"0x01", "0x02", "0x03"}
	if len(fc.gotOpts.SuspectCloids) != len(want) {
		t.Fatalf("cloids not split, got %#v", fc.gotOpts.SuspectCloids)
	}
	for i, c := range want {
		if fc.gotOpts.SuspectCloids[i] != c {
			t.Fatalf("cloid[%d] = %q, want %q", i, fc.gotOpts.SuspectCloids[i], c)
		}
	}
}

// iqReconcileErrClient errors from Reconcile → network failure envelope.
type iqReconcileErrClient struct{ core.ClientAPI }

func (iqReconcileErrClient) Reconcile(context.Context, core.ReconcileOpts) (*core.ReconcileView, error) {
	return nil, output.Network("recon_unreachable", "audit read failed")
}

func TestReconcileCmdMethodError(t *testing.T) {
	saveSince, saveCloids := rcSince, rcCloids
	t.Cleanup(func() { rcSince, rcCloids = saveSince, saveCloids })
	rcSince, rcCloids = 0, nil

	withFakeClient(t, iqReconcileErrClient{})
	env, err := runCmd(t, reconcileCmd, nil)
	ce, ok := err.(*output.CmdError)
	if !ok || ce.Code != output.ExitNetwork {
		t.Fatalf("want network CmdError, got %T %v", err, err)
	}
	if env.OK || env.Error.Category != "network" || env.Error.Code != "recon_unreachable" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

// A build-client failure surfaces as a failure envelope (auth, exit 30).
func TestReconcileCmdClientBuildError(t *testing.T) {
	withClientErr(t, output.Auth("no_agent_key", "run onboard"))
	env, err := runCmd(t, reconcileCmd, nil)
	ce, ok := err.(*output.CmdError)
	if !ok || ce.Code != output.ExitAuth {
		t.Fatalf("want auth CmdError, got %T %v", err, err)
	}
	if env.OK || env.Error.Code != "no_agent_key" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

// --- preview: no-sign what-if ---

// iqPreviewClient records the args it received so the test can assert that the
// positional coin/side/size and the --limit/--leverage flags reach Preview.
type iqPreviewClient struct {
	core.ClientAPI
	res     *core.PreviewResult
	gotCoin string
	gotSide core.Side
	gotSize string
	gotLim  string
	gotLev  int
}

func (c *iqPreviewClient) Preview(_ context.Context, coin string, side core.Side, size, limit string, leverage int) (*core.PreviewResult, error) {
	c.gotCoin, c.gotSide, c.gotSize, c.gotLim, c.gotLev = coin, side, size, limit, leverage
	return c.res, nil
}

func TestPreviewCmdHappy(t *testing.T) {
	saveLim, saveLev := pvLimit, pvLeverage
	t.Cleanup(func() { pvLimit, pvLeverage = saveLim, saveLev })
	pvLimit, pvLeverage = "65000", 5

	fc := &iqPreviewClient{res: &core.PreviewResult{Coin: "BTC", Side: "buy", Size: "0.1", EntryPx: "65000"}}
	withFakeClient(t, fc)
	env, err := runCmd(t, previewCmd, []string{"BTC", "buy", "0.1"})
	if err != nil {
		t.Fatalf("preview should succeed, got %v", err)
	}
	if !env.OK || env.Cmd != "preview" {
		t.Fatalf("envelope wrong: %+v", env)
	}
	if fc.gotCoin != "BTC" || fc.gotSide != core.Buy || fc.gotSize != "0.1" {
		t.Fatalf("positional args not threaded: %+v", fc)
	}
	if fc.gotLim != "65000" || fc.gotLev != 5 {
		t.Fatalf("--limit/--leverage flags not threaded: lim=%q lev=%d", fc.gotLim, fc.gotLev)
	}
}

// A bad side is rejected before any client is built (exit 10).
func TestPreviewCmdBadSideValidation(t *testing.T) {
	withFakeClient(t, &iqPreviewClient{}) // Preview must never be reached
	env, err := runCmd(t, previewCmd, []string{"BTC", "sideways", "0.1"})
	ce, ok := err.(*output.CmdError)
	if !ok || ce.Code != output.ExitValidation {
		t.Fatalf("want validation CmdError, got %T %v", err, err)
	}
	if env.OK || env.Error.Category != "validation" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

// iqPreviewErrClient errors from Preview → exchange failure envelope (exit 50).
type iqPreviewErrClient struct{ core.ClientAPI }

func (iqPreviewErrClient) Preview(context.Context, string, core.Side, string, string, int) (*core.PreviewResult, error) {
	return nil, output.Exchange("no_oracle", "no mark price for coin")
}

func TestPreviewCmdMethodError(t *testing.T) {
	saveLim, saveLev := pvLimit, pvLeverage
	t.Cleanup(func() { pvLimit, pvLeverage = saveLim, saveLev })
	pvLimit, pvLeverage = "", 0

	withFakeClient(t, iqPreviewErrClient{})
	env, err := runCmd(t, previewCmd, []string{"BTC", "sell", "1"})
	ce, ok := err.(*output.CmdError)
	if !ok || ce.Code != output.ExitExchange {
		t.Fatalf("want exchange CmdError (exit %d), got %T %v", output.ExitExchange, err, err)
	}
	if env.OK || env.Error.Category != "exchange" || env.Error.Code != "no_oracle" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

// --- leaderboard: filter/sort/paginate ---

// iqLeaderboardClient records the params so the test can assert that the flag
// globals are assembled into LeaderboardParams.
type iqLeaderboardClient struct {
	core.ClientAPI
	view *core.LeaderboardView
	gotP core.LeaderboardParams
}

func (c *iqLeaderboardClient) Leaderboard(_ context.Context, p core.LeaderboardParams) (*core.LeaderboardView, error) {
	c.gotP = p
	return c.view, nil
}

func TestLeaderboardCmdHappy(t *testing.T) {
	saveWin, saveSort, saveLimit := lbWindow, lbSort, lbLimit
	t.Cleanup(func() { lbWindow, lbSort, lbLimit = saveWin, saveSort, saveLimit })
	lbWindow, lbSort, lbLimit = "week", "roi", 10

	fc := &iqLeaderboardClient{view: &core.LeaderboardView{Network: "testnet", SortWindow: "week", SortBy: "roi", Returned: 1}}
	withFakeClient(t, fc)
	env, err := runCmd(t, leaderboardCmd, nil)
	if err != nil {
		t.Fatalf("leaderboard should succeed, got %v", err)
	}
	if !env.OK || env.Cmd != "leaderboard" {
		t.Fatalf("envelope wrong: %+v", env)
	}
	// The flag globals must reach the params struct.
	if fc.gotP.Window != "week" || fc.gotP.SortBy != "roi" || fc.gotP.Limit != 10 {
		t.Fatalf("filters not threaded into params: %+v", fc.gotP)
	}
}

// iqLeaderboardErrClient errors from Leaderboard → network failure envelope.
type iqLeaderboardErrClient struct{ core.ClientAPI }

func (iqLeaderboardErrClient) Leaderboard(context.Context, core.LeaderboardParams) (*core.LeaderboardView, error) {
	return nil, output.Network("lb_unreachable", "stats-data unreachable")
}

func TestLeaderboardCmdMethodError(t *testing.T) {
	withFakeClient(t, iqLeaderboardErrClient{})
	env, err := runCmd(t, leaderboardCmd, nil)
	ce, ok := err.(*output.CmdError)
	if !ok || ce.Code != output.ExitNetwork {
		t.Fatalf("want network CmdError, got %T %v", err, err)
	}
	if env.OK || env.Error.Category != "network" || env.Error.Code != "lb_unreachable" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

// --- connect: preflight aggregation ---

// iqConnectClient stubs the read surface connect aggregates: a fresh MetaStore
// (Age ~0, no markets), a query address, a not-halted box, and a small skew.
type iqConnectClient struct {
	core.ClientAPI
	skewCalled bool
}

func (iqConnectClient) QueryAddr() string { return "0xMASTER" }
func (iqConnectClient) Halted() bool      { return false }
func (iqConnectClient) Meta() *core.MetaStore {
	return core.NewMetaStore("testnet", nil, nil, time.Now())
}

func (c *iqConnectClient) MeasureSkew(context.Context) (int64, error) {
	c.skewCalled = true
	return 42, nil
}

func TestConnectCmdHappy(t *testing.T) {
	// Route key load through the env override so the test never touches the OS
	// keychain (0x...01 is a valid secp256k1 key). agent_key then reports present.
	t.Setenv("DELIVERATOR_AGENT_KEY", "0000000000000000000000000000000000000000000000000000000000000001")

	fc := &iqConnectClient{}
	withFakeClient(t, fc)
	env, err := runCmd(t, connectCmd, nil)
	if err != nil {
		t.Fatalf("connect happy path should be exit 0, got %v", err)
	}
	if !env.OK || env.Cmd != "connect" {
		t.Fatalf("envelope wrong: %+v", env)
	}
	if !fc.skewCalled {
		t.Fatal("connect must measure clock skew")
	}
	var data struct {
		APIReachable bool   `json:"api_reachable"`
		QueryAddress string `json:"query_address"`
		Halted       bool   `json:"halted"`
		ClockSkewMs  int64  `json:"clock_skew_ms"`
		AgentKey     string `json:"agent_key"`
	}
	iqUnmarshalData(t, env, &data)
	if !data.APIReachable || data.QueryAddress != "0xMASTER" || data.Halted {
		t.Fatalf("preflight data wrong: %+v", data)
	}
	if data.ClockSkewMs != 42 || data.AgentKey != "present" {
		t.Fatalf("skew/agent-key not aggregated: %+v", data)
	}
}

// A build-client failure marks the box unreachable and returns exit 40 (network),
// but still emits the OK-shaped preflight envelope (api_reachable=false).
func TestConnectCmdAPIUnreachable(t *testing.T) {
	t.Setenv("DELIVERATOR_AGENT_KEY", "0000000000000000000000000000000000000000000000000000000000000001")
	withClientErr(t, output.Network("dial_failed", "connection refused"))
	env, err := runCmd(t, connectCmd, nil)
	ce, ok := err.(*output.CmdError)
	if !ok || ce.Code != output.ExitNetwork {
		t.Fatalf("unreachable API must return exit %d, got %T %v", output.ExitNetwork, err, err)
	}
	if !env.OK {
		t.Fatalf("connect must still emit a preflight envelope, got %+v", env)
	}
	var data struct {
		APIReachable bool `json:"api_reachable"`
	}
	iqUnmarshalData(t, env, &data)
	if data.APIReachable {
		t.Fatal("api_reachable must be false when newClient fails")
	}
}
