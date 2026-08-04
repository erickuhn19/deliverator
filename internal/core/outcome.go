package core

import (
	"fmt"
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
	m.outcomeMeta = om

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
		id   int
		name string
	}
	qByOutcome := make(map[int]qref)
	settled := make(map[int]bool)
	for _, q := range om.Questions {
		ref := qref{id: q.Question, name: q.Name}
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
		for side := 0; side < len(o.SideSpecs) && side <= 1; side++ {
			coin := hl.OutcomeCoin(o.Outcome, side)
			label := o.SideSpecs[side].Name // "Yes" / "No"
			mk := Market{
				Coin:             coin,
				Class:            "outcome",
				AssetIndex:       hl.OutcomeAsset(o.Outcome, side),
				SzDecimals:       0,
				PxDecimals:       MaxDecimalsOutcome,
				IsOutcome:        true,
				Outcome:          o.Outcome,
				Side:             label,
				Title:            outcomeTitle(o, q.name, label, und, target, expiry),
				Question:         q.id,
				QuestionName:     q.name,
				Underlying:       und,
				TargetPrice:      target,
				Expiry:           expiry,
				ResolutionStatus: status,
				PriceBound:       "0..1",
				QuoteToken:       o.QuoteToken,
			}
			key := strings.ToUpper(coin)
			m.byCoin[key] = mk
			m.outcomeCoins = append(m.outcomeCoins, key)
			m.outcomeMarkets = append(m.outcomeMarkets, mk)
		}
	}
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
