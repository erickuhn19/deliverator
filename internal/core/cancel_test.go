package core

import (
	"fmt"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
)

// cancelResp wraps MixedArray status elements into an /exchange cancel response.
func cancelResp(statuses string) string {
	return `{"status":"ok","response":{"type":"cancel","data":{"statuses":[` + statuses + `]}}}`
}

// openOrderJSON is a minimal FrontendOpenOrder used for coin resolution.
func openOrderJSON(coin string, oid int64, cloid string) string {
	cl := "null"
	if cloid != "" {
		cl = `"` + cloid + `"`
	}
	return fmt.Sprintf(`{"coin":%q,"oid":%d,"cloid":%s,"limitPx":"60000","origSz":"0.001","sz":"0.001","side":"B","orderType":"Limit","tif":"Gtc","reduceOnly":false,"isTrigger":false,"isPositionTpsl":false,"triggerCondition":"N/A","triggerPx":"0","timestamp":1}`, coin, oid, cl)
}

// Batch cancel by oid list: coins resolved from one open-orders read, all succeed.
func TestCancelBatchByOids(t *testing.T) {
	front := "[" + openOrderJSON("BTC", 1, "") + "," + openOrderJSON("BTC", 2, "") + "]"
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", front, cancelResp(`"success","success"`)))
	res, err := c.Cancel(ctx, CancelReq{Oids: []int64{1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Canceled != 2 || len(res.Oids) != 2 || len(res.Failed) != 0 {
		t.Fatalf("want 2 canceled, 0 failed: %+v", res)
	}
}

// An oid not on the book is reported as Failed (already gone) and never sent;
// the remaining legs still cancel.
func TestCancelBatchByOidsSomeGone(t *testing.T) {
	front := "[" + openOrderJSON("BTC", 1, "") + "," + openOrderJSON("BTC", 2, "") + "]"
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", front, cancelResp(`"success","success"`)))
	res, err := c.Cancel(ctx, CancelReq{Oids: []int64{1, 2, 99}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Canceled != 2 || len(res.Failed) != 1 || res.Failed[0].Oid == nil || *res.Failed[0].Oid != 99 {
		t.Fatalf("oid 99 should be failed (gone): %+v", res)
	}
}

// A per-leg exchange error (already filled/canceled) is reported on that leg
// without failing the whole batch.
func TestCancelBatchPerLegError(t *testing.T) {
	front := "[" + openOrderJSON("BTC", 1, "") + "," + openOrderJSON("BTC", 2, "") + "]"
	resp := cancelResp(`"success",{"error":"Order was never placed, already canceled, or filled"}`)
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", front, resp))
	res, err := c.Cancel(ctx, CancelReq{Oids: []int64{1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Canceled != 1 || len(res.Oids) != 1 || res.Oids[0] != 1 {
		t.Fatalf("only oid 1 should cancel: %+v", res)
	}
	if len(res.Failed) != 1 || res.Failed[0].Oid == nil || *res.Failed[0].Oid != 2 {
		t.Fatalf("oid 2 should be failed: %+v", res)
	}
}

// --coin pins every leg's coin, so no open-orders read is needed.
func TestCancelBatchCoinPinned(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "[]", cancelResp(`"success","success"`)))
	res, err := c.Cancel(ctx, CancelReq{Oids: []int64{10, 11}, Coin: "BTC"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Canceled != 2 {
		t.Fatalf("coin-pinned batch should cancel 2 without a lookup: %+v", res)
	}
}

// Batch cancel by cloid list, coins resolved from the book.
func TestCancelBatchByCloids(t *testing.T) {
	cl1 := "0x00000000000000000000000000000001"
	cl2 := "0x00000000000000000000000000000002"
	front := "[" + openOrderJSON("BTC", 1, cl1) + "," + openOrderJSON("BTC", 2, cl2) + "]"
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", front, cancelResp(`"success","success"`)))
	res, err := c.Cancel(ctx, CancelReq{Cloids: []string{cl1, cl2}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Canceled != 2 || len(res.Cloids) != 2 {
		t.Fatalf("want 2 cloids canceled: %+v", res)
	}
}

// Mixing --oids and --cloids in one batch is rejected (different signed actions).
func TestCancelBatchMixedRejected(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "[]", cancelResp(`"success"`)))
	_, err := c.Cancel(ctx, CancelReq{Oids: []int64{1}, Cloids: []string{"0x00000000000000000000000000000001"}})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

// A short status array must not over-report: a leg with no returned status is
// reported unconfirmed (Failed), never counted as canceled.
func TestCancelBatchShortStatusUnconfirmed(t *testing.T) {
	front := "[" + openOrderJSON("BTC", 1, "") + "," + openOrderJSON("BTC", 2, "") + "]"
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", front, cancelResp(`"success"`))) // 1 status, 2 legs
	res, err := c.Cancel(ctx, CancelReq{Oids: []int64{1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Canceled != 1 || len(res.Oids) != 1 || res.Oids[0] != 1 {
		t.Fatalf("only the confirmed leg should count: %+v", res)
	}
	if len(res.Failed) != 1 || res.Failed[0].Oid == nil || *res.Failed[0].Oid != 2 {
		t.Fatalf("unconfirmed leg 2 must be Failed: %+v", res)
	}
}

// A single --oid/--cloid combined with a batch list is rejected (a requested
// target must never be silently dropped).
func TestCancelBatchSingularPlusBatchRejected(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "[]", cancelResp(`"success"`)))
	oid := int64(5)
	_, err := c.Cancel(ctx, CancelReq{Oid: &oid, Oids: []int64{1, 2}, Coin: "BTC"})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

// An empty element in a --cloids list is rejected, never turned into a random
// (phantom) cloid that gets signed.
func TestCancelBatchEmptyCloidRejected(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "[]", cancelResp(`"success"`)))
	_, err := c.Cancel(ctx, CancelReq{Cloids: []string{"0x00000000000000000000000000000001", ""}, Coin: "BTC"})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

// An unknown coin fails only its own leg (asset pre-check), never aborts the batch.
func TestCancelBatchUnknownCoinPerLeg(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "[]", cancelResp(`"success"`)))
	res, err := c.Cancel(ctx, CancelReq{Oids: []int64{1, 2}, Coin: "NOTACOIN"})
	if err != nil {
		t.Fatalf("unknown coin must not hard-fail the batch: %v", err)
	}
	if res.Canceled != 0 || len(res.Failed) != 2 {
		t.Fatalf("both legs should be Failed (unknown asset), not aborted: %+v", res)
	}
}

// Dry-run resolves + reports the targets without signing.
func TestCancelBatchDryRun(t *testing.T) {
	front := "[" + openOrderJSON("BTC", 1, "") + "," + openOrderJSON("BTC", 2, "") + "]"
	c, ctx := newTestClient(t, config.Default(), Options{DryRun: true}, engineResp("", "", front, cancelResp(`"success","success"`)))
	res, err := c.Cancel(ctx, CancelReq{Oids: []int64{1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || res.Canceled != 2 {
		t.Fatalf("dry-run should report 2 targets: %+v", res)
	}
}
