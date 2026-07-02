package cmd

// NEXT-2 items 1 + 7 at the envelope level: a tolerant read with partial
// coverage (unreadable sub-dex) or a truncated paged history must exit 0 but
// carry warnings PLUS the additive machine-readable envelope fields
// (degraded_dexs / truncated). Uses the rd* offline harness (reads_runE_test.go)
// with its dex-scoped "<type>@<dex>" failure registration.

import (
	"strconv"
	"strings"
	"testing"
)

// rdWithSubDex configures the shared harness with one HIP-3 sub-dex.
func rdWithSubDex(t *testing.T) *rdServer {
	t.Helper()
	srv := rdSetup(t)
	savePerpDexs := Cfg.PerpDexs
	Cfg.PerpDexs = []string{"xyz"}
	t.Cleanup(func() { Cfg.PerpDexs = savePerpDexs })
	return srv
}

func rdWarningsJoined(env rdEnvelope) string { return strings.Join(env.Warnings, " | ") }

// portfolio with an unreadable sub-dex clearinghouse: exit 0, warnings, and
// degraded_dexs both at the top level and inside data.
func TestPortfolioRunEDegradedSubDexEnvelope(t *testing.T) {
	srv := rdWithSubDex(t)
	srv.fail("clearinghouseState@xyz")
	env, err := rdRun(t, portfolioCmd, nil)
	rdAssertOK(t, env, err, "portfolio")
	if len(env.DegradedDexs) != 1 || env.DegradedDexs[0] != "xyz" {
		t.Fatalf("envelope degraded_dexs must name the unreadable sub-dex, got %v", env.DegradedDexs)
	}
	if !strings.Contains(rdWarningsJoined(env), "sub-dex state (xyz)") {
		t.Fatalf("degradation must be warned, got %v", env.Warnings)
	}
	if !strings.Contains(string(env.Data), `"degraded_dexs":["xyz"]`) {
		t.Fatalf("portfolio data must carry degraded_dexs, got %s", env.Data)
	}
}

// orders with an unreadable sub-dex sweep: exit 0 + degraded_dexs + warning.
func TestOrdersRunEDegradedSubDexEnvelope(t *testing.T) {
	srv := rdWithSubDex(t)
	srv.fail("frontendOpenOrders@xyz")
	env, err := rdRun(t, ordersCmd, nil)
	rdAssertOK(t, env, err, "orders")
	if len(env.DegradedDexs) != 1 || env.DegradedDexs[0] != "xyz" {
		t.Fatalf("orders envelope must carry degraded_dexs, got %v", env.DegradedDexs)
	}
	if !strings.Contains(rdWarningsJoined(env), "open orders (xyz)") {
		t.Fatalf("degraded sweep must be warned, got %v", env.Warnings)
	}
}

// positions with an unreadable sub-dex: exit 0 + degraded_dexs + warning.
func TestPositionsRunEDegradedSubDexEnvelope(t *testing.T) {
	srv := rdWithSubDex(t)
	srv.fail("clearinghouseState@xyz")
	env, err := rdRun(t, positionsCmd, nil)
	rdAssertOK(t, env, err, "positions")
	if len(env.DegradedDexs) != 1 || env.DegradedDexs[0] != "xyz" {
		t.Fatalf("positions envelope must carry degraded_dexs, got %v", env.DegradedDexs)
	}
}

// risk with a degraded source portfolio: exit 0, degraded_dexs on the envelope
// and inside the view data.
func TestRiskRunEDegradedSubDexEnvelope(t *testing.T) {
	srv := rdWithSubDex(t)
	srv.fail("clearinghouseState@xyz")
	env, err := rdRun(t, riskCmd, nil)
	rdAssertOK(t, env, err, "risk")
	if len(env.DegradedDexs) != 1 || env.DegradedDexs[0] != "xyz" {
		t.Fatalf("risk envelope must carry degraded_dexs, got %v", env.DegradedDexs)
	}
	if !strings.Contains(string(env.Data), `"degraded_dexs":["xyz"]`) {
		t.Fatalf("risk view must carry degraded_dexs, got %s", env.Data)
	}
}

// snapshot — the documented once-per-tick read — with an unreadable sub-dex
// clearinghouse: TOOLS.md promises the TOP-LEVEL degraded_dexs envelope field on
// snapshot too, not just the copy nested in data.portfolio.data. An agent keying
// on envelope.degraded_dexs (as documented) must see the degradation here.
func TestSnapshotRunEDegradedSubDexEnvelope(t *testing.T) {
	srv := rdWithSubDex(t)
	srv.fail("clearinghouseState@xyz")
	env, err := rdRun(t, snapshotCmd, nil)
	rdAssertOK(t, env, err, "snapshot")
	if len(env.DegradedDexs) != 1 || env.DegradedDexs[0] != "xyz" {
		t.Fatalf("snapshot envelope must carry top-level degraded_dexs (the TOOLS.md contract), got %v", env.DegradedDexs)
	}
	if !strings.Contains(rdWarningsJoined(env), "sub-dex state (xyz)") {
		t.Fatalf("degradation must be warned, got %v", env.Warnings)
	}
	if !strings.Contains(string(env.Data), `"degraded_dexs":["xyz"]`) {
		t.Fatalf("the embedded portfolio section must also carry degraded_dexs, got %s", env.Data)
	}
}

// funding whose pagination cannot complete: exit 0, truncated:true + warning.
// (The fake server returns the same full 500-row page for every startTime, so
// the cursor cannot advance — the read keeps the first page, discards the
// duplicate, and reports truncation.)
func TestFundingRunETruncatedEnvelope(t *testing.T) {
	srv := rdSetup(t)
	var rows []string
	for i := 1; i <= 500; i++ {
		rows = append(rows, `{"delta":{"coin":"BTC","fundingRate":"0.0001","size":"1","type":"funding","usdc":"0.1"},"hash":"0x","time":`+strconv.Itoa(i)+`}`)
	}
	srv.setResp("userFunding", "["+strings.Join(rows, ",")+"]")
	rSince = 1
	env, err := rdRun(t, fundingCmd, nil)
	rdAssertOK(t, env, err, "funding")
	if !env.Truncated {
		t.Fatalf("an incomplete paged read must set envelope truncated, got %+v", env)
	}
	joined := rdWarningsJoined(env)
	if !strings.Contains(joined, "incomplete") && !strings.Contains(joined, "truncated") {
		t.Fatalf("truncation must be warned, got %v", env.Warnings)
	}
}
