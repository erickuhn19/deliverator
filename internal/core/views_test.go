package core

import (
	"errors"
	"strings"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	hl "github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/output"
)

func sptr(s string) *string { return &s }

func TestSideFromSzi(t *testing.T) {
	cases := map[string]string{"-1.5": "short", "0": "flat", "0.0": "flat", "": "flat", "2.0": "long"}
	for szi, want := range cases {
		if got := sideFromSzi(szi); got != want {
			t.Errorf("sideFromSzi(%q) = %q, want %q", szi, got, want)
		}
	}
}

func TestToPositionViews(t *testing.T) {
	st := &hl.UserState{AssetPositions: []hl.AssetPosition{
		{Position: hl.Position{
			Coin: "BTC", Szi: "-0.5", PositionValue: "32500", UnrealizedPnl: "10",
			ReturnOnEquity: "0.1", MarginUsed: "100", EntryPx: sptr("65000"), LiquidationPx: sptr("90000"),
			Leverage: hl.Leverage{Type: "isolated", Value: 5},
		}},
		{Position: hl.Position{Coin: "ETH", Szi: "1.0", Leverage: hl.Leverage{Type: "cross", Value: 10}}},
	}}
	all := toPositionViews(st, "")
	if len(all) != 2 {
		t.Fatalf("want 2 positions, got %d", len(all))
	}
	if all[0].Side != "short" || all[0].EntryPx != "65000" || all[0].LiquidationPx != "90000" || all[0].LeverageType != "isolated" {
		t.Errorf("BTC view wrong: %+v", all[0])
	}
	only := toPositionViews(st, "ETH")
	if len(only) != 1 || only[0].Coin != "ETH" || only[0].Side != "long" {
		t.Errorf("coin filter wrong: %+v", only)
	}
}

func TestApplyStatus(t *testing.T) {
	filled := &PlaceResult{Size: "1.0"}
	applyStatus(filled, hl.OrderStatus{Filled: &hl.OrderStatusFilled{Oid: 7, TotalSz: "1.0", AvgPx: "100"}})
	if filled.Status != "filled" || filled.Oid == nil || *filled.Oid != 7 || filled.FilledSz != "1.0" || filled.AvgPx != "100" {
		t.Errorf("filled: %+v", filled)
	}
	resting := &PlaceResult{}
	applyStatus(resting, hl.OrderStatus{Resting: &hl.OrderStatusResting{Oid: 9}})
	if resting.Status != "resting" || resting.Oid == nil || *resting.Oid != 9 {
		t.Errorf("resting: %+v", resting)
	}
	sub := &PlaceResult{}
	applyStatus(sub, hl.OrderStatus{})
	if sub.Status != "submitted" {
		t.Errorf("submitted: %+v", sub)
	}
}

func TestMapOrderReject(t *testing.T) {
	cases := map[string]string{
		"Price not divisible by tick size":      "tick_reject",
		"Order has too many significant":        "tick_reject",
		"reduce only order rejected":            "reduce_only",
		"Order must have minimum value of $10.": "min_order_value",
		"Insufficient margin":                   "margin",
		"some other reason":                     "order_rejected",
	}
	for msg, wantCode := range cases {
		err := mapOrderReject(msg)
		var oe *output.Error
		if !errors.As(err, &oe) || oe.Category != output.CatExchange {
			t.Fatalf("mapOrderReject(%q) wrong type/category: %v", msg, err)
		}
		if oe.Code != wantCode {
			t.Errorf("mapOrderReject(%q) code = %q, want %q", msg, oe.Code, wantCode)
		}
		if oe.ExitCode() != output.ExitExchange {
			t.Errorf("mapOrderReject(%q) exit = %d, want %d", msg, oe.ExitCode(), output.ExitExchange)
		}
	}
}

