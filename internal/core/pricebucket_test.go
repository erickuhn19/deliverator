package core

import (
	"strings"
	"testing"

	hl "github.com/erickuhn19/deliverator/internal/hl"
)

// Tests for #42. `deliverator markets` rendered every leg of a live BTC range
// market as an identical "Recurring: Recurring Named Outcome — Yes", which made
// three DIFFERENT bets indistinguishable and a tradable market effectively
// invisible.

// liveQ165 is the mainnet payload captured 2026-08-04 (question 165), verbatim
// except for the field names. The testnet twin (question 926) has the same shape
// with different thresholds.
func liveQ165() *hl.OutcomeMeta {
	sides := []hl.OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}
	return &hl.OutcomeMeta{
		Outcomes: []hl.OutcomeInfo{
			{Outcome: 1005, Name: "Recurring Fallback", Description: "other", SideSpecs: sides, QuoteToken: "USDC"},
			{Outcome: 1006, Name: "Recurring Named Outcome", Description: "index:0", SideSpecs: sides, QuoteToken: "USDC"},
			{Outcome: 1007, Name: "Recurring Named Outcome", Description: "index:1", SideSpecs: sides, QuoteToken: "USDC"},
			{Outcome: 1008, Name: "Recurring Named Outcome", Description: "index:2", SideSpecs: sides, QuoteToken: "USDC"},
		},
		Questions: []hl.OutcomeQuestion{{
			Question: 165, Name: "Recurring",
			Description:     "class:priceBucket|underlying:BTC|expiry:20260805-0600|priceThresholds:62506,65057|period:1d",
			FallbackOutcome: 1005,
			NamedOutcomes:   []int{1006, 1007, 1008},
		}},
	}
}

// THE MONEY ASSERTION. Two thresholds define three buckets, and each leg must be
// labelled with the RIGHT one. Mislabelling would have an agent buy "above" when
// the coin is really "below". Verified against live pricing: with BTC at 63,819
// the middle bucket (#10070) traded near 0.81 while #10060/#10080 sat near 0.22
// and 0.10.
func TestPriceBucketTitlesNameTheRightRange(t *testing.T) {
	ms := testMeta()
	ms.AddOutcomes(liveQ165())

	for _, tc := range []struct {
		coin, want string
		low, high  string
		idx        int
	}{
		{"#10060", "BTC below 62506 by 2026-08-05 06:00Z — Yes", "", "62506", 0},
		{"#10070", "BTC in 62506-65057 by 2026-08-05 06:00Z — Yes", "62506", "65057", 1},
		{"#10080", "BTC above 65057 by 2026-08-05 06:00Z — Yes", "65057", "", 2},
	} {
		mk, ok := ms.Lookup(tc.coin)
		if !ok {
			t.Fatalf("%s should resolve", tc.coin)
		}
		if mk.Title != tc.want {
			t.Errorf("%s title = %q, want %q", tc.coin, mk.Title, tc.want)
		}
		if mk.BucketIndex == nil || *mk.BucketIndex != tc.idx {
			t.Errorf("%s bucket_index = %v, want %d", tc.coin, mk.BucketIndex, tc.idx)
		}
		if mk.BucketLow != tc.low || mk.BucketHigh != tc.high {
			t.Errorf("%s bounds = (%q,%q), want (%q,%q)", tc.coin, mk.BucketLow, mk.BucketHigh, tc.low, tc.high)
		}
	}

	// The three legs must no longer share one label.
	seen := map[string]bool{}
	for _, c := range []string{"#10060", "#10070", "#10080"} {
		mk, _ := ms.Lookup(c)
		if seen[mk.Title] {
			t.Fatalf("two legs still share the title %q — the market is indistinguishable", mk.Title)
		}
		seen[mk.Title] = true
	}
}

// The No side reads as the same range, opposite side.
func TestPriceBucketNoSide(t *testing.T) {
	ms := testMeta()
	ms.AddOutcomes(liveQ165())
	mk, ok := ms.Lookup("#10071")
	if !ok {
		t.Fatal("#10071 (No leg) should resolve")
	}
	if want := "BTC in 62506-65057 by 2026-08-05 06:00Z — No"; mk.Title != want {
		t.Errorf("title = %q, want %q", mk.Title, want)
	}
}

