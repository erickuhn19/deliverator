package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
)

// rateLogLines counts entries in the local rate-cap log under the temp HOME.
func rateLogLines(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(config.Dir(), "rate.log"))
	if err != nil {
		return 0
	}
	n := 0
	for _, ln := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	return n
}

// okOrders wraps N status elements into an /exchange order response.
func okOrders(statuses ...string) string {
	return `{"status":"ok","response":{"type":"order","data":{"statuses":[` +
		strings.Join(statuses, ",") + `]}}}`
}

func limitOrder(coin string, side Side, size, px string) OrderReq {
	return OrderReq{Coin: coin, Side: side, Size: size, Limit: px, Tif: "Gtc"}
}

// A batch of independent limits rests as one action: grouping "na", N labeled
// results, one builder warning, one "batch" audit row.
func TestPlaceBatchAllRest(t *testing.T) {
	var grouping string
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			return engineResp("", "", "", "")(path, typ, body)
		}
		if a, ok := body["action"].(map[string]any); ok {
			grouping, _ = a["grouping"].(string)
		}
		return 200, okOrders(`{"resting":{"oid":1}}`, `{"resting":{"oid":2}}`, `{"resting":{"oid":3}}`)
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	reqs := []OrderReq{
		limitOrder("BTC", Buy, "0.001", "60000"),
		limitOrder("BTC", Buy, "0.001", "59000"),
		limitOrder("BTC", Buy, "0.001", "58000"),
	}
	res, _, err := c.PlaceBatch(ctx, reqs)
	if err != nil {
		t.Fatal(err)
	}
	if grouping != "na" {
		t.Fatalf("batch must be independent (grouping na), got %q", grouping)
	}
	if len(res) != 3 {
		t.Fatalf("want 3 results, got %d", len(res))
	}
	for i, r := range res {
		if r.Status != "resting" || r.Oid == nil {
			t.Errorf("leg %d not resting: %+v", i, r)
		}
	}
	if a := readAudit(t); len(a) == 0 || a[len(a)-1]["action"] != "batch" {
		t.Errorf("expected a batch audit row, got %v", a)
	}
}

// HL may reject individual legs while the action succeeds; the rejected leg is a
// per-leg result (status rejected + error), not a whole-batch failure.
func TestPlaceBatchPartialReject(t *testing.T) {
	resp := okOrders(`{"resting":{"oid":1}}`, `{"error":"Order must have minimum value of $10."}`, `{"resting":{"oid":3}}`)
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", resp))
	res, _, err := c.PlaceBatch(ctx, []OrderReq{
		limitOrder("BTC", Buy, "0.001", "60000"),
		limitOrder("BTC", Buy, "0.001", "59000"),
		limitOrder("BTC", Buy, "0.001", "58000"),
	})
	if err != nil {
		t.Fatalf("a per-leg reject must not fail the whole call: %v", err)
	}
	if res[0].Status != "resting" || res[2].Status != "resting" {
		t.Errorf("legs 0/2 should rest: %+v", res)
	}
	if res[1].Status != "rejected" || !strings.Contains(res[1].Error, "minimum value") {
		t.Errorf("leg 1 should carry the reject reason: %+v", res[1])
	}
}

// Local validation is atomic: a sub-floor leg rejects the WHOLE batch pre-flight,
// before anything is signed (no batch audit row written).
func TestPlaceBatchAtomicPreflightReject(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", okOrders(`{"resting":{"oid":1}}`)))
	_, _, err := c.PlaceBatch(ctx, []OrderReq{
		limitOrder("BTC", Buy, "0.001", "60000"),  // $60 ok
		limitOrder("BTC", Buy, "0.0001", "60000"), // $6 < $10 floor
	})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
	for _, a := range readAudit(t) {
		if a["action"] == "batch" {
			t.Fatal("atomic pre-flight reject must not sign / audit the batch")
		}
	}
}

