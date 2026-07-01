package cmd

// RunE coverage for the write commands in writes.go (close, cancel, modify,
// leverage, margin, twap[+status/cancel], position-tpsl, panic) plus sell/order
// (which share runTrade with buy) and snapshot (runReadWarn). Uses the shared
// harness (withFakeClient/runCmd). All identifiers are prefixed `wr`; the
// configurable wrFake embeds core.ClientAPI so only the methods a handler calls
// are stubbed. No t.Parallel (the seam + flag globals are process-wide).

import (
	"context"
	"os"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
)

type wrFake struct {
	core.ClientAPI
	closeFn    func(coin, size string, market bool, limit, cloid string) (*core.PlaceResult, []string, error)
	cancelFn   func(core.CancelReq) (*core.CancelResult, error)
	modifyFn   func(oid *int64, cloid, sz, lim string) (*core.PlaceResult, []string, error)
	levFn      func(coin string, x int, cross bool) (*core.LeverageResult, error)
	marginFn   func(coin string, usd float64) (*core.MarginResult, error)
	twapFn     func(core.TwapReq) (*core.TwapResult, []string, error)
	twapStatFn func(coin string, id int64) (*core.TwapStatusView, error)
	twapCxlFn  func(coin string, id int64) (*core.TwapCancelResult, error)
	tpslFn     func(core.PositionTpslReq) ([]*core.PlaceResult, []string, error)
	panicFn    func() (*core.PanicResult, error)
	placeFn    func(core.OrderReq) (*core.PlaceResult, []string, error)
	bracketFn  func(core.BracketReq) ([]*core.PlaceResult, []string, error)
	snapFn     func([]string) (*core.SnapshotView, []string, error)
}

func (f wrFake) PlaceBracket(_ context.Context, r core.BracketReq) ([]*core.PlaceResult, []string, error) {
	return f.bracketFn(r)
}

func (f wrFake) Close(_ context.Context, coin, size string, market bool, limit, cloid string) (*core.PlaceResult, []string, error) {
	return f.closeFn(coin, size, market, limit, cloid)
}

func (f wrFake) Cancel(_ context.Context, r core.CancelReq) (*core.CancelResult, error) {
	return f.cancelFn(r)
}

func (f wrFake) Modify(_ context.Context, oid *int64, cloid, sz, lim string) (*core.PlaceResult, []string, error) {
	return f.modifyFn(oid, cloid, sz, lim)
}

func (f wrFake) SetLeverage(_ context.Context, coin string, x int, cross bool) (*core.LeverageResult, error) {
	return f.levFn(coin, x, cross)
}

func (f wrFake) AdjustMargin(_ context.Context, coin string, usd float64) (*core.MarginResult, error) {
	return f.marginFn(coin, usd)
}

func (f wrFake) Twap(_ context.Context, r core.TwapReq) (*core.TwapResult, []string, error) {
	return f.twapFn(r)
}

func (f wrFake) TwapStatus(_ context.Context, coin string, id int64) (*core.TwapStatusView, error) {
	return f.twapStatFn(coin, id)
}

func (f wrFake) TwapCancel(_ context.Context, coin string, id int64) (*core.TwapCancelResult, error) {
	return f.twapCxlFn(coin, id)
}

func (f wrFake) PlacePositionTpsl(_ context.Context, r core.PositionTpslReq) ([]*core.PlaceResult, []string, error) {
	return f.tpslFn(r)
}
func (f wrFake) Panic(_ context.Context) (*core.PanicResult, error) { return f.panicFn() }
func (f wrFake) Place(_ context.Context, r core.OrderReq) (*core.PlaceResult, []string, error) {
	return f.placeFn(r)
}

func (f wrFake) Snapshot(_ context.Context, coins []string) (*core.SnapshotView, []string, error) {
	return f.snapFn(coins)
}