// The fallback leg says what it actually resolves on.
func TestPriceBucketFallbackLeg(t *testing.T) {
	ms := testMeta()
	ms.AddOutcomes(liveQ165())
	mk, _ := ms.Lookup("#10050")
	if !mk.IsFallback {
		t.Error("#10050 should be marked is_fallback")
	}
	if !strings.Contains(mk.Title, "none of the 3 BTC ranges") {
		t.Errorf("fallback title = %q, want it to name what a Yes means", mk.Title)
	}
	if mk.BucketIndex != nil {
		t.Errorf("the fallback is not a bucket; bucket_index = %v", mk.BucketIndex)
	}
}

// Question shape is exposed so an agent can discover a new class at runtime.
func TestPriceBucketExposesQuestionShape(t *testing.T) {
	ms := testMeta()
	ms.AddOutcomes(liveQ165())
	mk, _ := ms.Lookup("#10070")
	if mk.QuestionClass != "priceBucket" || mk.QuestionPeriod != "1d" {
		t.Errorf("class/period = %q/%q, want priceBucket/1d", mk.QuestionClass, mk.QuestionPeriod)
	}
	if strings.Join(mk.PriceThresholds, ",") != "62506,65057" {
		t.Errorf("thresholds = %v", mk.PriceThresholds)
	}
	if mk.Underlying != "BTC" {
		t.Errorf("underlying = %q, want BTC", mk.Underlying)
	}
	if mk.Expiry != "2026-08-05 06:00Z" {
		t.Errorf("expiry = %q", mk.Expiry)
	}
}

// REFUSE TO GUESS. Every one of these is a question whose structure does not
// check out; each must fall back to the legacy title rather than label a bucket
// it cannot confirm. A wrong label here is a money bug.
func TestPriceBucketRefusesUnverifiableShapes(t *testing.T) {
	sides := []hl.OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}
	mk3 := func(d0, d1, d2 string) []hl.OutcomeInfo {
		return []hl.OutcomeInfo{
			{Outcome: 1006, Name: "Recurring Named Outcome", Description: d0, SideSpecs: sides},
			{Outcome: 1007, Name: "Recurring Named Outcome", Description: d1, SideSpecs: sides},
			{Outcome: 1008, Name: "Recurring Named Outcome", Description: d2, SideSpecs: sides},
		}
	}
	for _, tc := range []struct {
		name  string
		desc  string
		outs  []hl.OutcomeInfo
		named []int
	}{
		{"thresholds not ascending", "class:priceBucket|underlying:BTC|priceThresholds:65057,62506|period:1d",
			mk3("index:0", "index:1", "index:2"), []int{1006, 1007, 1008}},
		{"duplicate thresholds", "class:priceBucket|underlying:BTC|priceThresholds:62506,62506|period:1d",
			mk3("index:0", "index:1", "index:2"), []int{1006, 1007, 1008}},
		{"non-numeric threshold", "class:priceBucket|underlying:BTC|priceThresholds:62506,abc|period:1d",
			mk3("index:0", "index:1", "index:2"), []int{1006, 1007, 1008}},
		{"leg count disagrees with bucket count", "class:priceBucket|underlying:BTC|priceThresholds:62506,65057,70000|period:1d",
			mk3("index:0", "index:1", "index:2"), []int{1006, 1007, 1008}},
		{"two legs claim the same bucket", "class:priceBucket|underlying:BTC|priceThresholds:62506,65057|period:1d",
			mk3("index:0", "index:1", "index:1"), []int{1006, 1007, 1008}},
		{"index out of range", "class:priceBucket|underlying:BTC|priceThresholds:62506,65057|period:1d",
			mk3("index:0", "index:1", "index:9"), []int{1006, 1007, 1008}},
		{"leg missing its index", "class:priceBucket|underlying:BTC|priceThresholds:62506,65057|period:1d",
			mk3("index:0", "index:1", "other"), []int{1006, 1007, 1008}},
		{"no underlying", "class:priceBucket|priceThresholds:62506,65057|period:1d",
			mk3("index:0", "index:1", "index:2"), []int{1006, 1007, 1008}},
		{"absurd underlying", "class:priceBucket|underlying:" + strings.Repeat("A", 3000) + "|priceThresholds:62506,65057|period:1d",
			mk3("index:0", "index:1", "index:2"), []int{1006, 1007, 1008}},
		{"no thresholds", "class:priceBucket|underlying:BTC|period:1d",
			mk3("index:0", "index:1", "index:2"), []int{1006, 1007, 1008}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms := testMeta()
			ms.AddOutcomes(&hl.OutcomeMeta{
				Outcomes: tc.outs,
				Questions: []hl.OutcomeQuestion{{
					Question: 165, Name: "Recurring", Description: tc.desc, NamedOutcomes: tc.named,
				}},
			})
			mk, ok := ms.Lookup("#10060")
			if !ok {
				t.Fatal("the leg must still resolve and trade — only the LABEL degrades")
			}
			if mk.BucketIndex != nil {
				t.Errorf("bucket_index was set for an unverifiable question: %v", mk.BucketIndex)
			}
			if strings.Contains(mk.Title, " in ") || strings.Contains(mk.Title, " below ") || strings.Contains(mk.Title, " above ") {
				t.Errorf("labelled a bucket it could not verify: %q", mk.Title)
			}
		})
	}
}

