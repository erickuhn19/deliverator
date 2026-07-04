package config

import (
	"strings"
	"testing"
)

// TestIsMMKey nails the "mm." routing predicate the CLI uses to dispatch to
// SetMMKey. Only the exact prefix counts — a bare "mm" or a "mmx." must not route.
func TestIsMMKey(t *testing.T) {
	yes := []string{"mm.enabled", "mm.selection.pins", "mm.", "mm.a.b.c"}
	for _, k := range yes {
		if !IsMMKey(k) {
			t.Errorf("IsMMKey(%q) = false, want true", k)
		}
	}
	no := []string{"", "mm", "mmenabled", "risk.max_leverage", "MM.enabled", " mm.enabled"}
	for _, k := range no {
		if IsMMKey(k) {
			t.Errorf("IsMMKey(%q) = true, want false", k)
		}
	}
}

// TestMMOrDefault_NonNil exercises the branch where an [mm] table IS present:
// MMOrDefault must return the operator's own pointer (not a fresh DefaultMM),
// preserving whatever they configured. TestMM_AbsentIsNil covers the nil branch.
func TestMMOrDefault_NonNil(t *testing.T) {
	cfg := Default()
	cfg.MM = DefaultMM()
	cfg.MM.QuoteIntervalMs = 1234
	got := cfg.MMOrDefault()
	if got != cfg.MM {
		t.Fatalf("MMOrDefault should return the configured pointer, got a different one")
	}
	if got.QuoteIntervalMs != 1234 {
		t.Fatalf("MMOrDefault dropped the configured value: got %d want 1234", got.QuoteIntervalMs)
	}
}