// wrResetGlobals zeroes the write/read flag globals these handlers read (beyond
// the trade flags resetWriteFlags covers) and restores them after.
func wrResetGlobals(t *testing.T) {
	t.Helper()
	resetWriteFlags(t)
	save := struct {
		market, all, isolated, remove, add bool
		size, cloid, coin, coins           string
		oid, twapID                        int64
		oids                               []int64
		cloids                             []string
		minutes                            int
	}{wMarket, wAll, wIsolated, wRemove, wAdd, wSize, wCloid, rCoin, rCoins, wOid, wTwapID, wOids, wCloids, wMinutes}
	wMarket, wAll, wIsolated, wRemove, wAdd = false, false, false, false, false
	wSize, wCloid, rCoin, rCoins = "", "", "", ""
	wOid, wTwapID, wOids, wCloids, wMinutes = 0, 0, nil, nil, 0
	t.Cleanup(func() {
		wMarket, wAll, wIsolated, wRemove, wAdd = save.market, save.all, save.isolated, save.remove, save.add
		wSize, wCloid, rCoin, rCoins = save.size, save.cloid, save.coin, save.coins
		wOid, wTwapID, wOids, wCloids, wMinutes = save.oid, save.twapID, save.oids, save.cloids, save.minutes
	})
}

func TestCloseCmd(t *testing.T) {
	wrResetGlobals(t)
	wMarket = true
	// full close -> exit 0
	withFakeClient(t, wrFake{closeFn: func(coin, _ string, _ bool, _, _ string) (*core.PlaceResult, []string, error) {
		if coin != "BTC" {
			t.Errorf("coin not threaded: %q", coin)
		}
		return &core.PlaceResult{Status: "filled", Size: "1", FilledSz: "1", Coin: "BTC"}, nil, nil
	}})
	if env, err := runCmd(t, closeCmd, []string{"BTC"}); err != nil || !env.OK || env.Cmd != "close" {
		t.Fatalf("close happy: env=%+v err=%v", env, err)
	}
	// partial close -> exit 60
	withFakeClient(t, wrFake{closeFn: func(_, _ string, _ bool, _, _ string) (*core.PlaceResult, []string, error) {
		return &core.PlaceResult{Status: "filled", Size: "1", FilledSz: "0.4"}, nil, nil
	}})
	if _, err := runCmd(t, closeCmd, []string{"BTC"}); err == nil || err.(*output.CmdError).Code != output.ExitPartial {
		t.Fatalf("partial close must be exit 60, got %v", err)
	}
	// method error -> fail envelope
	withFakeClient(t, wrFake{closeFn: func(_, _ string, _ bool, _, _ string) (*core.PlaceResult, []string, error) {
		return nil, nil, output.Exchange("no_position", "nothing to close")
	}})
	if env, err := runCmd(t, closeCmd, []string{"BTC"}); err == nil || env.OK || env.Error.Code != "no_position" {
		t.Fatalf("close error: env=%+v err=%v", env, err)
	}
}

func TestCancelCmd(t *testing.T) {
	wrResetGlobals(t)
	// no target -> validation (no client built)
	if env, err := runCmd(t, cancelCmd, nil); err == nil || env.Error.Code != "missing_target" {
		t.Fatalf("cancel with no target must be validation, got env=%+v err=%v", env, err)
	}
	// by --oid -> threaded into CancelReq, happy
	wrResetGlobals(t)
	wOid = 7
	withFakeClient(t, wrFake{cancelFn: func(r core.CancelReq) (*core.CancelResult, error) {
		if r.Oid == nil || *r.Oid != 7 {
			t.Errorf("oid not threaded: %+v", r)
		}
		return &core.CancelResult{Canceled: 1}, nil
	}})
	if env, err := runCmd(t, cancelCmd, nil); err != nil || !env.OK {
		t.Fatalf("cancel by oid: env=%+v err=%v", env, err)
	}
}

func TestModifyCmd(t *testing.T) {
	wrResetGlobals(t)
	wOid, wSize, wLimit = 7, "0.2", "65000"
	withFakeClient(t, wrFake{modifyFn: func(oid *int64, _, sz, lim string) (*core.PlaceResult, []string, error) {
		if oid == nil || *oid != 7 || sz != "0.2" || lim != "65000" {
			t.Errorf("modify args not threaded: oid=%v sz=%q lim=%q", oid, sz, lim)
		}
		return &core.PlaceResult{Status: "resting"}, nil, nil
	}})
	if env, err := runCmd(t, modifyCmd, nil); err != nil || !env.OK {
		t.Fatalf("modify: env=%+v err=%v", env, err)
	}
}

