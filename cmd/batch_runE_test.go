package cmd

// RunE coverage for the batch command group (#102): batch, modify-batch, grid.
// Each test swaps the newClient seam for a tiny fake (via the shared harness) so
// the handlers run offline. Fakes capture the reqs the handler builds so we can
// assert that file/stdin JSON and grid flags reach the client unmangled. All
// identifiers are bt-prefixed to stay collision-free when the cmd test files are
// compiled together.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
)

// btSetFile points the shared --file global (bFile, used by both batch and
// modify-batch) at a temp file holding body, restoring it after the test.
func btSetFile(t *testing.T, body string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "batch.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp batch file: %v", err)
	}
	prev := bFile
	bFile = p
	t.Cleanup(func() { bFile = prev })
}

// btSetStdin replaces os.Stdin with a temp file holding body (so readBatchInput's
// stdin branch reads our fixture), restoring the real stdin and bFile after.
func btSetStdin(t *testing.T, body string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stdin.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp stdin file: %v", err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open temp stdin file: %v", err)
	}
	prevStdin, prevFile := os.Stdin, bFile
	os.Stdin = f
	bFile = "" // force readBatchInput onto the stdin branch
	t.Cleanup(func() {
		os.Stdin = prevStdin
		bFile = prevFile
		_ = f.Close()
	})
}

// --- batch ---------------------------------------------------------------

// btBatchClient captures the OrderReqs the handler hands PlaceBatch and returns a
// canned result per leg, so a test can assert both the parsed inputs and the
// emitted envelope.
type btBatchClient struct {
	core.ClientAPI
	gotReqs []core.OrderReq
	results []*core.PlaceResult
}

func (c *btBatchClient) PlaceBatch(_ context.Context, reqs []core.OrderReq) ([]*core.PlaceResult, []string, error) {
	c.gotReqs = reqs
	return c.results, []string{"builder fee 0.050% applied"}, nil
}

func TestBatchCmdHappy(t *testing.T) {
	btSetFile(t, `[
		{"coin":"BTC","side":"buy","size":"0.001","limit":"60000"},
		{"coin":"ETH","side":"sell","size":0.5,"limit":3000}
	]`)
	fake := &btBatchClient{results: []*core.PlaceResult{
		{Coin: "BTC", Side: "buy", Status: "resting", Size: "0.001"},
		{Coin: "ETH", Side: "sell", Status: "resting", Size: "0.5"},
	}}
	withFakeClient(t, fake)

	env, err := runCmd(t, batchCmd, nil)
	if err != nil {
		t.Fatalf("batch happy should be exit 0, got %v", err)
	}
	if !env.OK || env.Cmd != "batch" {
		t.Fatalf("batch envelope wrong: %+v", env)
	}
	// The two parsed legs must reach PlaceBatch with side/size/limit intact.
	if len(fake.gotReqs) != 2 {
		t.Fatalf("PlaceBatch got %d reqs, want 2", len(fake.gotReqs))
	}
	if fake.gotReqs[0].Coin != "BTC" || fake.gotReqs[0].Side != core.Buy || fake.gotReqs[0].Size != "0.001" || fake.gotReqs[0].Limit != "60000" {
		t.Fatalf("leg 0 mangled: %+v", fake.gotReqs[0])
	}
	// flexStr must preserve the bare-number literals "0.5"/"3000" (no float round-trip).
	if fake.gotReqs[1].Coin != "ETH" || fake.gotReqs[1].Side != core.Sell || fake.gotReqs[1].Size != "0.5" || fake.gotReqs[1].Limit != "3000" {
		t.Fatalf("leg 1 mangled: %+v", fake.gotReqs[1])
	}
	// Data carries both legs.
	var legs []core.PlaceResult
	if e := json.Unmarshal(env.Data, &legs); e != nil {
		t.Fatalf("data not a result array: %v (%s)", e, env.Data)
	}
	if len(legs) != 2 || legs[0].Coin != "BTC" || legs[1].Coin != "ETH" {
		t.Fatalf("data legs wrong: %+v", legs)
	}
}

func TestBatchCmdStdin(t *testing.T) {
	btSetStdin(t, `[{"coin":"SOL","side":"buy","size":"1","limit":"150"}]`)
	fake := &btBatchClient{results: []*core.PlaceResult{
		{Coin: "SOL", Side: "buy", Status: "resting", Size: "1"},
	}}
	withFakeClient(t, fake)

	env, err := runCmd(t, batchCmd, nil)
	if err != nil {
		t.Fatalf("batch stdin should be exit 0, got %v", err)
	}
	if !env.OK || env.Cmd != "batch" {
		t.Fatalf("batch stdin envelope wrong: %+v", env)
	}
	if len(fake.gotReqs) != 1 || fake.gotReqs[0].Coin != "SOL" {
		t.Fatalf("stdin leg did not reach PlaceBatch: %+v", fake.gotReqs)
	}
}