func TestMapExchangeErr(t *testing.T) {
	cases := []struct {
		in   string
		cat  output.Category
		exit int
	}{
		{"context deadline exceeded", output.CatTimeout, output.ExitTimeout},
		{"request timed out", output.CatTimeout, output.ExitTimeout},
		{"too many requests", output.CatRateLimit, output.ExitRateLimit},
		{"insufficient margin", output.CatExchange, output.ExitExchange},
		{"Nonce 123 is too old", output.CatClock, output.ExitClock},
		{"invalid nonce: must be within window", output.CatClock, output.ExitClock},
		{"random failure", output.CatExchange, output.ExitExchange},
	}
	for _, c := range cases {
		err := mapExchangeErr(errors.New(c.in))
		var oe *output.Error
		if !errors.As(err, &oe) {
			t.Fatalf("mapExchangeErr(%q) not an *output.Error: %v", c.in, err)
		}
		if oe.Category != c.cat || oe.ExitCode() != c.exit {
			t.Errorf("mapExchangeErr(%q) = %s/%d, want %s/%d", c.in, oe.Category, oe.ExitCode(), c.cat, c.exit)
		}
	}
	// An already-typed *output.Error passes through unchanged.
	orig := output.Risk("x", "y")
	if got := mapExchangeErr(orig); got != orig {
		t.Error("typed *output.Error should pass through")
	}
}

func TestResolveBuilder(t *testing.T) {
	c := newCfgClient(t, config.Default())
	// Drive resolveBuilder (the pure config resolver) from a clean posture — the
	// shipped default attaches on by default, but this unit test builds the cases up.
	c.cfg.Builder = config.Builder{}
	// No builder address => nil.
	if b := c.resolveBuilder(nil); b != nil {
		t.Errorf("no address should be nil, got %+v", b)
	}
	c.cfg.Builder.Address = "0xbuilder"
	c.cfg.Builder.FeeTenthsBps = 50
	// Manual mode, no explicit flag => not attached.
	if b := c.resolveBuilder(nil); b != nil {
		t.Errorf("manual mode without flag should be nil, got %+v", b)
	}
	// Manual mode + explicit override => attached.
	o := 30
	if b := c.resolveBuilder(&o); b == nil || b.Fee != 30 || b.Builder != "0xbuilder" {
		t.Errorf("override should attach fee 30: %+v", b)
	}
	// Attach-all => attached with config fee.
	c.cfg.Builder.AttachMode = config.AttachAll
	if b := c.resolveBuilder(nil); b == nil || b.Fee != 50 {
		t.Errorf("attach-all should attach config fee 50: %+v", b)
	}
	// Fee <= 0 => nil.
	c.cfg.Builder.FeeTenthsBps = 0
	if b := c.resolveBuilder(nil); b != nil {
		t.Errorf("zero fee should be nil, got %+v", b)
	}
}

func TestTifAndSideHelpers(t *testing.T) {
	if tifOf("ioc") != hl.TifIoc || tifOf("alo") != hl.TifAlo || tifOf("") != hl.TifGtc || tifOf("GTC") != hl.TifGtc {
		t.Error("tifOf mapping wrong")
	}
	if sideOf(hl.OrderSideAsk) != Sell || sideOf(hl.OrderSideBid) != Buy {
		t.Error("sideOf mapping wrong")
	}
	if Buy.String() != "buy" || Sell.String() != "sell" {
		t.Error("Side.String wrong")
	}
}

func TestSignURLFor(t *testing.T) {
	if signURLFor(config.NetworkMainnet) != hl.MainnetAPIURL {
		t.Error("mainnet url wrong")
	}
	if signURLFor(config.NetworkTestnet) != hl.TestnetAPIURL {
		t.Error("testnet url wrong")
	}
}

func TestGenCloidFormat(t *testing.T) {
	cl, err := GenCloid()
	if err != nil {
		t.Fatalf("GenCloid errored under a working RNG: %v", err)
	}
	if !strings.HasPrefix(cl, "0x") || len(cl) != 34 {
		t.Errorf("GenCloid format wrong: %q", cl)
	}
	a, _ := GenCloid()
	b, _ := GenCloid()
	if a == b {
		t.Error("GenCloid should be unique")
	}
}

func TestTrimLevels(t *testing.T) {
	in := []hl.Level{{Px: 100, Sz: 1, N: 2}, {Px: 99, Sz: 2, N: 3}, {Px: 98, Sz: 3, N: 4}}
	out := trimLevels(in, 2)
	if len(out) != 2 || out[0].Px != "100" || out[1].Px != "99" {
		t.Errorf("trimLevels(2) wrong: %+v", out)
	}
	if len(trimLevels(in, 0)) != 3 {
		t.Error("trimLevels(0) should keep all")
	}
}
