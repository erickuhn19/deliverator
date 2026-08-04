package core

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	hl "github.com/erickuhn19/deliverator/internal/hl"
)

// AddOutcomes indexes HIP-4 outcome markets (binary Yes/No leaves). Each side
// resolves and is placeable as "#<encoding>" (asset id hl.OutcomeAsset, integer
// sizes, (0,1) probability prices via IsOutcome). They populate the coin->Market
// lookup AND a separate outcome list surfaced by `markets --class outcome` — kept
// out of the default `markets` listing because they number in the hundreds and
// rotate daily. Rich fields (Yes/No side, the question grouping, parsed priceBinary
// underlying/target/expiry, resolution status) let an agent discover and reason
// about them.
// It is IDEMPOTENT and safe to call repeatedly: `serve` reloads the universe on
// the daily roll (RefreshOutcomes), and an append-only writer duplicated every
// row on each reload while leaving rolled-out coins resolvable forever (#43).
// Each call installs exactly the universe it was given.
func (m *MetaStore) AddOutcomes(om *hl.OutcomeMeta) {
	if om == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addOutcomesLocked(om)
}

// addOutcomesLocked is AddOutcomes's body. Callers must hold m.mu for writing.
func (m *MetaStore) addOutcomesLocked(om *hl.OutcomeMeta) {
	if om == nil {
		return
	}
	m.outcomeMeta = om
	m.outcomeFetchedAt = time.Now()

	// Retire the previous universe. Coins that rolled out must stop resolving:
	// leaving them in byCoin lets an order price against a market that no longer
	// trades, and the stale asset id would sign for the wrong leaf.
	for _, key := range m.outcomeCoins {
		delete(m.byCoin, key)
	}
	m.outcomeCoins = m.outcomeCoins[:0]
	m.outcomeMarkets = nil

	// Map each outcome to its grouping question, and collect settled outcome ids.
	type qref struct {
		id       int
		name     string
		spec     questionSpec
		plan     bucketPlan
		fallback int
	}
	byID := make(map[int]hl.OutcomeInfo, len(om.Outcomes))
	for _, o := range om.Outcomes {
		byID[o.Outcome] = o
	}
	qByOutcome := make(map[int]qref)
	settled := make(map[int]bool)
	for _, q := range om.Questions {
		// The CLASS lives on the QUESTION, not on its legs: a priceBucket leg's own
		// description is just "index:N", which is why these markets rendered as an
		// indistinguishable "Recurring: Recurring Named Outcome — Yes" (#42).
		spec := parseQuestionSpec(q.Description)
		ref := qref{
			id: q.Question, name: q.Name, spec: spec,
			plan:     planPriceBuckets(q, spec, byID),
			fallback: q.FallbackOutcome,
		}
		for _, oid := range q.NamedOutcomes {
			qByOutcome[oid] = ref
		}
		if q.FallbackOutcome != 0 {
			qByOutcome[q.FallbackOutcome] = ref
		}
		for _, oid := range q.SettledNamedOutcomes {
			settled[oid] = true
		}
	}

	for _, o := range om.Outcomes {
		und, target, expiry := parseOutcomeDescription(o.Description)
		q := qByOutcome[o.Outcome]
		status := "open"
		if settled[o.Outcome] {
			status = "settled"
		}
		// Question-level shape is surfaced whenever it parsed, INDEPENDENT of
		// whether the bucket mapping verified: expiry feeds the settlement gate and
		// the class/period/thresholds feed discovery, and withholding them because a
		// LABELLING check failed would trade a display problem for a safety one.
		if und == "" && q.spec.Underlying != "" && len(q.spec.Underlying) <= maxUnderlyingLen {
			und = q.spec.Underlying
		}
		if expiry == "" {
			expiry = q.spec.Expiry // already round-trip validated in parseQuestionSpec
		}

		// Bucket placement is set ONLY when the question's structure verified.
		bucketIdx, bucketLow, bucketHigh := -1, "", ""
		if q.plan.ok {
			if k, ok := q.plan.byIndex[o.Outcome]; ok {
				bucketIdx = k
				bucketLow, bucketHigh = bucketBounds(q.plan.edges, k)
			}
		}
		isFallback := q.fallback != 0 && o.Outcome == q.fallback

		for side := 0; side < len(o.SideSpecs) && side <= 1; side++ {
			coin := hl.OutcomeCoin(o.Outcome, side)
			label := o.SideSpecs[side].Name // "Yes" / "No"
			title := outcomeTitle(o, q.name, label, und, target, expiry)
			switch {
			case bucketIdx >= 0:
				title = bucketTitle(q.spec.Underlying, bucketLow, bucketHigh, expiry, label)
			case isFallback && q.plan.ok:
				title = fmt.Sprintf("%s: none of the %d %s ranges — %s",
					q.name, len(q.plan.edges)+1, q.spec.Underlying, label)
			}
			mk := Market{
				Coin:             coin,
				Class:            "outcome",
				AssetIndex:       hl.OutcomeAsset(o.Outcome, side),
				SzDecimals:       0,
				PxDecimals:       MaxDecimalsOutcome,
				IsOutcome:        true,
				Outcome:          o.Outcome,
				Side:             label,
				Title:            title,
				Question:         q.id,
				QuestionName:     q.name,
				Underlying:       und,
				TargetPrice:      target,
				Expiry:           expiry,
				ResolutionStatus: status,
				PriceBound:       "0..1",
				QuoteToken:       o.QuoteToken,
				QuestionClass:    q.spec.Class,
				QuestionPeriod:   q.spec.Period,
				PriceThresholds:  q.spec.Thresholds,
				BucketLow:        bucketLow,
				BucketHigh:       bucketHigh,
				IsFallback:       isFallback,
			}
			if bucketIdx >= 0 {
				k := bucketIdx
				mk.BucketIndex = &k
			}
			key := strings.ToUpper(coin)
			m.byCoin[key] = mk
			m.outcomeCoins = append(m.outcomeCoins, key)
			m.outcomeMarkets = append(m.outcomeMarkets, mk)
		}
	}
}

