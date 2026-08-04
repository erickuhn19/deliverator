package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
	"github.com/erickuhn19/deliverator/internal/state"
)

// The risk layer is enforced in core, before signing — switching the invocation
// surface cannot bypass it (§3.6, §6). The threat model is an LLM hallucinating
// a size, price, or leverage; treat every value as hostile until checked.

func haltPath() string     { return filepath.Join(config.Dir(), "halt") }
func ratePath() string     { return filepath.Join(config.Dir(), "rate.log") }
func rateLockPath() string { return filepath.Join(config.Dir(), "rate.lock") }

// Halted reports whether a global halt is active (§6). It fails CLOSED: a
// permission/IO error reading the halt file (anything other than "absent") is
// treated as halted, so a damaged or locked-down state dir never silently
// re-opens the order path (audit #91 / S9).
func (c *Client) Halted() bool {
	_, err := os.Stat(haltPath())
	if err == nil {
		return true
	}
	return !os.IsNotExist(err)
}

// SetHalt turns the global halt on/off.
func SetHalt(on bool) error {
	p := haltPath()
	if on {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		return state.WriteFileAtomic(p, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (c *Client) coinAllowed(coin string) bool {
	auto := c.automationConfig()
	if len(auto.AllowedCoins) == 0 {
		return true // empty allowlist = allow all (operator opts into lockdown)
	}
	for _, a := range auto.AllowedCoins {
		if strings.EqualFold(strings.TrimSpace(a), coin) {
			return true
		}
	}
	return false
}

// riskCheck carries the values a pre-trade gate evaluates.
type riskCheck struct {
	Coin string
	// IsMarket marks an order that EXECUTES at market — a plain market order OR
	// a trigger order with is_market=true, which fires as a market order
	// (possibly instantly if the trigger price is already crossed). Callers set
	// it accordingly so automation.limit_only cannot be bypassed via --trigger-market.
	IsMarket            bool
	NotionalUSD         float64
	PositionNotionalUSD float64 // resulting/current position notional for the coin
	MinNotionalUSD      float64 // floor for NEW exposure; 0 = no floor
	ReduceOnly          bool    // a reduce-only order cannot increase exposure
	Closing             bool    // a reductive exit (spot close): exempt from NEW-exposure guards, keep the floor
	HaltExempt          bool    // a Panic flatten leg: skips the halt gate (core-internal only, see OrderReq.panicFlatten)
}

// pricingGuardsActive reports whether any guard that needs a PRICED order is
// configured — the per-order notional floor/caps OR the account-wide portfolio
// gates. When true, a market/TWAP order with no available mid must fail CLOSED:
// it cannot be priced, so the guard could not be enforced and we refuse rather
// than slip through.
func (c *Client) pricingGuardsActive() bool {
	r := c.riskConfig()
	return r.MaxOrderNotionalUSD > 0 || r.MaxPositionNotionalUSD > 0 || r.MinOrderNotionalUSD > 0 ||
		c.portfolioGuardsActive()
}

// preTradeChecks runs every guard that must pass before an order is signed (§6).
func (c *Client) preTradeChecks(rc riskCheck) error {
	if err := c.staticChecks(rc); err != nil {
		return err
	}
	return c.checkRateCap()
}

// staticChecks runs the per-order guards that are independent of how many orders
// are in flight — halt, allowlist, limit-only, and the notional floor + caps. The
// rate cap is kept separate (see preTradeChecks) so a batch — one signed action —
// charges it ONCE rather than once per leg, which would self-trip mid-batch.
func (c *Client) staticChecks(rc riskCheck) error {
	guards := c.currentGuards()
	// Panic's internal flatten legs skip the halt gate: the emergency flatten
	// must work DURING a halt (see Panic). Every other write stays halt-gated.
	if !rc.HaltExempt && c.Halted() {
		return output.Halt("halted", "global halt is active — new orders rejected").
			WithHint("deliverator halt off  (to resume)")
	}
	// A close (reduce-only perp, or a spot exit) bypasses the NEW-exposure policy
	// guards — allowlist, limit-only, and the max caps — so a holding can always be
	// exited; it cannot grow exposure. (Perp market-close skips this gauntlet
	// entirely; this keeps the spot-close path consistent.)
	exit := rc.ReduceOnly || rc.Closing
	if !exit && !c.coinAllowed(rc.Coin) {
		return output.Risk("coin_not_allowed",
			"coin "+rc.Coin+" is not in automation.allowed_coins").
			WithHint("trade an allowed coin or add it to allowed_coins")
	}
	if rc.IsMarket && !exit && guards.automation.LimitOnly {
		return output.Risk("limit_only",
			"automation.limit_only is set — market orders (including trigger orders that execute as market) are blocked").
			WithHint("place a limit order with --limit/--alo, or a trigger-limit instead of --trigger-market")
	}
	// Notional caps bound NEW exposure. A reduce-only order can only shrink the
	// position, so it is exempt — otherwise a legitimate close/bracket gets
	// blocked by the very cap it would bring you back under.
	if !rc.ReduceOnly {
		// Min floor: HL rejects sub-minimum orders (~$10). Reject pre-flight with
		// a clear validation error. The NotionalUSD>0 guard is load-bearing: if a
		// price could not be determined notional is 0, and we must NOT min-reject
		// every order — callers fail closed before this point when a guard is active.
		if floor := rc.MinNotionalUSD; floor > 0 && rc.NotionalUSD > 0 && rc.NotionalUSD < floor {
			return output.Validation("min_order_notional",
				fmt.Sprintf("order notional $%.2f is below the $%.2f minimum", rc.NotionalUSD, floor)).
				WithHint(fmt.Sprintf("increase size so notional >= $%.2f (Hyperliquid rejects sub-minimum orders)", floor))
		}
		// The max caps bound NEW exposure only — a close (which the floor above still
		// guards against HL's sub-minimum reject) is exempt so it can always exit.
		if !rc.Closing {
			if cap := guards.risk.MaxOrderNotionalUSD; cap > 0 && rc.NotionalUSD > cap {
				return output.Risk("max_order_notional",
					fmt.Sprintf("order notional $%.2f exceeds cap $%.2f", rc.NotionalUSD, cap)).
					WithHint(fmt.Sprintf("reduce size so notional <= $%.2f", cap))
			}
			if cap := guards.risk.MaxPositionNotionalUSD; cap > 0 && rc.PositionNotionalUSD > cap {
				return output.Risk("max_position_notional",
					fmt.Sprintf("resulting position notional ~$%.2f (position + resting orders + this order) exceeds cap $%.2f", rc.PositionNotionalUSD, cap)).
					WithHint(fmt.Sprintf("reduce size or cancel resting orders in %s so position notional <= $%.2f", rc.Coin, cap))
			}
		}
	}
	return nil
}

// outcomeSettleGate is the core fail-closed backstop for HIP-4 settlement risk
// (spec §10). It refuses to OPEN or ADD outcome exposure on a market that has
// already settled, or — when risk.outcome_settle_blackout_mins is set — within that
// many minutes of expiry (the adverse-selection window where informed flow lifts a
// stale quote right before resolution). Exits (reduce-only / closing) are always
// allowed so a holding can be unwound, and non-outcome orders pass through
// untouched. It is enforced in the Place / PlaceBatch entry paths, before signing —
// so no invocation surface (CLI, the outcome-mm daemon, a hallucinating agent) can
// open fresh exposure into a resolving market.
func (c *Client) outcomeSettleGate(mk Market, req OrderReq) error {
	// Outcome tokens are long-only spot, so a Sell can only REDUCE a holding — it is
	// always de-risking. Exempt it (like ReduceOnly/Closing) so a plain `sell` can
	// unwind a position during the blackout; only Buys (opening/adding exposure) are gated.
	if req.ReduceOnly || req.Closing || (mk.IsOutcome && req.Side == Sell) {
		return nil
	}
	return c.outcomeSettleCheck(mk)
}

// outcomeSettleCheck is the settlement/blackout backstop with no exit-exemption
// logic — callers invoke it only for orders that OPEN or ADD outcome exposure
// (Place/PlaceBatch new legs, an exposure-increasing Modify, a non-reduce-only Twap
// or bracket entry). Non-outcome coins pass through. See outcomeSettleGate.
func (c *Client) outcomeSettleCheck(mk Market) error {
	if !mk.IsOutcome {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(mk.ResolutionStatus), "settled") {
		return output.Risk("outcome_settled",
			"outcome market "+mk.Coin+" has settled — no new exposure").
			WithHint("this market is resolved; quote an open outcome market")
	}
	win := c.riskConfig().OutcomeSettleBlackoutMins
	if win <= 0 {
		return nil
	}
	// An absent expiry is STRUCTURAL, not stale metadata: event and multi-outcome
	// markets carry no expiry (parseOutcomeDescription only fills it for priceBinary),
	// so there is no near-expiry blackout window to enforce. Do NOT fail closed here —
	// that would ban the entire event/multi-outcome class regardless of resolution time.
	// The "settled" check above still stops new exposure once such a market resolves.
	if strings.TrimSpace(mk.Expiry) == "" {
		return nil
	}
	exp, ok := parseOutcomeExpiryTime(mk.Expiry)
	if !ok {
		// Fail closed: a NON-EMPTY but unparseable expiry means stale/corrupt metadata
		// for a market that does have one — refuse rather than assume safety.
		return output.Risk("outcome_expiry_unknown",
			"cannot parse expiry for "+mk.Coin+" to enforce the settlement blackout — refusing new exposure").
			WithHint("retry once market metadata is fresh, or clear risk.outcome_settle_blackout_mins")
	}
	if time.Until(exp) <= time.Duration(win)*time.Minute {
		return output.Risk("outcome_settle_blackout",
			fmt.Sprintf("%s is within the %d-minute settlement blackout before expiry — no new exposure", mk.Coin, win)).
			WithHint("quotes are pulled near expiry to avoid adverse selection; reduce or hold to settle")
	}
	return nil
}

// checkLeverage caps leverage changes (§6).
func (c *Client) checkLeverage(x int) error {
	if cap := c.riskConfig().MaxLeverage; cap > 0 && x > cap {
		return output.Risk("max_leverage",
			fmt.Sprintf("leverage %dx exceeds cap %dx", x, cap)).
			WithHint(fmt.Sprintf("use <= %dx", cap))
	}
	return nil
}

// checkRateCap is a best-effort local throttle that fires before the exchange's
// own per-address limit (§7). It records action timestamps in a small log and
// rejects when more than max_orders_per_min occurred in the last 60s.
func (c *Client) checkRateCap() error {
	limit := c.automationConfig().MaxOrdersPerMin
	if limit <= 0 {
		return nil
	}
	// Serialize the whole read-modify-write across concurrent processes. Without
	// the lock, overlapping `deliverator` invocations each read the same count,
	// each pass the gate, and the last writer clobbers the others — a TOCTOU cap
	// bypass that lets more than max_orders_per_min through in a 60s window.
	lk, err := state.Lock(rateLockPath())
	if err != nil {
		return output.Network("rate_lock", "acquire rate lock: "+err.Error())
	}
	defer lk.Unlock()

	p := ratePath()
	cutoff := time.Now().Add(-time.Minute).UnixMilli()
	now := time.Now().UnixMilli()

	var kept []string
	if b, err := os.ReadFile(p); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line == "" {
				continue
			}
			if ts, err := strconv.ParseInt(line, 10, 64); err == nil && ts >= cutoff {
				kept = append(kept, line)
			}
		}
	}
	if len(kept) >= limit {
		return output.RateLimit("local_rate_cap",
			fmt.Sprintf("local cap: %d orders in the last minute (automation.max_orders_per_min=%d)", len(kept), limit)).
			WithRetryAfter(2000)
	}
	kept = append(kept, strconv.FormatInt(now, 10))
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = state.WriteFileAtomic(p, []byte(strings.Join(kept, "\n")+"\n"), 0o600)
	return nil
}
