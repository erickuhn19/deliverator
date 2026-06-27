package core

// Coverage for the #91 core hardening: Halted() fail-closed (S9) and meta-cache
// timestamp + symlink integrity (S11).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/erickuhn19/deliverator/internal/config"
	hl "github.com/erickuhn19/deliverator/internal/hl"
)

// --- S9: Halted fails closed ---

func TestHaltedFalseWhenAbsent(t *testing.T) {
	testHome(t)
	if (&Client{}).Halted() {
		t.Fatal("Halted() must be false when the halt file is absent")
	}
}

func TestHaltedFailsClosedOnStatError(t *testing.T) {
	// Point DELIVERATOR_HOME at a regular FILE so config.Dir() is a non-dir and
	// os.Stat(haltPath()) returns ENOTDIR — a non-IsNotExist error that must be
	// treated as halted (fail-closed), never as "no halt".
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DELIVERATOR_HOME", f)
	if !(&Client{}).Halted() {
		t.Fatal("Halted() must fail closed on a non-not-exist stat error")
	}
}

// --- S11: meta cache timestamp + symlink integrity ---

func metaWithTime(at time.Time) *MetaStore {
	return NewMetaStore("testnet",
		&hl.Meta{Universe: []hl.AssetInfo{{Name: "BTC", SzDecimals: 5}}},
		&hl.SpotMeta{}, at)
}

func TestLoadMetaCacheRejectsFutureTimestamp(t *testing.T) {
	testHome(t)
	path := filepath.Join(t.TempDir(), "meta.json")
	if err := metaWithTime(time.Now().Add(48 * time.Hour)).Save(path); err != nil {
		t.Fatal(err)
	}
	// A future FetchedAt makes Age() negative, so a poisoned cache would never
	// expire — it must be rejected outright.
	if _, ok := LoadMetaCache(path, "testnet"); ok {
		t.Fatal("a future FetchedAt must be rejected")
	}
}

func TestLoadMetaCacheRejectsAncientTimestamp(t *testing.T) {
	testHome(t)
	path := filepath.Join(t.TempDir(), "meta.json")
	if err := metaWithTime(time.Now().Add(-400 * 24 * time.Hour)).Save(path); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadMetaCache(path, "testnet"); ok {
		t.Fatal("a >1y-old FetchedAt must be rejected as corrupt")
	}
}

func TestLoadMetaCacheAcceptsRecent(t *testing.T) {
	testHome(t)
	path := filepath.Join(t.TempDir(), "meta.json")
	if err := metaWithTime(time.Now().Add(-time.Minute)).Save(path); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadMetaCache(path, "testnet"); !ok {
		t.Fatal("a recent FetchedAt must still load")
	}
}

func TestLoadMetaCacheRefusesSymlink(t *testing.T) {
	testHome(t)
	dir := t.TempDir()
	real := filepath.Join(dir, "real-meta.json")
	if err := metaWithTime(time.Now()).Save(real); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "meta.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	// A symlinked cache path is the poisoning vector — refuse it.
	if _, ok := LoadMetaCache(link, "testnet"); ok {
		t.Fatal("LoadMetaCache must refuse a symlinked cache path")
	}
}

func TestLoadMetaCacheRejectsExactly365Days(t *testing.T) {
	testHome(t)
	path := filepath.Join(t.TempDir(), "meta.json")
	if err := metaWithTime(time.Now().Add(-365 * 24 * time.Hour)).Save(path); err != nil {
		t.Fatal(err)
	}
	// The boundary itself is stale (>= 365d), not "just barely fresh".
	if _, ok := LoadMetaCache(path, "testnet"); ok {
		t.Fatal("a cache exactly 365 days old must be rejected (>=, not >)")
	}
}

// --- S8: core InfoPost body cap ---

func TestInfoPostRejectsOversizedBody(t *testing.T) {
	big := strings.Repeat("a", maxInfoBodyBytes+1)
	c, ctx := newTestClient(t, config.Default(), Options{}, func(_, _ string, _ map[string]any) (int, string) {
		return 200, big
	})
	var out map[string]any
	err := c.InfoPost(ctx, map[string]any{"type": "x"}, &out)
	if err == nil {
		t.Fatal("InfoPost must fail closed on an oversized /info body")
	}
	if !strings.Contains(err.Error(), "limit") && !strings.Contains(err.Error(), "too_large") {
		t.Fatalf("error should name the cap, got: %v", err)
	}
}

// --- S10: GenCloid error propagation ---
//
// We do NOT fault-inject the RNG: on Go 1.24+ crypto/rand.Read crashes the
// process (runtime.fatal) on a Reader failure instead of returning an error, so
// the error branch can't be exercised by swapping rand.Reader without killing
// the test binary. The (string, error) signature is forward/backward-compatible
// defense; the happy path is covered by TestGenCloidFormat.

// --- T3-keybind: detection + surfacing ---

func TestSignerWarnFor(t *testing.T) {
	const master = "0x9ccAcA47f0318FaeF9C8175767a15AEe1586177e"
	// A real agent wallet has a different address -> no warning.
	if w := signerWarnFor(master, "0x1111111111111111111111111111111111111111"); w != "" {
		t.Errorf("distinct agent address must NOT warn, got %q", w)
	}
	// Loaded the master key itself (case-insensitive) -> warn.
	if w := signerWarnFor(master, strings.ToLower(master)); w == "" {
		t.Error("loading the master key as the agent must warn (withdrawal-capable)")
	}
	// No master configured -> nothing to compare, no warning.
	if w := signerWarnFor("", master); w != "" {
		t.Errorf("no master configured must NOT warn, got %q", w)
	}
}

func TestSignerWarningsSurface(t *testing.T) {
	c := &Client{}
	if c.signerWarnings() != nil {
		t.Error("no warning -> nil")
	}
	c.signerWarn = "danger"
	w := c.signerWarnings()
	if len(w) != 1 || w[0] != "danger" {
		t.Fatalf("signerWarnings must surface the stored warning, got %v", w)
	}
}

// --- T3-flatten: panic step-4 re-verifies outcome holdings ---

func TestPanicVerifiesOutcomeHoldings(t *testing.T) {
	resp := func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" {
			switch typ {
			case "frontendOpenOrders":
				return 200, "[]"
			case "clearinghouseState":
				return 200, emptyState
			case "spotClearinghouseState":
				return 200, `[1]` // not an object -> SpotUserState read fails
			case "webData2":
				return 200, `{"twapStates":[]}`
			}
			return 200, "{}"
		}
		return 200, defaultEx
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	c.Meta().AddOutcomes(&hl.OutcomeMeta{Outcomes: []hl.OutcomeInfo{
		{Outcome: 641, SideSpecs: []hl.OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}, QuoteToken: "USDC"},
	}})
	res, err := c.Panic(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// With outcomes enabled, a failed spot re-read in step 4 must degrade the
	// result — the flatten can't be confirmed complete.
	if res.Complete {
		t.Fatalf("an outcome spot-read failure must mark panic NOT complete: %+v", res)
	}
	found := false
	for _, d := range res.Degraded {
		if d == "outcomes" {
			found = true
		}
	}
	if !found {
		t.Errorf("Degraded must name 'outcomes', got %v", res.Degraded)
	}
}