// ---- HIP-4 question descriptions (#42) ----

// maxBucketThresholds caps how many bucket edges a question may declare. The
// description is exchange-controlled text on a money path: an unbounded count
// would let a malformed (or hostile) payload fan one question into arbitrarily
// many markets.
const maxBucketThresholds = 32

// maxUnderlyingLen bounds the underlying symbol before it is interpolated into a
// Title that propagates into position/ctx views. Live symbols are 3-5 chars; the
// longest observed outcome description is ~1.7KB, so an unbounded field would
// put kilobytes of venue-chosen text into every rendered row.
const maxUnderlyingLen = 32

// questionSpec is a parsed pipe-delimited question description, e.g.
// "class:priceBucket|underlying:BTC|expiry:20260805-0600|priceThresholds:62506,65057|period:1d".
// Unknown keys are ignored and an unknown class is carried through verbatim, so
// a new class degrades to today's rendering instead of erroring.
type questionSpec struct {
	Class      string
	Underlying string
	Expiry     string // formatted + round-trip validated, or "" if it did not parse
	Thresholds []string
	Period     string
}

// parseQuestionSpec parses a pipe-delimited key:value description. It never
// errors: anything unrecognised yields a zero/partial spec and the caller falls
// back to the legacy rendering. This is untrusted exchange input.
func parseQuestionSpec(desc string) questionSpec {
	var q questionSpec
	if !strings.Contains(desc, "class:") {
		return q
	}
	for _, part := range strings.Split(desc, "|") {
		k, v, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "class":
			q.Class = v
		case "underlying":
			q.Underlying = v
		case "period":
			q.Period = v
		case "expiry":
			// Expiry is a RISK-GATE INPUT (the outcome settlement blackout), not
			// display text: formatOutcomeExpiry passes malformed input through
			// unchanged, and a non-empty unparseable expiry makes the gate reject.
			// Only keep a value that round-trips.
			if e := formatOutcomeExpiry(v); e != "" {
				if _, ok := parseOutcomeExpiryTime(e); ok {
					q.Expiry = e
				}
			}
		case "priceThresholds":
			for _, t := range strings.Split(v, ",") {
				if t = strings.TrimSpace(t); t != "" {
					q.Thresholds = append(q.Thresholds, t)
				}
			}
		}
	}
	return q
}

// bucketPlan is a VERIFIED mapping from a question's named outcomes to price
// buckets. ok is false whenever anything about the question's shape fails to
// check out, and the caller must then fall back to the legacy rendering.
type bucketPlan struct {
	ok       bool
	byIndex  map[int]int // outcome id -> bucket index
	fallback int         // fallback outcome id (0 = none)
	edges    []string
}

