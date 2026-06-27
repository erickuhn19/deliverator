package core

import (
	"strconv"
	"strings"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
)

func i64(v int64) *int64 { return &v }

// Modify two resting orders in one action: both re-priced, one re-sized.
func TestModifyBatchHappy(t *testing.T) {
	front := "[" + openOrderJSON("BTC", 1, "") + "," + openOrderJSON("BTC", 2, "") + "]"
	// builderAllCfg + approved so the builder-dropped-on-modify warning fires.
	c, ctx := newTestClient(t, builderAllCfg(), Options{}, approve(100, engineResp("", "", front, okOrders(`{"resting":{"oid":1}}`, `{"resting":{"oid":2}}`))))
	res, w, err := c.ModifyBatch(ctx, []ModifyReq{{Oid: i64(1), Limit: "61000"}, {Oid: i64(2), Limit: "62000", Size: "0.002"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Status != "resting" || res[0].LimitPx != "61000" {
		t.Fatalf("leg 0 wrong: %+v", res[0])
	}
	if res[1].LimitPx != "62000" || res[1].Size != "0.002" {
		t.Fatalf("leg 1 wrong: %+v", res[1])
	}
	if !warningsContain(w, "builder fee dropped") {
		t.Errorf("modify must warn the builder fee is dropped: %v", w)
	}
}

// Atomic pre-flight: one unresolvable target rejects the whole batch (nothing signed).
func TestModifyBatchOrderNotFound(t *testing.T) {
	front := "[" + openOrderJSON("BTC", 1, "") + "]"
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", front, okOrders(`{"resting":{"oid":1}}`)))
	_, _, err := c.ModifyBatch(ctx, []ModifyReq{{Oid: i64(1), Limit: "61000"}, {Oid: i64(99), Limit: "62000"}})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

// A trigger order cannot be modified in place — rejected pre-flight.
func TestModifyBatchTriggerRejected(t *testing.T) {
	trig := `{"coin":"BTC","oid":5,"cloid":null,"limitPx":"60000","origSz":"0.001","sz":"0.001","side":"B","orderType":"Stop Limit","tif":"Gtc","reduceOnly":false,"isTrigger":true,"isPositionTpsl":false,"triggerCondition":"x","triggerPx":"59000","timestamp":1}`
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "["+trig+"]", "{}"))
	_, _, err := c.ModifyBatch(ctx, []ModifyReq{{Oid: i64(5), Limit: "61000"}})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

// Targeting the same order twice in one batch is rejected.
func TestModifyBatchDupTarget(t *testing.T) {
	front := "[" + openOrderJSON("BTC", 1, "") + "]"
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", front, "{}"))
	_, _, err := c.ModifyBatch(ctx, []ModifyReq{{Oid: i64(1), Limit: "61000"}, {Oid: i64(1), Limit: "62000"}})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

// HL may reject one leg while the action succeeds — surfaced per-leg.
func TestModifyBatchPartialReject(t *testing.T) {
	front := "[" + openOrderJSON("BTC", 1, "") + "," + openOrderJSON("BTC", 2, "") + "]"
	resp := okOrders(`{"resting":{"oid":1}}`, `{"error":"Order was never placed, already canceled, or filled"}`)
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", front, resp))
	res, _, err := c.ModifyBatch(ctx, []ModifyReq{{Oid: i64(1), Limit: "61000"}, {Oid: i64(2), Limit: "62000"}})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Status != "resting" || res[1].Status != "rejected" || res[1].Error == "" {
		t.Fatalf("partial outcome wrong: %+v", res)
	}
}

func TestModifyBatchDryRun(t *testing.T) {
	front := "[" + openOrderJSON("BTC", 1, "") + "]"
	c, ctx := newTestClient(t, config.Default(), Options{DryRun: true}, engineResp("", "", front, "{}"))
	res, _, err := c.ModifyBatch(ctx, []ModifyReq{{Oid: i64(1), Limit: "61000"}})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Status != "dry_run" || !res[0].DryRun {
		t.Fatalf("dry-run leg wrong: %+v", res[0])
	}
}

// A modify REPLACES a resting order, so re-pricing N same-coin orders must NOT
// be summed against the position cap — exposure is unchanged. (10 × $60.10 would
// breach a $500 cap if wrongly accumulated; each leg alone is fine.)
func TestModifyBatchSameCoinRepriceNotSummed(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxPositionNotionalUSD = 500
	parts := make([]string, 10)
	reqs := make([]ModifyReq, 10)
	statuses := make([]string, 10)
	for i := 0; i < 10; i++ {
		parts[i] = openOrderJSON("BTC", int64(i+1), "")
		reqs[i] = ModifyReq{Oid: i64(int64(i + 1)), Limit: "60100"} // 0.001 BTC @ 60100 = $60.10
		statuses[i] = `{"resting":{"oid":` + strconv.Itoa(i+1) + `}}`
	}
	front := "[" + strings.Join(parts, ",") + "]"
	c, ctx := newTestClient(t, cfg, Options{}, engineResp("", "", front, okOrders(statuses...)))
	if _, _, err := c.ModifyBatch(ctx, reqs); err != nil {
		t.Fatalf("a same-coin re-price must not be summed against the position cap: %v", err)
	}
}

// The same order targeted once by oid and once by its cloid is a duplicate —
// dedup must collapse the two id forms to the resolved order's oid.
func TestModifyBatchDupTargetMixedIdForms(t *testing.T) {
	cl := "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	front := "[" + openOrderJSON("BTC", 1, cl) + "]"
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", front, "{}"))
	_, _, err := c.ModifyBatch(ctx, []ModifyReq{{Oid: i64(1), Limit: "61000"}, {Cloid: cl, Limit: "62000"}})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

func TestModifyBatchEmptyAndMissingId(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "[]", "{}"))
	_, _, err := c.ModifyBatch(ctx, nil)
	assertErr(t, err, output.CatValidation, output.ExitValidation)
	_, _, err = c.ModifyBatch(ctx, []ModifyReq{{Limit: "61000"}}) // neither oid nor cloid
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}