// The position cap is cumulative across same-coin legs: each leg is under the cap
// alone, but together they breach it -> the breaching leg is rejected pre-flight.
func TestPlaceBatchCumulativePositionCap(t *testing.T) {
	cfg := config.Default()
	cfg.Risk.MaxPositionNotionalUSD = 1000 // each leg $640; two legs = $1280 > cap
	c, ctx := newTestClient(t, cfg, Options{}, engineResp("", "", "", okOrders(`{"resting":{"oid":1}}`)))
	_, _, err := c.PlaceBatch(ctx, []OrderReq{
		limitOrder("BTC", Buy, "0.01", "64000"),
		limitOrder("BTC", Buy, "0.01", "64000"),
	})
	assertErr(t, err, output.CatRisk, output.ExitRisk)
}

// Duplicate explicit client order ids in one batch are rejected pre-flight.
func TestPlaceBatchDuplicateCloid(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", okOrders(`{"resting":{"oid":1}}`)))
	dup := "0x00000000000000000000000000000001"
	a := limitOrder("BTC", Buy, "0.001", "60000")
	a.Cloid = dup
	b := limitOrder("BTC", Buy, "0.001", "59000")
	b.Cloid = dup
	_, _, err := c.PlaceBatch(ctx, []OrderReq{a, b})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

// One signed action charges the rate cap ONCE: a 5-order batch passes even when
// the per-minute cap is 1 (per-leg charging would self-trip at leg 2).
func TestPlaceBatchRateCapChargedOnce(t *testing.T) {
	cfg := config.Default()
	cfg.Automation.MaxOrdersPerMin = 1
	c, ctx := newTestClient(t, cfg, Options{}, engineResp("", "", "",
		okOrders(`{"resting":{"oid":1}}`, `{"resting":{"oid":2}}`, `{"resting":{"oid":3}}`, `{"resting":{"oid":4}}`, `{"resting":{"oid":5}}`)))
	reqs := make([]OrderReq, 5)
	for i := range reqs {
		reqs[i] = limitOrder("BTC", Buy, "0.001", "60000")
	}
	if _, _, err := c.PlaceBatch(ctx, reqs); err != nil {
		t.Fatalf("batch should charge the rate cap once, got %v", err)
	}
}

func TestPlaceBatchEmptyAndTooLarge(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", "{}"))
	_, _, err := c.PlaceBatch(ctx, nil)
	assertErr(t, err, output.CatValidation, output.ExitValidation)

	big := make([]OrderReq, maxBatchOrders+1)
	for i := range big {
		big[i] = limitOrder("BTC", Buy, "0.001", "60000")
	}
	_, _, err = c.PlaceBatch(ctx, big)
	assertErr(t, err, output.CatValidation, output.ExitValidation)
}

// Dry-run never signs and labels every leg dry_run.
func TestPlaceBatchDryRun(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{DryRun: true}, engineResp("", "", "", "{}"))
	res, _, err := c.PlaceBatch(ctx, []OrderReq{
		limitOrder("BTC", Buy, "0.001", "60000"),
		limitOrder("BTC", Sell, "0.001", "70000"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Status != "dry_run" || !r.DryRun {
			t.Errorf("leg not dry-run: %+v", r)
		}
	}
}

// HL can reject the WHOLE action at the envelope level (Ok=false, reason, no
// per-leg statuses) — e.g. "Must deposit before trading". That must be a graceful
// error carrying the reason, never a nil-deref panic.
func TestPlaceBatchWholeActionReject(t *testing.T) {
	env := `{"status":"err","response":"Must deposit before trading"}`
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", env))
	_, _, err := c.PlaceBatch(ctx, []OrderReq{limitOrder("BTC", Buy, "0.001", "60000")})
	assertErr(t, err, output.CatExchange, output.ExitExchange)
	if err == nil || !strings.Contains(err.Error(), "deposit") {
		t.Fatalf("should surface the action reason, got %v", err)
	}
}

// An ok-but-empty statuses body is also a graceful error, not a panic.
func TestPlaceBatchEmptyStatusesBody(t *testing.T) {
	empty := `{"status":"ok","response":{"type":"order","data":{"statuses":[]}}}`
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", empty))
	_, _, err := c.PlaceBatch(ctx, []OrderReq{limitOrder("BTC", Buy, "0.001", "60000")})
	assertErr(t, err, output.CatExchange, output.ExitExchange)
}

// The rate cap is charged AFTER atomic validation: a pre-flight-rejected batch
// burns zero slots (parity with single Place), a signed batch burns exactly one.
func TestPlaceBatchRateCapChargedAfterValidation(t *testing.T) {
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", okOrders(`{"resting":{"oid":1}}`)))
	_, _, err := c.PlaceBatch(ctx, []OrderReq{
		limitOrder("BTC", Buy, "0.001", "60000"),
		limitOrder("BTC", Buy, "0.0001", "60000"), // $6 < $10 floor -> atomic reject
	})
	assertErr(t, err, output.CatValidation, output.ExitValidation)
	if n := rateLogLines(t); n != 0 {
		t.Fatalf("pre-flight-rejected batch must charge 0 rate slots, got %d", n)
	}
	if _, _, err := c.PlaceBatch(ctx, []OrderReq{limitOrder("BTC", Buy, "0.001", "60000")}); err != nil {
		t.Fatal(err)
	}
	if n := rateLogLines(t); n != 1 {
		t.Fatalf("signed batch must charge exactly 1 rate slot, got %d", n)
	}
}

// If HL returns FEWER statuses than orders, the unmatched legs are flagged
// "unknown" with a reason — never left blank and read as success.
func TestPlaceBatchShortStatusResponse(t *testing.T) {
	resp := okOrders(`{"resting":{"oid":1}}`, `{"resting":{"oid":2}}`) // 2 statuses for 3 orders
	c, ctx := newTestClient(t, config.Default(), Options{}, engineResp("", "", "", resp))
	res, _, err := c.PlaceBatch(ctx, []OrderReq{
		limitOrder("BTC", Buy, "0.001", "60000"),
		limitOrder("BTC", Buy, "0.001", "59000"),
		limitOrder("BTC", Buy, "0.001", "58000"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res[2].Status != "unknown" || res[2].Error == "" {
		t.Fatalf("unmatched leg must be flagged unknown with a reason: %+v", res[2])
	}
}

func TestBuildGrid(t *testing.T) {
	c := newCfgClient(t, config.Default())
	reqs, err := c.BuildGrid(GridReq{Coin: "BTC", Side: Buy, Levels: 3, FromPx: "100", ToPx: "200", TotalSize: "3", Tif: "Gtc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 3 {
		t.Fatalf("want 3 levels, got %d", len(reqs))
	}
	wantPx := []string{"100", "150", "200"}
	for i, r := range reqs {
		if r.Limit != wantPx[i] || r.Size != "1" || r.Coin != "BTC" || r.Side != Buy {
			t.Errorf("level %d wrong: %+v (want px %s)", i, r, wantPx[i])
		}
	}
	// Single level sits at FromPx.
	one, err := c.BuildGrid(GridReq{Coin: "BTC", Side: Sell, Levels: 1, FromPx: "100", ToPx: "200", TotalSize: "2"})
	if err != nil || len(one) != 1 || one[0].Limit != "100" || one[0].Size != "2" {
		t.Fatalf("single-level grid wrong: %+v (err %v)", one, err)
	}
}

func TestBuildGridRejectsBadInput(t *testing.T) {
	c := newCfgClient(t, config.Default())
	cases := []GridReq{
		{Coin: "BTC", Side: Buy, Levels: 0, FromPx: "100", ToPx: "200", TotalSize: "3"},
		{Coin: "BTC", Side: Buy, Levels: 3, FromPx: "x", ToPx: "200", TotalSize: "3"},
		{Coin: "BTC", Side: Buy, Levels: 3, FromPx: "100", ToPx: "200", TotalSize: "0"},
		{Coin: "BTC", Side: Buy, Levels: maxBatchOrders + 1, FromPx: "100", ToPx: "200", TotalSize: "3"},
	}
	for i, gr := range cases {
		if _, err := c.BuildGrid(gr); err == nil {
			t.Errorf("case %d should reject, got nil", i)
		}
	}
}