// TestSetMMKey_EveryScalarCase drives every key arm of SetMMKey with a value that
// DIFFERS from its DefaultMM default and asserts the exact field took that value.
// Each row starts from a nil MM so it also re-exercises the DefaultMM seed path, and
// the per-field check would catch a mis-wired case (e.g. min_s writing min_c).
func TestSetMMKey_EveryScalarCase(t *testing.T) {
	cases := []struct {
		key, val string
		check    func(*MM) bool
	}{
		// top-level
		{"mm.enabled", "true", func(m *MM) bool { return m.Enabled }},
		{"mm.dry_run", "false", func(m *MM) bool { return !m.DryRun }},
		{"mm.quote_interval_ms", "250", func(m *MM) bool { return m.QuoteIntervalMs == 250 }},
		// selection ints
		{"mm.selection.max_active_markets", "9", func(m *MM) bool { return m.Selection.MaxActiveMarkets == 9 }},
		{"mm.selection.min_ttl_mins", "5", func(m *MM) bool { return m.Selection.MinTTLMins == 5 }},
		{"mm.selection.max_ttl_mins", "1000", func(m *MM) bool { return m.Selection.MaxTTLMins == 1000 }},
		{"mm.selection.scan_interval_secs", "120", func(m *MM) bool { return m.Selection.ScanIntervalSecs == 120 }},
		{"mm.selection.max_per_underlying", "4", func(m *MM) bool { return m.Selection.MaxPerUnderlying == 4 }},
		{"mm.selection.max_per_expiry", "7", func(m *MM) bool { return m.Selection.MaxPerExpiry == 7 }},
		// selection lists
		{"mm.selection.priceable_underlyings", "BTC, ETH", func(m *MM) bool {
			p := m.Selection.PriceableUnderlyings
			return len(p) == 2 && p[0] == "BTC" && p[1] == "ETH"
		}},
		{"mm.selection.pins", "#1, #2", func(m *MM) bool {
			p := m.Selection.Pins
			return len(p) == 2 && p[0] == "#1" && p[1] == "#2"
		}},
		{"mm.selection.blacklist", "#3, #4", func(m *MM) bool {
			b := m.Selection.Blacklist
			return len(b) == 2 && b[0] == "#3" && b[1] == "#4"
		}},
		// selection floats
		{"mm.selection.w_liquidity", "0.41", func(m *MM) bool { return m.Selection.WLiquidity == 0.41 }},
		{"mm.selection.w_spread", "0.31", func(m *MM) bool { return m.Selection.WSpread == 0.31 }},
		{"mm.selection.w_confidence", "0.32", func(m *MM) bool { return m.Selection.WConfidence == 0.32 }},
		{"mm.selection.w_volume", "0.61", func(m *MM) bool { return m.Selection.WVolume == 0.61 }},
		{"mm.selection.w_depth", "0.39", func(m *MM) bool { return m.Selection.WDepth == 0.39 }},
		{"mm.selection.vol_sat", "12345", func(m *MM) bool { return m.Selection.VolSat == 12345 }},
		{"mm.selection.depth_ref", "6000", func(m *MM) bool { return m.Selection.DepthRef == 6000 }},
		{"mm.selection.spread_ref", "0.06", func(m *MM) bool { return m.Selection.SpreadRef == 0.06 }},
		{"mm.selection.half_spread_floor", "0.02", func(m *MM) bool { return m.Selection.HalfSpreadFloor == 0.02 }},
		{"mm.selection.plateau_k", "3", func(m *MM) bool { return m.Selection.PlateauK == 3 }},
		{"mm.selection.min_l", "0.11", func(m *MM) bool { return m.Selection.MinL == 0.11 }},
		{"mm.selection.min_s", "0.12", func(m *MM) bool { return m.Selection.MinS == 0.12 }},
		{"mm.selection.min_c", "0.13", func(m *MM) bool { return m.Selection.MinC == 0.13 }},
		{"mm.selection.hysteresis_margin", "0.06", func(m *MM) bool { return m.Selection.HysteresisMargin == 0.06 }},
		{"mm.selection.drop_threshold", "0.2", func(m *MM) bool { return m.Selection.DropThreshold == 0.2 }},
		// strategy
		{"mm.strategy.base_spread", "0.03", func(m *MM) bool { return m.Strategy.BaseSpread == 0.03 }},
		{"mm.strategy.levels", "4", func(m *MM) bool { return m.Strategy.Levels == 4 }},
		{"mm.strategy.level_step", "0.02", func(m *MM) bool { return m.Strategy.LevelStep == 0.02 }},
		{"mm.strategy.base_size_shares", "20", func(m *MM) bool { return m.Strategy.BaseSizeShares == 20 }},
		{"mm.strategy.size_step_shares", "6", func(m *MM) bool { return m.Strategy.SizeStepShares == 6 }},
		{"mm.strategy.inventory_skew", "0.6", func(m *MM) bool { return m.Strategy.InventorySkew == 0.6 }},
		{"mm.strategy.max_inventory_shares", "600", func(m *MM) bool { return m.Strategy.MaxInventoryShares == 600 }},
		{"mm.strategy.min_edge", "0.006", func(m *MM) bool { return m.Strategy.MinEdge == 0.006 }},
		{"mm.strategy.mid_anchor_weight", "0.9", func(m *MM) bool { return m.Strategy.MidAnchorWeight == 0.9 }},
		{"mm.strategy.quote_no_side", "false", func(m *MM) bool { return !m.Strategy.QuoteNoSide }},
		{"mm.strategy.no_side_min_price", "0.15", func(m *MM) bool { return m.Strategy.NoSideMinPrice == 0.15 }},
		// arb
		{"mm.arb.enabled", "true", func(m *MM) bool { return m.Arb.Enabled }},
		{"mm.arb.min_edge", "0.007", func(m *MM) bool { return m.Arb.MinEdge == 0.007 }},
		{"mm.arb.max_size_shares", "150", func(m *MM) bool { return m.Arb.MaxSizeShares == 150 }},
		// settle + fees
		{"mm.settle.blackout_mins", "20", func(m *MM) bool { return m.Settle.BlackoutMins == 20 }},
		{"mm.fees.close_fee_bps", "12.5", func(m *MM) bool { return m.Fees.CloseFeeBps == 12.5 }},
	}
	for _, c := range cases {
		cfg := &Config{} // MM nil -> SetMMKey must seed DefaultMM then apply
		if err := cfg.SetMMKey(c.key, c.val); err != nil {
			t.Fatalf("SetMMKey(%q,%q) unexpected err: %v", c.key, c.val, err)
		}
		if cfg.MM == nil {
			t.Fatalf("SetMMKey(%q,%q) left MM nil", c.key, c.val)
		}
		if !c.check(cfg.MM) {
			t.Errorf("SetMMKey(%q,%q) did not apply the expected value: %+v", c.key, c.val, cfg.MM)
		}
	}
}