func TestLeverageCmd(t *testing.T) {
	wrResetGlobals(t)
	// happy
	withFakeClient(t, wrFake{levFn: func(coin string, x int, cross bool) (*core.LeverageResult, error) {
		if coin != "BTC" || x != 5 || !cross {
			t.Errorf("leverage args: coin=%q x=%d cross=%v", coin, x, cross)
		}
		return &core.LeverageResult{Leverage: 5, Mode: "cross"}, nil
	}})
	if env, err := runCmd(t, leverageCmd, []string{"BTC", "5"}); err != nil || !env.OK {
		t.Fatalf("leverage happy: env=%+v err=%v", env, err)
	}
	// risk-reject -> exit 20
	withFakeClient(t, wrFake{levFn: func(string, int, bool) (*core.LeverageResult, error) {
		return nil, output.Risk("max_leverage", "exceeds cap")
	}})
	if env, err := runCmd(t, leverageCmd, []string{"BTC", "50"}); err == nil || err.(*output.CmdError).Code != output.ExitRisk || env.Error.Code != "max_leverage" {
		t.Fatalf("leverage risk reject: env=%+v err=%v", env, err)
	}
	// bad integer -> validation, no client
	if env, err := runCmd(t, leverageCmd, []string{"BTC", "abc"}); err == nil || env.Error.Code != "bad_leverage" {
		t.Fatalf("leverage bad arg: env=%+v err=%v", env, err)
	}
}

func TestMarginCmd(t *testing.T) {
	wrResetGlobals(t)
	wRemove = true // --remove negates the usd
	withFakeClient(t, wrFake{marginFn: func(coin string, usd float64) (*core.MarginResult, error) {
		if coin != "ETH" || usd != -25 {
			t.Errorf("margin args: coin=%q usd=%v (want ETH/-25)", coin, usd)
		}
		return &core.MarginResult{}, nil
	}})
	if env, err := runCmd(t, marginCmd, []string{"ETH", "25"}); err != nil || !env.OK {
		t.Fatalf("margin: env=%+v err=%v", env, err)
	}
	// bad amount -> validation
	if env, err := runCmd(t, marginCmd, []string{"ETH", "-1"}); err == nil || env.Error.Code != "bad_amount" {
		t.Fatalf("margin bad amount: env=%+v err=%v", env, err)
	}
}

func TestTwapCmd(t *testing.T) {
	wrResetGlobals(t)
	wMinutes = 30
	withFakeClient(t, wrFake{twapFn: func(r core.TwapReq) (*core.TwapResult, []string, error) {
		if r.Coin != "BTC" || r.Side != core.Buy || r.Size != "0.5" || r.Minutes != 30 {
			t.Errorf("twap req not threaded: %+v", r)
		}
		return &core.TwapResult{}, []string{"builder fee NOT applied: ..."}, nil
	}})
	if env, err := runCmd(t, twapCmd, []string{"BTC", "buy", "0.5"}); err != nil || !env.OK || len(env.Warnings) == 0 {
		t.Fatalf("twap: env=%+v err=%v", env, err)
	}
}

func TestTwapStatusAndCancelCmd(t *testing.T) {
	wrResetGlobals(t)
	rCoin, wTwapID = "BTC", 99
	withFakeClient(t, wrFake{twapCxlFn: func(coin string, id int64) (*core.TwapCancelResult, error) {
		if coin != "BTC" || id != 99 {
			t.Errorf("twap cancel args: coin=%q id=%d", coin, id)
		}
		return &core.TwapCancelResult{}, nil
	}})
	if env, err := runCmd(t, twapCancelCmd, nil); err != nil || !env.OK {
		t.Fatalf("twap cancel: env=%+v err=%v", env, err)
	}
}

func TestPositionTpslCmd(t *testing.T) {
	wrResetGlobals(t)
	wTp, wSl = "62000", "55000"
	withFakeClient(t, wrFake{tpslFn: func(r core.PositionTpslReq) ([]*core.PlaceResult, []string, error) {
		if r.Coin != "BTC" {
			t.Errorf("tpsl coin: %q", r.Coin)
		}
		return []*core.PlaceResult{{Status: "resting"}, {Status: "resting"}}, nil, nil
	}})
	if env, err := runCmd(t, positionTpslCmd, []string{"BTC"}); err != nil || !env.OK {
		t.Fatalf("position-tpsl: env=%+v err=%v", env, err)
	}
}