// A partially-filled leg must still emit the success envelope but return exit 60,
// matching the single-order contract (batchExit).
func TestBatchCmdPartialExit(t *testing.T) {
	btSetFile(t, `[{"coin":"BTC","side":"buy","size":"0.1","limit":"60000","tif":"Ioc"}]`)
	withFakeClient(t, &btBatchClient{results: []*core.PlaceResult{
		{Coin: "BTC", Side: "buy", Status: "filled", Size: "0.1", FilledSz: "0.04"},
	}})

	env, err := runCmd(t, batchCmd, nil)
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitPartial {
		t.Fatalf("partial fill must return exit %d, got %T %v", output.ExitPartial, err, err)
	}
	if !env.OK || env.Cmd != "batch" {
		t.Fatalf("partial fill must still emit the success envelope: %+v", env)
	}
}

// Every leg rejected => exchange (exit 50), success envelope still emitted.
func TestBatchCmdAllRejectedExit(t *testing.T) {
	btSetFile(t, `[{"coin":"BTC","side":"buy","size":"0.1","limit":"60000"}]`)
	withFakeClient(t, &btBatchClient{results: []*core.PlaceResult{
		{Coin: "BTC", Side: "buy", Status: "rejected", Error: "margin"},
	}})

	env, err := runCmd(t, batchCmd, nil)
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitExchange {
		t.Fatalf("all-rejected batch must return exit %d, got %T %v", output.ExitExchange, err, err)
	}
	if !env.OK {
		t.Fatalf("all-rejected batch still emits the success envelope: %+v", env)
	}
}

// btBatchErrClient makes PlaceBatch fail (network) — the categorized failure path.
type btBatchErrClient struct{ core.ClientAPI }

func (btBatchErrClient) PlaceBatch(context.Context, []core.OrderReq) ([]*core.PlaceResult, []string, error) {
	return nil, nil, output.Network("net_down", "exchange unreachable")
}

