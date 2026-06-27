package core

// Tests for the autonomous copy executor (audit #89: copy_execute.go was 0%). The
// money-moving loop is exercised end-to-end against the signing harness: budget,
// per-leg reject isolation, the exit-42 stop-the-cycle contract, and flip ordering.

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
)

func openLeg(coin, action, size string) DiffLeg {
	return DiffLeg{Coin: coin, Class: "open", Action: action, Size: size}
}

const filledOrder = `{"filled":{"totalSz":"0.01","avgPx":"64000","oid":1}}`

func TestCopyExecuteOpensFill(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", okOrder(filledOrder)))
	diff := &CopyDiff{Leader: "0xL", Diff: []DiffLeg{openLeg("BTC", "buy", "0.01"), openLeg("ETH", "buy", "0.1")}}
	res, err := c.CopyExecute(ctx, diff, CopyParams{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Executed != 2 || !res.Complete || len(res.Legs) != 2 {
		t.Fatalf("both opens should fill, complete: %+v", res)
	}
}

func TestCopyExecuteBudgetStops(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", okOrder(filledOrder)))
	diff := &CopyDiff{Leader: "0xL", Diff: []DiffLeg{openLeg("BTC", "buy", "0.01"), openLeg("ETH", "buy", "0.1")}}
	res, _ := c.CopyExecute(ctx, diff, CopyParams{MaxOrdersPerCycle: 1})
	if res.Executed != 1 || res.Complete || len(res.Legs) != 1 {
		t.Fatalf("MaxOrdersPerCycle=1 must execute one leg and report incomplete: %+v", res)
	}
}

func TestCopyExecuteRejectContinues(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", okOrder(`{"error":"Insufficient margin"}`)))
	diff := &CopyDiff{Leader: "0xL", Diff: []DiffLeg{openLeg("BTC", "buy", "0.01"), openLeg("ETH", "buy", "0.1")}}
	res, _ := c.CopyExecute(ctx, diff, CopyParams{})
	if res.Executed != 2 || res.Complete {
		t.Fatalf("a (non-timeout) reject isolates the leg and the cycle continues: %+v", res)
	}
	for _, l := range res.Legs {
		if l.Status != "rejected" {
			t.Errorf("leg status = %q, want rejected", l.Status)
		}
	}
	if len(res.UnknownCloids) != 0 {
		t.Errorf("a reject is not an unknown: %v", res.UnknownCloids)
	}
}

func TestCopyExecuteTimeoutStopsCycle(t *testing.T) {
	// The first leg's /exchange call hangs past the client timeout → CatTimeout
	// (outcome UNKNOWN). The cycle MUST stop, surface the cloid in UnknownCloids, and
	// NOT attempt the second leg (the documented double-place hazard).
	var calls int32
	resp := func(path, typ string, _ map[string]any) (int, string) {
		if path == "/exchange" {
			if atomic.AddInt32(&calls, 1) == 1 {
				time.Sleep(400 * time.Millisecond) // exceed the client timeout below
			}
			return 200, okOrder(filledOrder)
		}
		switch typ {
		case "allMids":
			return 200, `{"BTC":"64000","ETH":"3000"}`
		}
		return 200, `{}`
	}
	// CopyExecute gives each leg a context budget of opts.Timeout+5s; a negative
	// opts.Timeout shrinks that to ~200ms so the hung /exchange call trips the
	// request deadline → CatTimeout (the harness exchange client has no timeout).
	c, ctx := newTestClient(t, config.Default(), Options{Timeout: 200*time.Millisecond - 5*time.Second}, resp)
	diff := &CopyDiff{Leader: "0xL", Diff: []DiffLeg{openLeg("BTC", "buy", "0.01"), openLeg("ETH", "buy", "0.1")}}
	res, _ := c.CopyExecute(ctx, diff, CopyParams{})
	if res.Complete || len(res.UnknownCloids) != 1 {
		t.Fatalf("timeout must mark incomplete with one unknown cloid: %+v", res)
	}
	if res.Executed != 1 || len(res.Legs) != 1 || res.Legs[0].Status != "unknown" {
		t.Fatalf("timeout must stop after the unknown leg, second leg not attempted: %+v", res)
	}
}

func TestCopyExecuteFlipCloseRejectSkipsOpen(t *testing.T) {
	// A flip closes the old side then opens the new one. If the close is rejected,
	// the open must NOT run (never gross-cross).
	resp := func(path, typ string, _ map[string]any) (int, string) {
		if path == "/exchange" {
			return 200, okOrder(`{"error":"cannot close"}`)
		}
		switch typ {
		case "clearinghouseState":
			return 200, btcShort
		case "allMids":
			return 200, `{"BTC":"64000"}`
		}
		return 200, `{}`
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	diff := &CopyDiff{Leader: "0xL", Diff: []DiffLeg{{Coin: "BTC", Class: "flip", Action: "buy", Size: "0.02"}}}
	res, _ := c.CopyExecute(ctx, diff, CopyParams{})
	if len(res.Legs) != 1 {
		t.Fatalf("a rejected flip-close must produce ONE leg (no open), got %d: %+v", len(res.Legs), res.Legs)
	}
	if res.Legs[0].Class != "flip-close" || res.Legs[0].Status != "rejected" || res.Complete {
		t.Fatalf("flip-close should be the only (rejected) leg: %+v", res)
	}
}

func TestIsCopyTimeout(t *testing.T) {
	if !isCopyTimeout(output.Timeout("t", "x")) {
		t.Error("a CatTimeout error should be a copy timeout")
	}
	if isCopyTimeout(output.Exchange("e", "x")) {
		t.Error("a CatExchange error is not a timeout")
	}
	if isCopyTimeout(errors.New("plain")) {
		t.Error("a plain error is not a timeout")
	}
	if !isCopyTimeout(fmt.Errorf("wrap: %w", output.Timeout("t", "x"))) {
		t.Error("a wrapped CatTimeout should still be detected")
	}
}
