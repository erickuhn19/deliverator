package core

import (
	"strings"
	"testing"
)

// The builder fee ships ON by default but attaches GRACEFULLY: only when the
// trader's master wallet has approved it up to at least the configured fee.
// Otherwise the order is placed fee-free (never rejected) and the skip is silent —
// the invitation to approve lives in onboard/connect/`builder status`, not per order.

func TestBuilderGracefulApprovedAttaches(t *testing.T) {
	// approved max 50 >= configured 50 -> attach + "applied" warning.
	c, ctx := newTestClient(t, builderAllCfg(), Options{}, approve(50, engineResp("", "", "", okOrder(`{"resting":{"oid":1}}`))))
	res, w, err := c.Place(ctx, limitBuy())
	if err != nil {
		t.Fatal(err)
	}
	if res.Builder == nil || res.Builder.FeeTenthsBps != 50 {
		t.Fatalf("approved (max=50, fee=50) must attach the builder, got %+v", res.Builder)
	}
	if !warningsContain(w, "builder fee 0.050% applied") {
		t.Errorf("approved attach should warn the fee applied: %v", w)
	}
}

func TestBuilderGracefulApprovedBelowFeeSkips(t *testing.T) {
	// approved max 40 < configured 50 -> skip: an attached fee above the approved
	// max would be rejected by Hyperliquid, blocking the trade.
	c, ctx := newTestClient(t, builderAllCfg(), Options{}, approve(40, engineResp("", "", "", okOrder(`{"resting":{"oid":1}}`))))
	res, w, err := c.Place(ctx, limitBuy())
	if err != nil {
		t.Fatal(err)
	}
	if res.Builder != nil {
		t.Fatalf("approved-below-fee must skip the builder, got %+v", res.Builder)
	}
	if warningsContain(w, "applied") {
		t.Errorf("must not claim a fee was applied when skipped: %v", w)
	}
}

func TestBuilderGracefulUnapprovedSkipsSilently(t *testing.T) {
	// maxBuilderFee 0 (never approved) -> place fee-free, no per-order builder warning.
	c, ctx := newTestClient(t, builderAllCfg(), Options{}, approve(0, engineResp("", "", "", okOrder(`{"resting":{"oid":1}}`))))
	res, w, err := c.Place(ctx, limitBuy())
	if err != nil {
		t.Fatal(err)
	}
	if res.Builder != nil {
		t.Fatalf("unapproved must skip the builder (place fee-free), got %+v", res.Builder)
	}
	for _, msg := range w {
		if strings.Contains(msg, "builder") {
			t.Errorf("unapproved skip must be silent (no per-order builder warning): %q", msg)
		}
	}
}

func TestBuilderGracefulCheckErrorSkips(t *testing.T) {
	// The approval read fails (engineResp returns {} -> unmarshal error). Fail safe:
	// skip the fee, never block the trade on an unverifiable approval.
	c, ctx := newTestClient(t, builderAllCfg(), Options{}, engineResp("", "", "", okOrder(`{"resting":{"oid":1}}`)))
	res, _, err := c.Place(ctx, limitBuy())
	if err != nil {
		t.Fatal(err)
	}
	if res.Builder != nil {
		t.Fatalf("a failed approval check must skip the builder (fail safe), got %+v", res.Builder)
	}
}

func TestBuilderGracefulExplicitOverrideAboveApprovedWarns(t *testing.T) {
	// An explicit --builder-fee above the master-approved max is dropped like any
	// unapproved fee — but because the user asked explicitly, it must say why (the
	// config-default skip stays silent).
	c, ctx := newTestClient(t, builderAllCfg(), Options{}, approve(50, engineResp("", "", "", okOrder(`{"resting":{"oid":1}}`))))
	o := 80
	req := limitBuy()
	req.BuilderFee = &o
	res, w, err := c.Place(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Builder != nil {
		t.Fatalf("override (80) above approved max (50) must be dropped, got %+v", res.Builder)
	}
	if !warningsContain(w, "--builder-fee 80 skipped") {
		t.Errorf("explicit-override drop must warn why: %v", w)
	}
}