// planPriceBuckets verifies a priceBucket question and maps its legs to buckets.
//
// ORDERING IS A MONEY QUESTION, NOT A DISPLAY ONE. N thresholds define N+1
// buckets, and each named outcome carries "index:K" naming its bucket. Getting
// the mapping wrong would label a market backwards — an agent shorting "BTC
// above X" when the coin is really "BTC below X". So this VERIFIES the structure
// and refuses to label anything it cannot confirm:
//
//   - the class is exactly priceBucket
//   - 1..maxBucketThresholds edges, each a finite number, STRICTLY ASCENDING
//   - underlying is present and bounded
//   - exactly len(edges)+1 named outcomes
//   - every named outcome carries a distinct "index:K" with 0 <= K <= len(edges),
//     and the set of K covers 0..len(edges) exactly once
//
// Any failure returns ok=false and the legacy title is kept. Confirmed against
// live mainnet and testnet payloads (question 165 / 926: two thresholds, three
// named outcomes at index:0/1/2, plus a fallback described "other"), and
// corroborated by live pricing — with BTC inside the middle bucket, index:1 was
// the leg trading near 0.81 while the outer two sat near 0.22 and 0.10.
func planPriceBuckets(q hl.OutcomeQuestion, spec questionSpec, byID map[int]hl.OutcomeInfo) bucketPlan {
	var p bucketPlan
	if spec.Class != "priceBucket" {
		return p
	}
	if spec.Underlying == "" || len(spec.Underlying) > maxUnderlyingLen {
		return p
	}
	if n := len(spec.Thresholds); n == 0 || n > maxBucketThresholds {
		return p
	}
	prev := math.Inf(-1)
	for _, t := range spec.Thresholds {
		f, err := strconv.ParseFloat(t, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= prev {
			return p // unparseable, or not strictly ascending
		}
		prev = f
	}
	wantLegs := len(spec.Thresholds) + 1
	if len(q.NamedOutcomes) != wantLegs {
		return p // the leg count disagrees with the bucket count — do not guess
	}

	byIndex := make(map[int]int, wantLegs)
	seen := make(map[int]bool, wantLegs)
	for _, oid := range q.NamedOutcomes {
		o, ok := byID[oid]
		if !ok {
			return p // a leg we cannot inspect
		}
		raw, ok := strings.CutPrefix(strings.TrimSpace(o.Description), "index:")
		if !ok {
			return p
		}
		k, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || k < 0 || k >= wantLegs || seen[k] {
			return p // out of range, or two legs claiming the same bucket
		}
		seen[k] = true
		byIndex[oid] = k
	}
	if len(seen) != wantLegs {
		return p
	}
	return bucketPlan{ok: true, byIndex: byIndex, fallback: q.FallbackOutcome, edges: spec.Thresholds}
}

// bucketBounds returns the open interval for bucket k: ("", edges[0]) for the
// lowest, (edges[len-1], "") for the highest, (edges[k-1], edges[k]) between.
func bucketBounds(edges []string, k int) (low, high string) {
	if k > 0 {
		low = edges[k-1]
	}
	if k < len(edges) {
		high = edges[k]
	}
	return low, high
}

// bucketTitle renders one verified bucket leg, e.g.
// "BTC in 62506-65057 by 2026-08-05 06:00Z — Yes".
func bucketTitle(underlying string, low, high, expiry, side string) string {
	var base string
	switch {
	case low == "" && high != "":
		base = fmt.Sprintf("%s below %s", underlying, high)
	case low != "" && high == "":
		base = fmt.Sprintf("%s above %s", underlying, low)
	case low != "" && high != "":
		base = fmt.Sprintf("%s in %s-%s", underlying, low, high)
	default:
		return side
	}
	if expiry != "" {
		base += " by " + expiry
	}
	return base + " — " + side
}

// parseOutcomeDescription extracts (underlying, targetPrice, expiry) from a
// priceBinary outcome description, e.g.
// "class:priceBinary|underlying:BTC|expiry:20260625-0600|targetPrice:62857|period:1d".
// Non-priceBinary descriptions (plain-English events, "index:N" named legs) return
// empty strings — the description is class-dependent and parsed defensively.
func parseOutcomeDescription(desc string) (underlying, target, expiry string) {
	if !strings.Contains(desc, "class:priceBinary") {
		return "", "", ""
	}
	for _, part := range strings.Split(desc, "|") {
		k, v, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "underlying":
			underlying = strings.TrimSpace(v)
		case "targetPrice":
			target = strings.TrimSpace(v)
		case "expiry":
			expiry = formatOutcomeExpiry(strings.TrimSpace(v))
		}
	}
	return
}

// formatOutcomeExpiry turns "YYYYMMDD-HHMM" into "YYYY-MM-DD HH:MMZ" (settlement is
// UTC). An unexpected shape is returned unchanged.
func formatOutcomeExpiry(s string) string {
	if len(s) != 13 || s[8] != '-' {
		return s
	}
	return fmt.Sprintf("%s-%s-%s %s:%sZ", s[0:4], s[4:6], s[6:8], s[9:11], s[11:13])
}

// outcomeExpiryLayout parses the string formatOutcomeExpiry emits (UTC, minute res).
const outcomeExpiryLayout = "2006-01-02 15:04Z"

// parseOutcomeExpiryTime parses a Market.Expiry ("YYYY-MM-DD HH:MMZ") as a UTC
// instant; ok=false when it is empty or malformed. It is the core-side counterpart
// to mm.ParseExpiry (core cannot import internal/mm — that would be an import cycle).
func parseOutcomeExpiryTime(s string) (time.Time, bool) {
	t, err := time.Parse(outcomeExpiryLayout, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// outcomeTitle builds a human-readable description of what a Yes resolves on.
func outcomeTitle(o hl.OutcomeInfo, questionName, side, underlying, target, expiry string) string {
	switch {
	case underlying != "" && target != "":
		base := fmt.Sprintf("%s above %s", underlying, target)
		if expiry != "" {
			base += " by " + expiry
		}
		return base + " — " + side
	case questionName != "" && o.Name != "" && !strings.EqualFold(questionName, o.Name):
		return fmt.Sprintf("%s: %s — %s", questionName, o.Name, side)
	case o.Name != "":
		return o.Name + " — " + side
	case questionName != "":
		return questionName + " — " + side
	default:
		return side
	}
}