// TestSetMMKey_BadValuesRejected covers each parse helper's error path (int, int64,
// float, bool) and the unknown-key arm — and asserts a rejected value does NOT mutate
// the field (the setX helpers only write on a successful parse).
func TestSetMMKey_BadValuesRejected(t *testing.T) {
	bad := []struct{ key, val string }{
		{"mm.selection.max_active_markets", "1.5"}, // setI: not an int
		{"mm.strategy.base_size_shares", "abc"},    // setI64: not an int64
		{"mm.selection.w_liquidity", "xyz"},        // setF: not a float
		{"mm.arb.enabled", "maybe"},                // setB: not a bool
		{"mm.strategy.mid_anchor_weight", "NaN?"},  // setF: not a float
		{"mm.selection.nope", "x"},                 // unknown key under a known table
		{"mm.totally.unknown", "x"},                // unknown key
	}
	for _, b := range bad {
		cfg := Default() // MM nil; SetMMKey seeds DefaultMM first
		if err := cfg.SetMMKey(b.key, b.val); err == nil {
			t.Errorf("SetMMKey(%q,%q) should have errored", b.key, b.val)
		}
	}

	// A failed parse must leave the prior value intact, not zero it.
	cfg := Default()
	cfg.MM = DefaultMM()
	want := cfg.MM.Selection.WLiquidity // 0.40 default
	if err := cfg.SetMMKey("mm.selection.w_liquidity", "not-a-number"); err == nil {
		t.Fatal("expected a parse error for a non-numeric weight")
	}
	if cfg.MM.Selection.WLiquidity != want {
		t.Fatalf("a rejected value must not mutate the field: got %v want %v", cfg.MM.Selection.WLiquidity, want)
	}
}

// TestMM_ValidateArms mutates one field of an otherwise-valid DefaultMM to an
// out-of-range value and asserts Validate rejects it, naming the offending knob.
// Covers the mm.validate arms the existing tests don't: the range checks and the
// per-section negative guards.
func TestMM_ValidateArms(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*MM)
		want string // substring the error must name
	}{
		{"quote_interval negative", func(m *MM) { m.QuoteIntervalMs = -1 }, "quote_interval_ms"},
		{"max_active_markets negative", func(m *MM) { m.Selection.MaxActiveMarkets = -1 }, "max_active_markets"},
		{"min_ttl negative", func(m *MM) { m.Selection.MinTTLMins = -1 }, "min_ttl_mins"},
		{"max_ttl negative", func(m *MM) { m.Selection.MaxTTLMins = -1 }, "max_ttl_mins"},
		{"scan_interval negative", func(m *MM) { m.Selection.ScanIntervalSecs = -1 }, "scan_interval_secs"},
		{"base_spread negative", func(m *MM) { m.Strategy.BaseSpread = -0.1 }, "base_spread"},
		{"level_step negative", func(m *MM) { m.Strategy.LevelStep = -0.1 }, "level_step"},
		{"min_edge negative", func(m *MM) { m.Strategy.MinEdge = -0.1 }, "min_edge"},
		{"inventory_skew negative", func(m *MM) { m.Strategy.InventorySkew = -0.1 }, "inventory_skew"},
		{"levels negative", func(m *MM) { m.Strategy.Levels = -1 }, "levels"},
		{"base_size negative", func(m *MM) { m.Strategy.BaseSizeShares = -1 }, "base_size_shares"},
		{"size_step negative", func(m *MM) { m.Strategy.SizeStepShares = -1 }, "size_step_shares"},
		{"max_inventory negative", func(m *MM) { m.Strategy.MaxInventoryShares = -1 }, "max_inventory_shares"},
		{"mid_anchor above 1", func(m *MM) { m.Strategy.MidAnchorWeight = 1.5 }, "mid_anchor_weight"},
		{"mid_anchor below 0", func(m *MM) { m.Strategy.MidAnchorWeight = -0.1 }, "mid_anchor_weight"},
		{"no_side_min at 1", func(m *MM) { m.Strategy.NoSideMinPrice = 1.0 }, "no_side_min_price"},
		{"no_side_min below 0", func(m *MM) { m.Strategy.NoSideMinPrice = -0.1 }, "no_side_min_price"},
		{"arb min_edge negative", func(m *MM) { m.Arb.MinEdge = -0.1 }, "arb"},
		{"arb max_size negative", func(m *MM) { m.Arb.MaxSizeShares = -1 }, "arb"},
		{"settle blackout negative", func(m *MM) { m.Settle.BlackoutMins = -1 }, "blackout_mins"},
		{"fees close negative", func(m *MM) { m.Fees.CloseFeeBps = -1 }, "close_fee_bps"},
	}
	for _, tc := range cases {
		cfg := Default()
		cfg.MM = DefaultMM()
		tc.mut(cfg.MM)
		err := cfg.Validate()
		if err == nil {
			t.Errorf("%s: expected a validation error, got nil", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q should name %q", tc.name, err.Error(), tc.want)
		}
	}
}