func TestPanicCmd(t *testing.T) {
	wrResetGlobals(t)
	saveYes := flagYes
	flagYes = true // panic is destructive — skip the interactive confirm
	t.Cleanup(func() { flagYes = saveYes })
	withFakeClient(t, wrFake{panicFn: func() (*core.PanicResult, error) {
		return &core.PanicResult{Complete: true}, nil
	}})
	if env, err := runCmd(t, panicCmd, nil); err != nil || !env.OK || env.Cmd != "panic" {
		t.Fatalf("panic: env=%+v err=%v", env, err)
	}
}

// sell and order share runTrade with buy — cover the partial-fill exit path.
func TestSellAndOrderRunTrade(t *testing.T) {
	wrResetGlobals(t)
	partial := wrFake{placeFn: func(core.OrderReq) (*core.PlaceResult, []string, error) {
		return &core.PlaceResult{Status: "filled", Size: "1", FilledSz: "0.5"}, nil, nil
	}}
	withFakeClient(t, partial)
	if _, err := runCmd(t, sellCmd, []string{"BTC", "1"}); err == nil || err.(*output.CmdError).Code != output.ExitPartial {
		t.Fatalf("sell partial must be exit 60, got %v", err)
	}
	withFakeClient(t, wrFake{placeFn: func(r core.OrderReq) (*core.PlaceResult, []string, error) {
		if r.Side != core.Sell {
			t.Errorf("order coin/side: %+v", r)
		}
		return &core.PlaceResult{Status: "filled", Size: "1", FilledSz: "1"}, nil, nil
	}})
	if env, err := runCmd(t, orderCmd, []string{"BTC", "sell", "1"}); err != nil || !env.OK {
		t.Fatalf("order happy: env=%+v err=%v", env, err)
	}
}

// buy --tp/--sl routes through runTrade's bracket branch -> PlaceBracket.
func TestBuyBracketRunTrade(t *testing.T) {
	wrResetGlobals(t)
	wTp, wSl = "62000", "55000"
	withFakeClient(t, wrFake{bracketFn: func(r core.BracketReq) ([]*core.PlaceResult, []string, error) {
		if r.Coin != "BTC" || r.TP != "62000" || r.SL != "55000" {
			t.Errorf("bracket req not threaded: %+v", r)
		}
		return []*core.PlaceResult{{Status: "filled", Size: "0.1", FilledSz: "0.1"}, {Status: "resting"}, {Status: "resting"}}, nil, nil
	}})
	if env, err := runCmd(t, buyCmd, []string{"BTC", "0.1"}); err != nil || !env.OK || env.Cmd != "buy" {
		t.Fatalf("bracket happy: env=%+v err=%v", env, err)
	}
}

// writeConfigTemplate drops a commented config at config.Path() (0600), used by `init`.
func TestWriteConfigTemplate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DELIVERATOR_HOME", home)
	if err := writeConfigTemplate(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(config.Path())
	if err != nil {
		t.Fatalf("template not written: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("template mode = %v, want 0600", fi.Mode().Perm())
	}
	loaded, err := config.Load(config.Path())
	if err != nil {
		t.Fatalf("template must be a valid loadable config: %v", err)
	}
	// Shipped defaults (opinionated onboarding): mainnet, xyz sub-dex, outcomes on,
	// limit-only on. Locked here so a future template edit can't silently drop them.
	if loaded.Network != config.NetworkMainnet {
		t.Errorf("shipped network = %q, want mainnet", loaded.Network)
	}
	if !loaded.Outcomes {
		t.Error("shipped default should enable outcomes")
	}
	if len(loaded.PerpDexs) != 1 || loaded.PerpDexs[0] != "xyz" {
		t.Errorf("shipped perp_dexs = %v, want [xyz]", loaded.PerpDexs)
	}
	if !loaded.Automation.LimitOnly {
		t.Error("shipped default should enable limit_only")
	}
}

// snapshot exercises runReadWarn (read + top-level warnings).
func TestSnapshotCmd(t *testing.T) {
	wrResetGlobals(t)
	withFakeClient(t, wrFake{snapFn: func([]string) (*core.SnapshotView, []string, error) {
		return &core.SnapshotView{}, []string{"ctx: rate limited"}, nil
	}})
	env, err := runCmd(t, snapshotCmd, nil)
	if err != nil || !env.OK || env.Cmd != "snapshot" {
		t.Fatalf("snapshot: env=%+v err=%v", env, err)
	}
	if len(env.Warnings) == 0 {
		t.Errorf("snapshot should surface top-level warnings: %+v", env)
	}
}