// An unknown future class must degrade to today's behaviour, never error, and
// still surface the class so an agent can notice something new exists.
func TestUnknownQuestionClassDegradesGracefully(t *testing.T) {
	ms := testMeta()
	ms.AddOutcomes(&hl.OutcomeMeta{
		Outcomes: []hl.OutcomeInfo{{
			Outcome: 1006, Name: "Some Leg", Description: "index:0",
			SideSpecs: []hl.OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}},
		}},
		Questions: []hl.OutcomeQuestion{{
			Question: 900, Name: "Novel",
			Description:   "class:priceLadder|underlying:BTC|rungs:1,2,3|period:4h",
			NamedOutcomes: []int{1006},
		}},
	})
	mk, ok := ms.Lookup("#10060")
	if !ok {
		t.Fatal("an unknown class must still produce a tradable market")
	}
	if mk.QuestionClass != "priceLadder" || mk.QuestionPeriod != "4h" {
		t.Errorf("class/period = %q/%q — the raw class must pass through for discovery", mk.QuestionClass, mk.QuestionPeriod)
	}
	if mk.BucketIndex != nil {
		t.Errorf("an unknown class must not be mapped to buckets: %v", mk.BucketIndex)
	}
	if mk.Title == "" {
		t.Error("title must not be empty")
	}
}

// EXPIRY IS A RISK-GATE INPUT, not display text: a non-empty unparseable value
// makes the outcome settlement gate reject. A malformed expiry must therefore
// leave it empty rather than pass the raw string through.
func TestMalformedExpiryIsDroppedNotPassedThrough(t *testing.T) {
	ms := testMeta()
	ms.AddOutcomes(&hl.OutcomeMeta{
		Outcomes: []hl.OutcomeInfo{{
			Outcome: 1006, Name: "Recurring Named Outcome", Description: "index:0",
			SideSpecs: []hl.OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}},
		}},
		Questions: []hl.OutcomeQuestion{{
			Question: 165, Name: "Recurring",
			Description:   "class:priceBucket|underlying:BTC|expiry:NOT-A-DATE|priceThresholds:62506|period:1d",
			NamedOutcomes: []int{1006},
		}},
	})
	mk, _ := ms.Lookup("#10060")
	if mk.Expiry != "" {
		t.Errorf("expiry = %q, want empty — an unparseable expiry would make the settlement gate reject", mk.Expiry)
	}
	if _, ok := parseOutcomeExpiryTime(mk.Expiry); ok {
		t.Error("an empty expiry must not parse")
	}
}

// The pre-existing priceBinary path must be untouched.
func TestPriceBinaryStillRendersAsBefore(t *testing.T) {
	ms := testMeta()
	ms.AddOutcomes(&hl.OutcomeMeta{Outcomes: []hl.OutcomeInfo{{
		Outcome: 1001, Name: "Recurring",
		Description: "class:priceBinary|underlying:BTC|expiry:20260805-0600|targetPrice:63782|period:1d",
		SideSpecs:   []hl.OutcomeSideSpec{{Name: "Yes"}, {Name: "No"}}, QuoteToken: "USDC",
	}}})
	mk, _ := ms.Lookup("#10010")
	if want := "BTC above 63782 by 2026-08-05 06:00Z — Yes"; mk.Title != want {
		t.Errorf("priceBinary title = %q, want %q", mk.Title, want)
	}
	if mk.BucketIndex != nil {
		t.Errorf("priceBinary must not be bucket-mapped: %v", mk.BucketIndex)
	}
}