// TestMM_ValidateBoundaryAccepts guards against an over-strict comparison: the exact
// endpoints mid_anchor_weight=1 and no_side_min_price=0 are IN range and must pass.
func TestMM_ValidateBoundaryAccepts(t *testing.T) {
	cfg := Default()
	cfg.MM = DefaultMM()
	cfg.MM.Strategy.MidAnchorWeight = 1.0 // inclusive upper bound
	cfg.MM.Strategy.NoSideMinPrice = 0.0  // inclusive lower bound
	if err := cfg.Validate(); err != nil {
		t.Fatalf("mid_anchor_weight=1 and no_side_min_price=0 are in range, got %v", err)
	}
}

// TestRisk_OutcomeGuards covers the two HIP-4 risk arms: both are >= 0 (0 = off), so a
// negative value must be rejected, naming the knob.
func TestRisk_OutcomeGuards(t *testing.T) {
	c := Default()
	c.Risk.OutcomeSettleBlackoutMins = -1
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "outcome_settle_blackout_mins") {
		t.Fatalf("negative outcome_settle_blackout_mins should be rejected, got %v", err)
	}

	c = Default()
	c.Risk.MaxOutcomeQuestionNotionalUSD = -1
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "max_outcome_question_notional_usd") {
		t.Fatalf("negative max_outcome_question_notional_usd should be rejected, got %v", err)
	}

	// Zero (the off sentinel) stays valid for both.
	c = Default()
	c.Risk.OutcomeSettleBlackoutMins = 0
	c.Risk.MaxOutcomeQuestionNotionalUSD = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("zero (off) outcome guards must validate, got %v", err)
	}
}

// TestMM_LoadPartialEnabledOnlyInheritsDefaults writes a config whose [mm] block sets
// ONLY enabled=true, then asserts Load re-merged it over DefaultMM: enabled flips true
// while every other knob keeps its DefaultMM value (a plain overlay would zero them).
func TestMM_LoadPartialEnabledOnlyInheritsDefaults(t *testing.T) {
	cfg, err := Load(writeCfg(t, "[mm]\nenabled = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MM == nil {
		t.Fatal("[mm] present, want non-nil MM")
	}
	d := DefaultMM()
	if !cfg.MM.Enabled {
		t.Fatal("explicit enabled=true not applied")
	}
	// Everything absent from the file must inherit DefaultMM, not Go-zero.
	if cfg.MM.DryRun != d.DryRun {
		t.Errorf("dry_run: got %v want default %v", cfg.MM.DryRun, d.DryRun)
	}
	if cfg.MM.QuoteIntervalMs != d.QuoteIntervalMs {
		t.Errorf("quote_interval_ms: got %d want default %d", cfg.MM.QuoteIntervalMs, d.QuoteIntervalMs)
	}
	if cfg.MM.Selection.MaxActiveMarkets != d.Selection.MaxActiveMarkets {
		t.Errorf("selection.max_active_markets: got %d want default %d", cfg.MM.Selection.MaxActiveMarkets, d.Selection.MaxActiveMarkets)
	}
	if cfg.MM.Strategy.BaseSpread != d.Strategy.BaseSpread {
		t.Errorf("strategy.base_spread: got %v want default %v", cfg.MM.Strategy.BaseSpread, d.Strategy.BaseSpread)
	}
	if cfg.MM.Strategy.MidAnchorWeight != d.Strategy.MidAnchorWeight {
		t.Errorf("strategy.mid_anchor_weight: got %v want default %v", cfg.MM.Strategy.MidAnchorWeight, d.Strategy.MidAnchorWeight)
	}
	if cfg.MM.Settle.BlackoutMins != d.Settle.BlackoutMins {
		t.Errorf("settle.blackout_mins: got %d want default %d", cfg.MM.Settle.BlackoutMins, d.Settle.BlackoutMins)
	}
}
