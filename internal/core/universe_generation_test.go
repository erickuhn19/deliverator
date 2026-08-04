package core

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	hl "github.com/erickuhn19/deliverator/internal/hl"
)

// Tests for #45. Three multi-hour incidents (#37, #41, #43) were invisible the
// same way: the process kept answering confidently from a snapshot that nothing
// in its output identified. On 2026-08-04 a server rejected every order with
// "coin #10070 not found in info" for ~13 hours — true, unhelpful, and identical
// whether the coin was bogus or the process was a roll behind.

// The fingerprint must change when the universe ROLLS and not merely because a
// refresh re-ran. A stamp that churned on every reload would be noise; one that
// did not change on a roll would be a lie.
func TestUniverseStampChangesOnARollNotOnARefresh(t *testing.T) {
	ms := testMeta()
	ms.AddOutcomes(rollUniverse(1000))
	first := ms.UniverseStamp()

	ms.AddOutcomes(rollUniverse(1000)) // same universe, reloaded
	same := ms.UniverseStamp()
	if same.Fingerprint != first.Fingerprint {
		t.Errorf("fingerprint changed on a no-op refresh: %s -> %s", first.Fingerprint, same.Fingerprint)
	}
	if same.Outcomes != first.Outcomes {
		t.Errorf("leg count drifted on a no-op refresh: %d -> %d", first.Outcomes, same.Outcomes)
	}

	ms.AddOutcomes(rollUniverse(2000)) // the daily roll
	rolled := ms.UniverseStamp()
	if rolled.Fingerprint == first.Fingerprint {
		t.Error("fingerprint did NOT change across a roll — a stale universe would look identical to a fresh one")
	}
}

// The stamp must be readable by a human comparing it against "when did the roll
// happen".
func TestUniverseStampRendersReadably(t *testing.T) {
	ms := testMeta()
	ms.AddOutcomes(rollUniverse(1000))
	got := ms.UniverseStamp().String()
	for _, want := range []string{"universe fetched", "outcome legs", "fp "} {
		if !strings.Contains(got, want) {
			t.Errorf("stamp %q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "0001-01-01") {
		t.Errorf("stamp carries a zero time: %q", got)
	}
}

func TestUniverseStampReportsNotLoaded(t *testing.T) {
	ms := testMeta()
	if got := ms.UniverseStamp().String(); got != "universe not loaded" {
		t.Errorf("empty universe stamp = %q", got)
	}
}

// THE POINT OF THE WHOLE ISSUE. Reporting the meta store's view while the SIGNER
// holds a different one would have described the #43 outage as healthy: the read
// path resolved the coin, the order passed the gates, got signed, then died at
// exit 50. The generation must cross-check the two resolvers.
func TestUniverseGenerationDetectsASignerOutOfSync(t *testing.T) {
	testHome(t)
	c, _ := newClientAt(t, nil, Options{}, "http://127.0.0.1:1")

	// Day 1 installed in both resolvers, as exchange() does on the first write.
	c.meta.AddOutcomes(rollUniverse(1000))
	c.ex.Info().RegisterOutcomes(rollUniverse(1000))
	if got := c.UniverseGeneration(); !strings.Contains(got, "signer in sync") {
		t.Fatalf("with both resolvers agreeing, generation = %q, want it to report sync", got)
	}

	// The roll lands in the meta store ONLY — precisely the #43 shape.
	c.meta.AddOutcomes(rollUniverse(2000))
	got := c.UniverseGeneration()
	if !strings.Contains(got, "SIGNER OUT OF SYNC") {
		t.Errorf("generation = %q; a signer a roll behind must be called out, not reported as healthy", got)
	}
	if !strings.Contains(got, "exit 50") {
		t.Errorf("generation = %q; it should say what the failure will look like", got)
	}
	if !strings.Contains(got, "#20010") {
		t.Errorf("generation = %q; it should name a coin that does not resolve for signing", got)
	}

	// Teaching the signer the same universe clears it.
	c.ex.Info().RegisterOutcomes(rollUniverse(2000))
	if got := c.UniverseGeneration(); !strings.Contains(got, "signer in sync") {
		t.Errorf("after re-registering, generation = %q, want sync restored", got)
	}
}

// Before the first write there is no signer. Say so honestly rather than
// claiming agreement that was never checked.
func TestUniverseGenerationReportsNoSignerYet(t *testing.T) {
	ms := testMeta()
	ms.AddOutcomes(rollUniverse(1000))
	c := &Client{meta: ms}
	if got := c.UniverseGeneration(); !strings.Contains(got, "signer not yet built") {
		t.Errorf("generation = %q, want it to report that no signer exists yet", got)
	}
}

// The real refresh path must leave the two resolvers in sync, which is the #43
// fix — pinned here from the generation's point of view.
func TestRefreshOutcomesLeavesGenerationInSync(t *testing.T) {
	testHome(t)
	day := 1000
	srv := newOutcomeMetaServer(t, func() *hl.OutcomeMeta { return rollUniverse(day) })
	defer srv.Close()

	c, ctx := newClientAt(t, nil, Options{}, srv.URL)
	if err := c.RefreshOutcomes(ctx); err != nil {
		t.Fatal(err)
	}
	c.ex.Info().RegisterOutcomes(c.meta.OutcomeMeta()) // what exchange() does once

	day = 2000
	if err := c.RefreshOutcomes(ctx); err != nil {
		t.Fatal(err)
	}
	if got := c.UniverseGeneration(); !strings.Contains(got, "signer in sync") {
		t.Errorf("after a real RefreshOutcomes the generation = %q, want sync", got)
	}
}

// The stamp is read under the same lock everything else is; make sure it does not
// deadlock or race against a reload. Run under -race.
func TestUniverseStampIsConcurrencySafe(t *testing.T) {
	ms := testMeta()
	ms.AddOutcomes(rollUniverse(1000))
	done := make(chan struct{}, 2)
	go func() {
		for i := 0; i < 300; i++ {
			ms.AddOutcomes(rollUniverse(1000))
		}
		done <- struct{}{}
	}()
	go func() {
		for i := 0; i < 300; i++ {
			_ = ms.UniverseStamp()
			_ = ms.OutcomeCoins()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
	_ = time.Now()
}

// newOutcomeMetaServer serves outcomeMeta from a supplier so a test can roll the
// universe between calls, and answers the other /info reads the client touches.
func newOutcomeMetaServer(t *testing.T, next func() *hl.OutcomeMeta) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Type string `json:"type"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Type == "outcomeMeta" {
			_ = json.NewEncoder(w).Encode(next())
			return
		}
		serveInfo(w, body.Type)
	}))
}