func TestBatchCmdMethodError(t *testing.T) {
	btSetFile(t, `[{"coin":"BTC","side":"buy","size":"0.1","limit":"60000"}]`)
	withFakeClient(t, btBatchErrClient{})

	env, err := runCmd(t, batchCmd, nil)
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitNetwork {
		t.Fatalf("PlaceBatch error must surface as exit %d, got %T %v", output.ExitNetwork, err, err)
	}
	if env.OK || env.Error.Category != "network" || env.Error.Code != "net_down" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

// Malformed JSON is rejected locally (validation, exit 10) before any client call —
// so the fake's PlaceBatch must never run (it would panic if it did).
func TestBatchCmdBadJSON(t *testing.T) {
	btSetFile(t, `{ not an array `)
	withFakeClient(t, btBatchErrClient{})

	env, err := runCmd(t, batchCmd, nil)
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitValidation {
		t.Fatalf("bad JSON must be exit %d, got %T %v", output.ExitValidation, err, err)
	}
	if env.OK || env.Error.Category != "validation" || env.Error.Code != "bad_json" {
		t.Fatalf("bad-json envelope wrong: %+v", env)
	}
}

// A leg with an invalid side fails in toOrderReq (validation) before signing.
func TestBatchCmdBadSide(t *testing.T) {
	btSetFile(t, `[{"coin":"BTC","side":"nope","size":"0.1","limit":"60000"}]`)
	withFakeClient(t, btBatchErrClient{})

	env, err := runCmd(t, batchCmd, nil)
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitValidation {
		t.Fatalf("bad side must be exit %d, got %T %v", output.ExitValidation, err, err)
	}
	if env.OK || env.Error.Category != "validation" || env.Error.Code != "bad_order" {
		t.Fatalf("bad-order envelope wrong: %+v", env)
	}
}

// --- modify-batch --------------------------------------------------------

// btModifyClient captures the ModifyReqs and returns a canned result per leg.
type btModifyClient struct {
	core.ClientAPI
	gotReqs []core.ModifyReq
	results []*core.PlaceResult
}

func (c *btModifyClient) ModifyBatch(_ context.Context, reqs []core.ModifyReq) ([]*core.PlaceResult, []string, error) {
	c.gotReqs = reqs
	return c.results, nil, nil
}

func TestModifyBatchCmdHappy(t *testing.T) {
	btSetFile(t, `[
		{"oid":123,"limit":"60500"},
		{"cloid":"0x00000000000000000000000000000001","size":"0.2"}
	]`)
	fake := &btModifyClient{results: []*core.PlaceResult{
		{Coin: "BTC", Status: "resting", Oid: ptrI64(123)},
		{Coin: "BTC", Status: "resting"},
	}}
	withFakeClient(t, fake)

	env, err := runCmd(t, modifyBatchCmd, nil)
	if err != nil {
		t.Fatalf("modify-batch happy should be exit 0, got %v", err)
	}
	if !env.OK || env.Cmd != "modify-batch" {
		t.Fatalf("modify-batch envelope wrong: %+v", env)
	}
	if len(fake.gotReqs) != 2 {
		t.Fatalf("ModifyBatch got %d reqs, want 2", len(fake.gotReqs))
	}
	if fake.gotReqs[0].Oid == nil || *fake.gotReqs[0].Oid != 123 || fake.gotReqs[0].Limit != "60500" {
		t.Fatalf("modify leg 0 mangled: %+v", fake.gotReqs[0])
	}
	if fake.gotReqs[1].Cloid != "0x00000000000000000000000000000001" || fake.gotReqs[1].Size != "0.2" {
		t.Fatalf("modify leg 1 mangled: %+v", fake.gotReqs[1])
	}
	var legs []core.PlaceResult
	if e := json.Unmarshal(env.Data, &legs); e != nil || len(legs) != 2 {
		t.Fatalf("data legs wrong: %v (%s)", e, env.Data)
	}
}

// btModifyErrClient makes ModifyBatch fail (risk) — categorized failure path.
type btModifyErrClient struct{ core.ClientAPI }

func (btModifyErrClient) ModifyBatch(context.Context, []core.ModifyReq) ([]*core.PlaceResult, []string, error) {
	return nil, nil, output.Risk("rate_cap", "modify rate cap exceeded")
}

func TestModifyBatchCmdMethodError(t *testing.T) {
	btSetFile(t, `[{"oid":123,"limit":"60500"}]`)
	withFakeClient(t, btModifyErrClient{})

	env, err := runCmd(t, modifyBatchCmd, nil)
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitRisk {
		t.Fatalf("ModifyBatch error must surface as exit %d, got %T %v", output.ExitRisk, err, err)
	}
	if env.OK || env.Error.Category != "risk" || env.Error.Code != "rate_cap" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

func TestModifyBatchCmdBadJSON(t *testing.T) {
	btSetFile(t, `{bad`)
	withFakeClient(t, btModifyErrClient{})

	env, err := runCmd(t, modifyBatchCmd, nil)
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitValidation {
		t.Fatalf("bad JSON must be exit %d, got %T %v", output.ExitValidation, err, err)
	}
	if env.OK || env.Error.Code != "bad_json" {
		t.Fatalf("bad-json envelope wrong: %+v", env)
	}
}

// --- grid ----------------------------------------------------------------

// btSetGridFlags sets the grid flag globals to known values and restores them.
func btSetGridFlags(t *testing.T, levels int, from, to, size, tif string, reduceOnly bool) {
	t.Helper()
	pl, pf, pt, ps, ptif, pro := gLevels, gFrom, gTo, gSize, gTif, gReduceOnly
	gLevels, gFrom, gTo, gSize, gTif, gReduceOnly = levels, from, to, size, tif, reduceOnly
	t.Cleanup(func() {
		gLevels, gFrom, gTo, gSize, gTif, gReduceOnly = pl, pf, pt, ps, ptif, pro
	})
}

// btGridClient captures both the GridReq passed to BuildGrid and the OrderReqs it
// returned (which the handler forwards to PlaceBatch), so a test can assert the
// flags flowed through and the built ladder is what gets placed.
type btGridClient struct {
	core.ClientAPI
	gotGrid  core.GridReq
	built    []core.OrderReq
	gotPlace []core.OrderReq
	results  []*core.PlaceResult
}

func (c *btGridClient) BuildGrid(req core.GridReq) ([]core.OrderReq, error) {
	c.gotGrid = req
	return c.built, nil
}

func (c *btGridClient) PlaceBatch(_ context.Context, reqs []core.OrderReq) ([]*core.PlaceResult, []string, error) {
	c.gotPlace = reqs
	return c.results, nil, nil
}

func TestGridCmdHappy(t *testing.T) {
	btSetGridFlags(t, 3, "60000", "62000", "0.3", "Gtc", true)
	built := []core.OrderReq{
		{Coin: "BTC", Side: core.Buy, Size: "0.1", Limit: "60000"},
		{Coin: "BTC", Side: core.Buy, Size: "0.1", Limit: "61000"},
		{Coin: "BTC", Side: core.Buy, Size: "0.1", Limit: "62000"},
	}
	fake := &btGridClient{
		built: built,
		results: []*core.PlaceResult{
			{Coin: "BTC", Side: "buy", Status: "resting", Size: "0.1"},
			{Coin: "BTC", Side: "buy", Status: "resting", Size: "0.1"},
			{Coin: "BTC", Side: "buy", Status: "resting", Size: "0.1"},
		},
	}
	withFakeClient(t, fake)

	env, err := runCmd(t, gridCmd, []string{"BTC", "buy"})
	if err != nil {
		t.Fatalf("grid happy should be exit 0, got %v", err)
	}
	if !env.OK || env.Cmd != "grid" {
		t.Fatalf("grid envelope wrong: %+v", env)
	}
	// The positional coin/side and every grid flag must reach BuildGrid.
	g := fake.gotGrid
	if g.Coin != "BTC" || g.Side != core.Buy || g.Levels != 3 ||
		g.FromPx != "60000" || g.ToPx != "62000" || g.TotalSize != "0.3" ||
		g.Tif != "Gtc" || !g.ReduceOnly {
		t.Fatalf("GridReq did not carry args/flags: %+v", g)
	}
	// The ladder BuildGrid returned is exactly what PlaceBatch receives.
	if len(fake.gotPlace) != 3 || fake.gotPlace[1].Limit != "61000" {
		t.Fatalf("built ladder not forwarded to PlaceBatch: %+v", fake.gotPlace)
	}
	var legs []core.PlaceResult
	if e := json.Unmarshal(env.Data, &legs); e != nil || len(legs) != 3 {
		t.Fatalf("data legs wrong: %v (%s)", e, env.Data)
	}
}

// An invalid side fails in parseSide (validation) before any client call — so
// neither BuildGrid nor PlaceBatch runs (they would panic on the bare embed).
func TestGridCmdBadSide(t *testing.T) {
	btSetGridFlags(t, 3, "60000", "62000", "0.3", "Gtc", false)
	withFakeClient(t, &btGridClient{})

	env, err := runCmd(t, gridCmd, []string{"BTC", "nope"})
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitValidation {
		t.Fatalf("bad side must be exit %d, got %T %v", output.ExitValidation, err, err)
	}
	if env.OK || env.Error.Category != "validation" || env.Error.Code != "bad_side" {
		t.Fatalf("bad-side envelope wrong: %+v", env)
	}
}

// btGridBuildErrClient makes BuildGrid fail (validation) — exercises the grid
// handler's BuildGrid error branch without reaching PlaceBatch.
type btGridBuildErrClient struct{ core.ClientAPI }

func (btGridBuildErrClient) BuildGrid(core.GridReq) ([]core.OrderReq, error) {
	return nil, output.Validation("bad_levels", "grid needs --levels >= 1")
}

func TestGridCmdBuildError(t *testing.T) {
	btSetGridFlags(t, 0, "60000", "62000", "0.3", "Gtc", false)
	withFakeClient(t, btGridBuildErrClient{})

	env, err := runCmd(t, gridCmd, []string{"BTC", "buy"})
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitValidation {
		t.Fatalf("BuildGrid error must be exit %d, got %T %v", output.ExitValidation, err, err)
	}
	if env.OK || env.Error.Category != "validation" || env.Error.Code != "bad_levels" {
		t.Fatalf("build-error envelope wrong: %+v", env)
	}
}

// btGridPlaceErrClient builds a one-leg grid then fails at PlaceBatch (exchange) —
// exercises the grid handler's PlaceBatch error branch.
type btGridPlaceErrClient struct{ core.ClientAPI }

func (btGridPlaceErrClient) BuildGrid(core.GridReq) ([]core.OrderReq, error) {
	return []core.OrderReq{{Coin: "BTC", Side: core.Buy, Size: "0.1", Limit: "60000"}}, nil
}

func (btGridPlaceErrClient) PlaceBatch(context.Context, []core.OrderReq) ([]*core.PlaceResult, []string, error) {
	return nil, nil, output.Exchange("oracle", "oracle price unavailable")
}

func TestGridCmdPlaceError(t *testing.T) {
	btSetGridFlags(t, 1, "60000", "60000", "0.1", "Gtc", false)
	withFakeClient(t, btGridPlaceErrClient{})

	env, err := runCmd(t, gridCmd, []string{"BTC", "buy"})
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitExchange {
		t.Fatalf("grid PlaceBatch error must be exit %d, got %T %v", output.ExitExchange, err, err)
	}
	if env.OK || env.Error.Category != "exchange" || env.Error.Code != "oracle" {
		t.Fatalf("place-error envelope wrong: %+v", env)
	}
}
