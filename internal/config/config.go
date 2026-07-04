// Package config loads and validates the Deliverator TOML config (spec §14).
// Config is machine-local and never travels with the binary. Secrets live in the
// keychain/age file referenced here, never in this file.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/erickuhn19/deliverator/internal/state"
)

// Networks.
const (
	NetworkMainnet = "mainnet"
	NetworkTestnet = "testnet"
)

// HLMaxPriorityBps is Hyperliquid's hard cap on the order-priority fee (8 bps,
// i.e. p = 80000 where rate = p/1e8). Config caps/defaults can't exceed it.
const HLMaxPriorityBps = 8

// Builder attach modes.
const (
	AttachAll    = "all"    // attach the configured builder fee to every order
	AttachManual = "manual" // only attach when --builder-fee is passed explicitly
)

// Builder-fee defaults. Deliverator ships with its builder fee ON by default so a
// fresh install can fund the project — but it is GRACEFUL: the fee is only ever
// charged once the trader signs the one-time master `approveBuilderFee` (which the
// non-custodial agent key cannot sign). Until then, orders are placed fee-free, so
// nobody is ever blocked from trading. Override builder.address to route the fee to
// your own builder, or set builder.attach_mode = "manual" to opt out.
const (
	// DefaultBuilderAddress is the Deliverator builder EOA.
	DefaultBuilderAddress = "0x2D2AEf445717466eD8DBEfd2751cDc42369Fba75"
	// DefaultBuilderFeeTenthsBps is the default fee in tenths of a basis point
	// (50 = 0.05%). Perps cap is 100 (0.1%).
	DefaultBuilderFeeTenthsBps = 50
)

// Config mirrors the TOML in spec §14.
type Config struct {
	Network    string            `toml:"network"`
	Wallet     Wallet            `toml:"wallet"`
	Builder    Builder           `toml:"builder"`
	Risk       Risk              `toml:"risk"`
	Automation Automation        `toml:"automation"`
	State      State             `toml:"state"`
	Alerting   Alerting          `toml:"alerting"`
	Copy       Copy              `toml:"copy"`
	Accounts   map[string]string `toml:"accounts"`
	Endpoints  Endpoints         `toml:"endpoints"`
	// PerpDexs opts into builder-deployed sub-dex perps (HIP-3), e.g. ["xyz"] to
	// trade xyz:BRENTOIL. Each named dex's universe is loaded and its coins become
	// tradable as "<dex>:<coin>". The wildcard ["all"] (or ["*"]) opts into every
	// sub-dex live on the network, resolved dynamically at load. Empty (default) =
	// main perp dex + spot only.
	PerpDexs []string `toml:"perp_dexs,omitempty"`
	// Outcomes opts into HIP-4 outcome (prediction) markets. When true, the live
	// outcome universe is loaded each init and its binary Yes/No legs become tradable
	// as "#<encoding>" (e.g. `buy #6410`). Default false keeps the safe baseline
	// unchanged — no extra /info call, no new resolvable coins — until the operator
	// opts in (mirrors PerpDexs).
	Outcomes bool `toml:"outcomes,omitempty"`

	// MM is the HIP-4 outcome market-maker (outcome-mm binary) configuration. It is
	// a POINTER so the default deliverator config is byte-for-byte unchanged: nil =
	// absent, omitted on Save, so an operator who never runs the MM never grows an
	// [mm] block. The outcome-mm binary fills defaults via MMOrDefault when the table
	// is absent. Every knob here is strategy/compute-shaping — the hard money
	// backstops remain the account-wide [risk] gates, which DO bind outcome coins.
	MM *MM `toml:"mm,omitempty"`

	path string // resolved source path, for diagnostics
}

// MM configures the outcome market maker. See spec §5/§8.
type MM struct {
	// Enabled is the master switch for live quoting. Off (default) means the daemon
	// runs read-only/shadow even without --dry-run. DryRun forces shadow regardless.
	Enabled         bool `toml:"enabled"`
	DryRun          bool `toml:"dry_run"`           // shadow: compute + render quotes, never sign
	QuoteIntervalMs int  `toml:"quote_interval_ms"` // fast active-set requote cadence

	Selection MMSelection `toml:"selection"`
	Strategy  MMStrategy  `toml:"strategy"`
	Arb       MMArb       `toml:"arb"`
	Settle    MMSettle    `toml:"settle"`
	Fees      MMFees      `toml:"fees"`
}

// MMSelection ranks the outcome universe into the active quote set (spec §5).
type MMSelection struct {
	MaxActiveMarkets     int      `toml:"max_active_markets"`    // top-N quoted at once (bounded by margin + rate budget)
	MinTTLMins           int      `toml:"min_ttl_mins"`          // drop markets expiring sooner (adverse selection / blackout)
	MaxTTLMins           int      `toml:"max_ttl_mins"`          // drop very-far expiries; 0 = no upper band
	ScanIntervalSecs     int      `toml:"scan_interval_secs"`    // slow candidate-scan cadence (read-heavy)
	PriceableUnderlyings []string `toml:"priceable_underlyings"` // v1 can price these (live mark + vol), e.g. ["BTC","ETH","HYPE"]
	Pins                 []string `toml:"pins"`                  // always-quote "#<enc>" coins (operator override)
	Blacklist            []string `toml:"blacklist"`             // never-quote "#<enc>" coins

	// Composite score = P·(w_liquidity·L + w_spread·S + w_confidence·C); weights sum≈1.
	WLiquidity  float64 `toml:"w_liquidity"`
	WSpread     float64 `toml:"w_spread"`
	WConfidence float64 `toml:"w_confidence"`
	WVolume     float64 `toml:"w_volume"` // within L: volume sub-weight
	WDepth      float64 `toml:"w_depth"`  // within L: two-sided-depth sub-weight

	VolSat           float64 `toml:"vol_sat"`           // $ notional above which more volume doesn't help
	DepthRef         float64 `toml:"depth_ref"`         // $ two-sided depth mapped to L_d=1
	SpreadRef        float64 `toml:"spread_ref"`        // book half-spread mapped to S=1
	HalfSpreadFloor  float64 `toml:"half_spread_floor"` // min half-spread (covers fee-on-close + margin)
	PlateauK         float64 `toml:"plateau_k"`         // >1 broadens the mid-probability confidence plateau
	MinL             float64 `toml:"min_l"`             // eligibility floors: a zero on any core axis excludes
	MinS             float64 `toml:"min_s"`
	MinC             float64 `toml:"min_c"`
	HysteresisMargin float64 `toml:"hysteresis_margin"`  // must beat the Nth incumbent by this to displace
	DropThreshold    float64 `toml:"drop_threshold"`     // an active market is dropped once score falls below this
	MaxPerUnderlying int     `toml:"max_per_underlying"` // diversification cap; 0 = no cap
	MaxPerExpiry     int     `toml:"max_per_expiry"`     // diversification cap; 0 = no cap
}

// MMStrategy configures the inventory-skew PMM (spec §14 decision 2).
type MMStrategy struct {
	BaseSpread         float64 `toml:"base_spread"`          // half-spread in probability units (e.g. 0.02)
	Levels             int     `toml:"levels"`               // ladder levels per side
	LevelStep          float64 `toml:"level_step"`           // probability step between ladder levels
	BaseSizeShares     int64   `toml:"base_size_shares"`     // integer shares at level 0
	SizeStepShares     int64   `toml:"size_step_shares"`     // shares added per level out
	InventorySkew      float64 `toml:"inventory_skew"`       // lean coefficient: skews p* against held inventory
	MaxInventoryShares int64   `toml:"max_inventory_shares"` // per-market soft inventory cap (quote reducing side only past it)
	MinEdge            float64 `toml:"min_edge"`             // minimum edge over fair required to quote a level
	MidAnchorWeight    float64 `toml:"mid_anchor_weight"`    // 0..1: blend the quoting anchor toward the live book mid (0 ⇒ pure model fair, 1 ⇒ pure mid)
	QuoteNoSide        bool    `toml:"quote_no_side"`        // also quote the sibling NO coin so the MM can be two-sided/neutral while flat (buy NO = short YES)
	NoSideMinPrice     float64 `toml:"no_side_min_price"`    // only quote the NO side when its price ≥ this (skip ultra-thin tails where the $10 min forces a lopsided leg)
}

// MMArb configures the cross-book YES/NO scanner (spec §5, engages independent of selection).
type MMArb struct {
	Enabled       bool    `toml:"enabled"`
	MinEdge       float64 `toml:"min_edge"`        // required 1 − YES_ask − NO_ask − costs
	MaxSizeShares int64   `toml:"max_size_shares"` // cap per arb capture
}

// MMSettle configures settlement/expiry handling (operator decision: blackout + hold-to-settle).
type MMSettle struct {
	BlackoutMins int `toml:"blackout_mins"` // cancel quotes + place none this many mins before expiry; hold residual to auto-settle
}

// MMFees feeds the fee-on-close cost model (HIP-4 charges on close/burn/settle, not open).
type MMFees struct {
	CloseFeeBps float64 `toml:"close_fee_bps"` // estimated close fee (bps of notional); 0 = model no fee (rely on half_spread_floor)
}

// Wallet — the query target (master). The agent signing key lives ONLY in the OS
// keychain (see internal/wallet); there is no key-source or key-file config, so a
// stale `agent_key_source` cannot silently point the CLI away from the keychain.
type Wallet struct {
	// MasterAddress is the QUERY target for all reads. Never the agent address —
	// querying the agent address returns empty/garbage (the canonical bug, §4).
	MasterAddress string `toml:"master_address"`
}

// Builder — the operator's builder EOA and fee posture (§14, §17.2).
type Builder struct {
	Address      string `toml:"address"`        // builder EOA (>=100 USDC perps, standard)
	FeeTenthsBps int    `toml:"fee_tenths_bps"` // 50 = 0.05%; perps cap 100; <= approved max
	AttachMode   string `toml:"attach_mode"`    // all | manual
}

// Risk — hard caps enforced in core before signing (§6).
type Risk struct {
	MaxOrderNotionalUSD    float64 `toml:"max_order_notional_usd"`    // 0 = no cap
	MaxPositionNotionalUSD float64 `toml:"max_position_notional_usd"` // 0 = no cap (per-coin)
	MinOrderNotionalUSD    float64 `toml:"min_order_notional_usd"`    // 0 = no floor; default mirrors HL's ~$10 minimum
	MaxLeverage            int     `toml:"max_leverage"`              // 0 = no cap
	DeadManSwitchSecs      int     `toml:"dead_man_switch_secs"`      // 0 = off

	// Portfolio-level gates (account-wide, enforced in core before signing — a
	// hallucinating agent cannot exceed them by switching invocation). All default
	// to 0 = off, so the safe baseline is unchanged until the operator opts in.
	// Evaluated against the RESULTING book (current positions + the proposed new
	// exposure); reduce-only/close legs are exempt. "Equity" is the GREATER of perp
	// account_value and available USDC collateral — a unified account holds its
	// equity in spot, where perp account_value reads only the open-position margin.
	MaxAccountLeverage         float64 `toml:"max_account_leverage"`           // 0 = off; gross notional / equity
	MaxNetExposureUSD          float64 `toml:"max_net_exposure_usd"`           // 0 = off; |signed long − short|
	MaxConcentrationPctPerCoin float64 `toml:"max_concentration_pct_per_coin"` // 0 = off; |coin notional| / equity * 100
	MaxDrawdownPct             float64 `toml:"max_drawdown_pct"`               // 0 = off; (peak − equity) / peak * 100
	MaxDailyLossUSD            float64 `toml:"max_daily_loss_usd"`             // 0 = off; (UTC-day anchor − equity) USD
	MaxDailyLossPct            float64 `toml:"max_daily_loss_pct"`             // 0 = off; (anchor − equity) / anchor * 100
	MaxOpenPositions           int     `toml:"max_open_positions"`             // 0 = off; cap on the number of concurrent open positions

	// HIP-4 outcome-specific gates (enforced in core alongside the generic caps).
	// Both default 0 = off, keeping the safe baseline unchanged. They apply to
	// outcome ("#<enc>") coins only; perp/spot orders are unaffected.
	//
	// OutcomeSettleBlackoutMins refuses to OPEN or ADD outcome exposure within this
	// many minutes of a market's expiry (the adverse-selection window where informed
	// flow picks off a resting quote just before resolution). A settled market is
	// ALWAYS refused regardless of this knob. Exits (reduce-only/closing) stay allowed
	// so a holding can be unwound. Fail-closed: if the window is set but the expiry
	// can't be parsed, new exposure is refused.
	OutcomeSettleBlackoutMins int `toml:"outcome_settle_blackout_mins"` // 0 = off
	// MaxOutcomeQuestionNotionalUSD caps the total worst-case notional across every
	// leg of one HIP-4 question — the Yes and No sides of a binary, and every outcome
	// under a multi-outcome question, share one bucket. It backstops the per-coin
	// concentration cap, which cannot see that two "#<enc>" coins are the same bet.
	MaxOutcomeQuestionNotionalUSD float64 `toml:"max_outcome_question_notional_usd"` // 0 = off

	// MaxPriorityBps caps the order-priority fee (faster sequencing, paid in HYPE
	// from staking balance). Hyperliquid's hard max is 8 bps; a lower value limits
	// your own spend. A requested priority above this is clamped down (with a
	// warning), never rejected.
	MaxPriorityBps int `toml:"max_priority_bps"` // 0 = use HL's 8 bps default cap
}

// Automation — agent-facing guardrails (§6).
type Automation struct {
	// AllowedCoins: empty = allow all; non-empty = reject anything not listed.
	AllowedCoins    []string `toml:"allowed_coins"`
	LimitOnly       bool     `toml:"limit_only"`         // block market orders from automation
	MaxOrdersPerMin int      `toml:"max_orders_per_min"` // local pre-exchange throttle
	JSONWhenNotTTY  bool     `toml:"json_when_not_tty"`  // emit JSON when stdout is not a TTY
	// PriorityBps is the DEFAULT order-priority fee (bps) applied to every order
	// (faster sequencing — ~45ms/bp). 0 = off. Overridable per-order with
	// --priority-bps; clamped to risk.max_priority_bps (and HL's 8 bps hard cap).
	PriorityBps int `toml:"priority_bps"`
}

// Alerting — best-effort webhook fired on RED-state command failures (#48).
type Alerting struct {
	WebhookURL string   `toml:"webhook_url"` // empty = disabled (env DELIVERATOR_ALERT_WEBHOOK overrides)
	Categories []string `toml:"categories"`  // error categories that fire; empty = halt,auth,timeout
	TimeoutSec int      `toml:"timeout_sec"` // webhook POST timeout; 0 = 5s
}

// Copy — defaults for the `copy` (mirror) command (#27). These are COMPUTE-SHAPING
// knobs, not risk backstops: a copy --execute leg routes through Place/Close, which
// already enforce the account-wide gates (risk.max_account_leverage / net-exposure /
// concentration / drawdown / open-positions). So there is deliberately no copy
// book-gross or leverage backstop key here.
type Copy struct {
	DefaultScaleMode  string  `toml:"default_scale_mode"`   // "equity" (default) | "fixed"
	DefaultScale      float64 `toml:"default_scale"`        // multiplier applied to the leader's sizes (default 1)
	MinDiffUSD        float64 `toml:"min_diff_usd"`         // skip diff legs below this $ delta (0 = off)
	MinLiqDistancePct float64 `toml:"min_liq_distance_pct"` // skip open/increase legs whose est liq is closer than this % (0 = off)
	MaxLeverage       int     `toml:"max_leverage"`         // per-leg size CLIP hint for nicer diffs; NOT a backstop (0 = off)
	MaxOrdersPerCycle int     `toml:"max_orders_per_cycle"` // cap legs executed per cycle (0 = no cap)
}

// State — local cache, audit, nonce coordination (§8).
type State struct {
	MetaTTLSecs int    `toml:"meta_ttl_secs"`
	Audit       bool   `toml:"audit"`
	AuditPath   string `toml:"audit_path"`
	// CommandLog, when set, appends one JSONL line per CLI invocation (argv + exit
	// code) for live human oversight — `tail -f` it or `deliverator logs -f` in a
	// second terminal to watch every command the agent runs. Empty = off. The env
	// var DELIVERATOR_COMMAND_LOG overrides this for a single session.
	CommandLog string `toml:"command_log,omitempty"`
	// LeaderboardTTLSecs is how long a cached leaderboard blob is reused without
	// even revalidating against the stats host (the board is ~32 MB / 39k rows and
	// changes slowly). 0 = always revalidate via a cheap conditional GET (which
	// still avoids the re-download on a 304). Default 300.
	LeaderboardTTLSecs int `toml:"leaderboard_ttl_secs"`
}

// Endpoints — optional overrides (else derived from network).
type Endpoints struct {
	InfoURL string `toml:"info_url"`
	WSURL   string `toml:"ws_url"`
	// LeaderboardURL overrides the public trader-leaderboard source (else derived
	// from network: stats-data.hyperliquid.xyz/{Mainnet,Testnet}/leaderboard).
	LeaderboardURL string `toml:"leaderboard_url"`
}

// Default returns a fully-populated config with safe defaults. Load overlays the
// TOML file on top of this, so any key absent from the file keeps its default.
func Default() *Config {
	return &Config{
		Network: NetworkTestnet, // neutral safe fallback when NO config file exists; `init`/`onboard` write the opinionated shipped default (mainnet)
		Builder: Builder{
			Address:      DefaultBuilderAddress,
			FeeTenthsBps: DefaultBuilderFeeTenthsBps, // 0.05%; only charged after the one-time master approveBuilderFee (graceful attach)
			AttachMode:   AttachAll,                  // attach on every order when approved; skip silently (fee-free) when not
		},
		Risk: Risk{
			MaxOrderNotionalUSD:    10000,
			MaxPositionNotionalUSD: 50000,
			MinOrderNotionalUSD:    10, // HL rejects sub-~$10 orders; reject pre-flight with a clear error
			MaxLeverage:            10,
			DeadManSwitchSecs:      0,                // off until armed + heartbeat cron exists
			MaxPriorityBps:         HLMaxPriorityBps, // HL's hard cap; lower it to limit spend
		},
		Automation: Automation{
			AllowedCoins:    nil, // allow-all until the operator locks it down
			LimitOnly:       false,
			MaxOrdersPerMin: 120,
			JSONWhenNotTTY:  true,
		},
		State: State{
			MetaTTLSecs:        3600,
			Audit:              true,
			AuditPath:          filepath.Join(Dir(), "audit.jsonl"),
			LeaderboardTTLSecs: 300,
		},
		Copy: Copy{
			DefaultScaleMode: "equity", // mirror the leader proportionally to your equity
			DefaultScale:     1,        // 1:1 with your equity share
		},
		Accounts:  map[string]string{},
		Endpoints: Endpoints{},
	}
}

// DefaultMM returns the outcome-mm defaults. It is deliberately SAFE by default:
// Enabled=false and DryRun=true, so a freshly-started daemon computes and renders
// quotes but never signs until the operator explicitly sets mm.enabled=true and
// mm.dry_run=false. All numeric knobs are conservative starting points meant to be
// tuned against live fills.
func DefaultMM() *MM {
	return &MM{
		Enabled: false,
		DryRun:  true,
		// 3s: each tick reads Portfolio + a per-market book; 1s blew the exchange's
		// per-IP read-weight budget (429s). Fair value only moves on the scan cadence
		// anyway, so a few-second requote is plenty responsive for outcome markets.
		QuoteIntervalMs: 3000,
		Selection: MMSelection{
			MaxActiveMarkets:     6,
			MinTTLMins:           30,  // inside 30m of expiry we don't open (blackout territory)
			MaxTTLMins:           0,   // no upper band by default
			ScanIntervalSecs:     300, // 5-min candidate scan
			PriceableUnderlyings: []string{"BTC", "ETH", "HYPE"},
			WLiquidity:           0.40,
			WSpread:              0.30,
			WConfidence:          0.30,
			WVolume:              0.60,
			WDepth:               0.40,
			VolSat:               100_000,
			DepthRef:             5_000,
			SpreadRef:            0.05,
			HalfSpreadFloor:      0.01,
			PlateauK:             2.0,
			MinL:                 0.05,
			MinS:                 0.05,
			MinC:                 0.05,
			HysteresisMargin:     0.05,
			DropThreshold:        0.10,
			MaxPerUnderlying:     3,
			MaxPerExpiry:         3,
		},
		Strategy: MMStrategy{
			BaseSpread:         0.02,
			Levels:             3,
			LevelStep:          0.01,
			BaseSizeShares:     10,
			SizeStepShares:     5,
			InventorySkew:      0.50,
			MaxInventoryShares: 500,
			MinEdge:            0.005,
			MidAnchorWeight:    0.70, // mostly straddle the live market (model too noisy per-coin on liquid books); 1 → pure market-neutral, 0 → trust the model
			QuoteNoSide:        true, // two-sided while flat: express "short YES" by buying the sibling NO coin
			NoSideMinPrice:     0.10, // skip NO-quoting on ultra-deep-ITM (NO<0.10) where the $10 min forces a lopsided tail leg
		},
		Arb: MMArb{
			// Off by default: the two IOC legs are NOT atomic on the venue, so a
			// one-leg fill leaves a naked directional outcome position. Opt in only
			// once you accept that (and ideally size it small).
			Enabled:       false,
			MinEdge:       0.005,
			MaxSizeShares: 100,
		},
		Settle: MMSettle{BlackoutMins: 15},
		Fees:   MMFees{CloseFeeBps: 0},
	}
}

// MMOrDefault returns the configured [mm] table, or the full DefaultMM when the
// operator supplied none. The returned pointer is always non-nil, so the outcome-mm
// binary can read knobs without a nil guard.
func (c *Config) MMOrDefault() *MM {
	if c.MM != nil {
		return c.MM
	}
	return DefaultMM()
}

// IsMMKey reports whether key names an [mm] table field ("mm." prefix). Used by the
// CLI's `config set` to route to SetMMKey.
func IsMMKey(key string) bool { return strings.HasPrefix(key, "mm.") }

// SetMMKey sets a single [mm] table value from its string form. It is defined here
// (not in package cmd like setConfigKey) because the SEPARATE outcome-mm binary and
// its TUI need the same guarded edit and cannot reach package cmd. When [mm] is
// absent it is first seeded from DefaultMM so a single `config set mm.enabled true`
// yields a fully-sane table (not an all-zero one). The caller Validates + Saves.
func (c *Config) SetMMKey(key, val string) error {
	if c.MM == nil {
		c.MM = DefaultMM()
	}
	m := c.MM
	atoi := func() (int, error) { return strconv.Atoi(strings.TrimSpace(val)) }
	atoi64 := func() (int64, error) { return strconv.ParseInt(strings.TrimSpace(val), 10, 64) }
	atof := func() (float64, error) { return strconv.ParseFloat(strings.TrimSpace(val), 64) }
	atob := func() (bool, error) { return strconv.ParseBool(strings.TrimSpace(val)) }
	list := func() []string {
		if strings.TrimSpace(val) == "" {
			return nil
		}
		parts := strings.Split(val, ",")
		out := parts[:0]
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	setF := func(dst *float64) error {
		f, err := atof()
		if err == nil {
			*dst = f
		}
		return err
	}
	setI := func(dst *int) error {
		n, err := atoi()
		if err == nil {
			*dst = n
		}
		return err
	}
	setI64 := func(dst *int64) error {
		n, err := atoi64()
		if err == nil {
			*dst = n
		}
		return err
	}
	setB := func(dst *bool) error {
		b, err := atob()
		if err == nil {
			*dst = b
		}
		return err
	}

	switch key {
	case "mm.enabled":
		return setB(&m.Enabled)
	case "mm.dry_run":
		return setB(&m.DryRun)
	case "mm.quote_interval_ms":
		return setI(&m.QuoteIntervalMs)
	case "mm.selection.max_active_markets":
		return setI(&m.Selection.MaxActiveMarkets)
	case "mm.selection.min_ttl_mins":
		return setI(&m.Selection.MinTTLMins)
	case "mm.selection.max_ttl_mins":
		return setI(&m.Selection.MaxTTLMins)
	case "mm.selection.scan_interval_secs":
		return setI(&m.Selection.ScanIntervalSecs)
	case "mm.selection.priceable_underlyings":
		m.Selection.PriceableUnderlyings = list()
		return nil
	case "mm.selection.pins":
		m.Selection.Pins = list()
		return nil
	case "mm.selection.blacklist":
		m.Selection.Blacklist = list()
		return nil
	case "mm.selection.w_liquidity":
		return setF(&m.Selection.WLiquidity)
	case "mm.selection.w_spread":
		return setF(&m.Selection.WSpread)
	case "mm.selection.w_confidence":
		return setF(&m.Selection.WConfidence)
	case "mm.selection.w_volume":
		return setF(&m.Selection.WVolume)
	case "mm.selection.w_depth":
		return setF(&m.Selection.WDepth)
	case "mm.selection.vol_sat":
		return setF(&m.Selection.VolSat)
	case "mm.selection.depth_ref":
		return setF(&m.Selection.DepthRef)
	case "mm.selection.spread_ref":
		return setF(&m.Selection.SpreadRef)
	case "mm.selection.half_spread_floor":
		return setF(&m.Selection.HalfSpreadFloor)
	case "mm.selection.plateau_k":
		return setF(&m.Selection.PlateauK)
	case "mm.selection.min_l":
		return setF(&m.Selection.MinL)
	case "mm.selection.min_s":
		return setF(&m.Selection.MinS)
	case "mm.selection.min_c":
		return setF(&m.Selection.MinC)
	case "mm.selection.hysteresis_margin":
		return setF(&m.Selection.HysteresisMargin)
	case "mm.selection.drop_threshold":
		return setF(&m.Selection.DropThreshold)
	case "mm.selection.max_per_underlying":
		return setI(&m.Selection.MaxPerUnderlying)
	case "mm.selection.max_per_expiry":
		return setI(&m.Selection.MaxPerExpiry)
	case "mm.strategy.base_spread":
		return setF(&m.Strategy.BaseSpread)
	case "mm.strategy.levels":
		return setI(&m.Strategy.Levels)
	case "mm.strategy.level_step":
		return setF(&m.Strategy.LevelStep)
	case "mm.strategy.base_size_shares":
		return setI64(&m.Strategy.BaseSizeShares)
	case "mm.strategy.size_step_shares":
		return setI64(&m.Strategy.SizeStepShares)
	case "mm.strategy.inventory_skew":
		return setF(&m.Strategy.InventorySkew)
	case "mm.strategy.max_inventory_shares":
		return setI64(&m.Strategy.MaxInventoryShares)
	case "mm.strategy.min_edge":
		return setF(&m.Strategy.MinEdge)
	case "mm.strategy.mid_anchor_weight":
		return setF(&m.Strategy.MidAnchorWeight)
	case "mm.strategy.quote_no_side":
		return setB(&m.Strategy.QuoteNoSide)
	case "mm.strategy.no_side_min_price":
		return setF(&m.Strategy.NoSideMinPrice)
	case "mm.arb.enabled":
		return setB(&m.Arb.Enabled)
	case "mm.arb.min_edge":
		return setF(&m.Arb.MinEdge)
	case "mm.arb.max_size_shares":
		return setI64(&m.Arb.MaxSizeShares)
	case "mm.settle.blackout_mins":
		return setI(&m.Settle.BlackoutMins)
	case "mm.fees.close_fee_bps":
		return setF(&m.Fees.CloseFeeBps)
	}
	return fmt.Errorf("unknown config key %q", key)
}

// Dir is the config/state directory: $DELIVERATOR_HOME or ~/.config/deliverator.
func Dir() string {
	if d := os.Getenv("DELIVERATOR_HOME"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// $HOME is unknown and DELIVERATOR_HOME is unset. A relative fallback
		// (".deliverator") would resolve against the CWD, so two processes in
		// different working dirs would lock DIFFERENT nonce files and defeat the
		// cross-process monotonic-nonce guarantee. Fall back to an ABSOLUTE,
		// machine-local dir so the guarantee holds; operators should set
		// DELIVERATOR_HOME explicitly (audit #91 / T3-cwd).
		return filepath.Join(os.TempDir(), "deliverator")
	}
	return filepath.Join(home, ".config", "deliverator")
}

// Path is the config file path: $DELIVERATOR_CONFIG or <Dir>/config.toml.
func Path() string {
	if p := os.Getenv("DELIVERATOR_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(Dir(), "config.toml")
}

// Load reads the config file (if present) over the defaults and validates it.
// A missing file is not an error — defaults are returned so `init`/`connect`
// can run on a fresh box.
func Load(path string) (*Config, error) {
	if path == "" {
		path = Path()
	}
	cfg := Default()
	cfg.path = path

	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil // fresh install: defaults only
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	md, err := toml.Decode(string(b), cfg)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	// Strict decode: an UNKNOWN key (a typo like `max_order_notinal_usd`) is silently
	// dropped by the TOML decoder, so the operator would believe a risk cap is set
	// when it isn't — and `config set` would then re-Save the struct and erase the
	// typo'd line for good. Reject it, naming the stray key, so the cap can never be
	// silently missing (audit S1). Known REMOVED keys are tolerated for back-compat.
	if stray := undecodedKeys(md); len(stray) > 0 {
		return nil, fmt.Errorf("config %s has unknown key(s): %s — fix the spelling; a mistyped key is ignored, so its setting (e.g. a risk cap) would NOT take effect",
			path, strings.Join(stray, ", "))
	}
	// The [mm] table is a POINTER that starts nil, so unlike the value sub-structs it
	// does NOT inherit Default() under overlay — a PARTIAL [mm] would leave absent
	// sub-keys at Go zero (broken weights/sizes). Re-decode just the mm subtree over a
	// full DefaultMM so a partial table inherits defaults, matching how risk.* behaves.
	if md.IsDefined("mm") {
		seed := struct {
			MM *MM `toml:"mm"`
		}{MM: DefaultMM()}
		if _, derr := toml.Decode(string(b), &seed); derr != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, derr)
		}
		cfg.MM = seed.MM
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// deprecatedKeys are config keys removed in past versions. They are tolerated
// (ignored) on load so an older config still works, rather than failing the strict
// unknown-key check. agent_key_source/agent_key_file were dropped when the agent key
// became keychain-default + DELIVERATOR_AGENT_KEY override (no stored source).
var deprecatedKeys = map[string]bool{
	"wallet.agent_key_source": true,
	"wallet.agent_key_file":   true,
}

// undecodedKeys returns the TOML keys present in the file that map to no config
// field, excluding the known-deprecated set.
func undecodedKeys(md toml.MetaData) []string {
	var out []string
	for _, k := range md.Undecoded() {
		if s := k.String(); !deprecatedKeys[s] {
			out = append(out, s)
		}
	}
	return out
}

var hexAddr = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// Validate checks invariants the rest of the system relies on.
func (c *Config) Validate() error {
	switch c.Network {
	case NetworkMainnet, NetworkTestnet:
	default:
		return fmt.Errorf("network must be %q or %q, got %q", NetworkMainnet, NetworkTestnet, c.Network)
	}
	if c.Wallet.MasterAddress != "" && !hexAddr.MatchString(c.Wallet.MasterAddress) {
		return fmt.Errorf("wallet.master_address %q is not a 0x-prefixed 40-hex address", c.Wallet.MasterAddress)
	}
	if c.Builder.Address != "" && !hexAddr.MatchString(c.Builder.Address) {
		return fmt.Errorf("builder.address %q is not a 0x-prefixed 40-hex address", c.Builder.Address)
	}
	switch c.Builder.AttachMode {
	case AttachAll, AttachManual:
	default:
		return fmt.Errorf("builder.attach_mode must be all|manual, got %q", c.Builder.AttachMode)
	}
	if c.Builder.FeeTenthsBps < 0 || c.Builder.FeeTenthsBps > 1000 {
		return fmt.Errorf("builder.fee_tenths_bps %d out of range [0,1000]", c.Builder.FeeTenthsBps)
	}
	if c.Builder.AttachMode == AttachAll && c.Builder.Address == "" {
		return fmt.Errorf("builder.attach_mode=all requires builder.address to be set")
	}
	for alias, addr := range c.Accounts {
		if !hexAddr.MatchString(addr) {
			return fmt.Errorf("accounts.%s = %q is not a 0x-prefixed 40-hex address", alias, addr)
		}
	}
	if c.Risk.MaxLeverage < 0 {
		return fmt.Errorf("risk.max_leverage must be >= 0 (0 = no cap)")
	}
	if c.Risk.MaxOrderNotionalUSD < 0 {
		return fmt.Errorf("risk.max_order_notional_usd must be >= 0 (0 = no cap)")
	}
	if c.Risk.MaxPriorityBps < 0 || c.Risk.MaxPriorityBps > HLMaxPriorityBps {
		return fmt.Errorf("risk.max_priority_bps must be in [0,%d] (Hyperliquid's hard cap)", HLMaxPriorityBps)
	}
	if c.Automation.PriorityBps < 0 || c.Automation.PriorityBps > HLMaxPriorityBps {
		return fmt.Errorf("automation.priority_bps must be in [0,%d] (Hyperliquid's hard cap)", HLMaxPriorityBps)
	}
	if c.Risk.MaxPositionNotionalUSD < 0 {
		return fmt.Errorf("risk.max_position_notional_usd must be >= 0 (0 = no cap)")
	}
	if c.Risk.MinOrderNotionalUSD < 0 {
		return fmt.Errorf("risk.min_order_notional_usd must be >= 0 (0 = no floor)")
	}
	if c.Risk.MinOrderNotionalUSD > 0 && c.Risk.MaxOrderNotionalUSD > 0 &&
		c.Risk.MinOrderNotionalUSD > c.Risk.MaxOrderNotionalUSD {
		return fmt.Errorf("risk.min_order_notional_usd ($%.2f) must be <= risk.max_order_notional_usd ($%.2f) — no order could satisfy both bounds",
			c.Risk.MinOrderNotionalUSD, c.Risk.MaxOrderNotionalUSD)
	}
	// From a flat position the resulting position notional equals the order
	// notional, so a floor above the position cap makes every fresh open impossible.
	if c.Risk.MinOrderNotionalUSD > 0 && c.Risk.MaxPositionNotionalUSD > 0 &&
		c.Risk.MinOrderNotionalUSD > c.Risk.MaxPositionNotionalUSD {
		return fmt.Errorf("risk.min_order_notional_usd ($%.2f) must be <= risk.max_position_notional_usd ($%.2f) — no new position could be opened",
			c.Risk.MinOrderNotionalUSD, c.Risk.MaxPositionNotionalUSD)
	}
	if c.Risk.DeadManSwitchSecs < 0 {
		return fmt.Errorf("risk.dead_man_switch_secs must be >= 0 (0 = off)")
	}
	for _, g := range []struct {
		name string
		val  float64
	}{
		{"risk.max_account_leverage", c.Risk.MaxAccountLeverage},
		{"risk.max_net_exposure_usd", c.Risk.MaxNetExposureUSD},
		{"risk.max_concentration_pct_per_coin", c.Risk.MaxConcentrationPctPerCoin},
		{"risk.max_drawdown_pct", c.Risk.MaxDrawdownPct},
		{"risk.max_daily_loss_usd", c.Risk.MaxDailyLossUSD},
		{"risk.max_daily_loss_pct", c.Risk.MaxDailyLossPct},
	} {
		if g.val < 0 {
			return fmt.Errorf("%s must be >= 0 (0 = off)", g.name)
		}
	}
	if c.Risk.MaxDrawdownPct > 100 || c.Risk.MaxDailyLossPct > 100 {
		return fmt.Errorf("risk.max_drawdown_pct / risk.max_daily_loss_pct are percentages in [0,100]")
	}
	if c.Risk.MaxOpenPositions < 0 {
		return fmt.Errorf("risk.max_open_positions must be >= 0 (0 = off)")
	}
	if c.Risk.OutcomeSettleBlackoutMins < 0 {
		return fmt.Errorf("risk.outcome_settle_blackout_mins must be >= 0 (0 = off)")
	}
	if c.Risk.MaxOutcomeQuestionNotionalUSD < 0 {
		return fmt.Errorf("risk.max_outcome_question_notional_usd must be >= 0 (0 = off)")
	}
	if c.Alerting.TimeoutSec < 0 {
		return fmt.Errorf("alerting.timeout_sec must be >= 0 (0 = 5s default)")
	}
	if c.Alerting.WebhookURL != "" && !strings.HasPrefix(c.Alerting.WebhookURL, "http://") && !strings.HasPrefix(c.Alerting.WebhookURL, "https://") {
		return fmt.Errorf("alerting.webhook_url must be an http(s) URL")
	}
	if c.Endpoints.LeaderboardURL != "" && !strings.HasPrefix(c.Endpoints.LeaderboardURL, "http://") && !strings.HasPrefix(c.Endpoints.LeaderboardURL, "https://") {
		return fmt.Errorf("endpoints.leaderboard_url must be an http(s) URL")
	}
	// /info supplies the mids that price market/TWAP orders and the coin→assetId
	// meta; the WS carries fills/positions. A plaintext override is a MitM vector
	// that mis-prices/mis-sizes orders signed against the real exchange — require
	// TLS, secure by default (audit #91 / S7).
	if c.Endpoints.InfoURL != "" && !strings.HasPrefix(c.Endpoints.InfoURL, "https://") {
		return fmt.Errorf("endpoints.info_url must be an https:// URL")
	}
	if c.Endpoints.WSURL != "" && !strings.HasPrefix(c.Endpoints.WSURL, "wss://") {
		return fmt.Errorf("endpoints.ws_url must be a wss:// URL")
	}
	// Confine the audit trail to the state dir: a config must not redirect the
	// money trail to an arbitrary location via .. or an absolute path (audit #91 / T3-path).
	if err := pathConfinedTo("state.audit_path", c.State.AuditPath, Dir()); err != nil {
		return err
	}
	switch c.Copy.DefaultScaleMode {
	case "", "equity", "fixed":
	default:
		return fmt.Errorf("copy.default_scale_mode must be equity|fixed, got %q", c.Copy.DefaultScaleMode)
	}
	if c.Copy.DefaultScale < 0 || c.Copy.MinDiffUSD < 0 || c.Copy.MinLiqDistancePct < 0 || c.Copy.MaxLeverage < 0 || c.Copy.MaxOrdersPerCycle < 0 {
		return fmt.Errorf("copy.* values must be >= 0")
	}
	if err := c.MM.validate(); err != nil {
		return err
	}
	return nil
}

// validate checks the [mm] table. A nil MM (the default: no [mm] in the file) is
// always valid — the outcome-mm binary substitutes DefaultMM. Method-on-pointer so
// the nil receiver is handled here rather than at every call site.
func (m *MM) validate() error {
	if m == nil {
		return nil
	}
	if m.QuoteIntervalMs < 0 {
		return fmt.Errorf("mm.quote_interval_ms must be >= 0")
	}
	s := m.Selection
	if s.MaxActiveMarkets < 0 {
		return fmt.Errorf("mm.selection.max_active_markets must be >= 0")
	}
	if s.MinTTLMins < 0 || s.MaxTTLMins < 0 {
		return fmt.Errorf("mm.selection.min_ttl_mins / max_ttl_mins must be >= 0")
	}
	if s.MaxTTLMins > 0 && s.MinTTLMins > 0 && s.MinTTLMins > s.MaxTTLMins {
		return fmt.Errorf("mm.selection.min_ttl_mins (%d) must be <= max_ttl_mins (%d)", s.MinTTLMins, s.MaxTTLMins)
	}
	if s.ScanIntervalSecs < 0 {
		return fmt.Errorf("mm.selection.scan_interval_secs must be >= 0")
	}
	for _, w := range []struct {
		name string
		val  float64
	}{
		{"w_liquidity", s.WLiquidity}, {"w_spread", s.WSpread}, {"w_confidence", s.WConfidence},
		{"w_volume", s.WVolume}, {"w_depth", s.WDepth}, {"vol_sat", s.VolSat}, {"depth_ref", s.DepthRef},
		{"spread_ref", s.SpreadRef}, {"half_spread_floor", s.HalfSpreadFloor}, {"plateau_k", s.PlateauK},
		{"min_l", s.MinL}, {"min_s", s.MinS}, {"min_c", s.MinC}, {"hysteresis_margin", s.HysteresisMargin},
		{"drop_threshold", s.DropThreshold},
	} {
		if w.val < 0 {
			return fmt.Errorf("mm.selection.%s must be >= 0", w.name)
		}
	}
	st := m.Strategy
	if st.BaseSpread < 0 || st.LevelStep < 0 || st.MinEdge < 0 || st.InventorySkew < 0 {
		return fmt.Errorf("mm.strategy.{base_spread,level_step,min_edge,inventory_skew} must be >= 0")
	}
	if st.Levels < 0 || st.BaseSizeShares < 0 || st.SizeStepShares < 0 || st.MaxInventoryShares < 0 {
		return fmt.Errorf("mm.strategy.{levels,base_size_shares,size_step_shares,max_inventory_shares} must be >= 0")
	}
	if st.BaseSpread >= 1 {
		return fmt.Errorf("mm.strategy.base_spread (%.4f) must be < 1 (probability units)", st.BaseSpread)
	}
	if st.MidAnchorWeight < 0 || st.MidAnchorWeight > 1 {
		return fmt.Errorf("mm.strategy.mid_anchor_weight (%.4f) must be in [0,1]", st.MidAnchorWeight)
	}
	if st.NoSideMinPrice < 0 || st.NoSideMinPrice >= 1 {
		return fmt.Errorf("mm.strategy.no_side_min_price (%.4f) must be in [0,1)", st.NoSideMinPrice)
	}
	if m.Arb.MinEdge < 0 || m.Arb.MaxSizeShares < 0 {
		return fmt.Errorf("mm.arb.{min_edge,max_size_shares} must be >= 0")
	}
	if m.Settle.BlackoutMins < 0 {
		return fmt.Errorf("mm.settle.blackout_mins must be >= 0")
	}
	if m.Fees.CloseFeeBps < 0 {
		return fmt.Errorf("mm.fees.close_fee_bps must be >= 0")
	}
	return nil
}

// Save writes the config as TOML, creating the directory if needed. Note:
// re-encoding does not preserve comments; `init` writes a commented template.
func (c *Config) Save(path string) error {
	if path == "" {
		path = c.path
	}
	if path == "" {
		path = Path()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Encode first, then write atomically: a crash mid-write must not lose the
	// risk caps or leave a half-written config, and a pre-existing world-readable
	// config is re-created 0600 (audit #91 / S12, T3-file-mode, T3-symlink).
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return err
	}
	return state.WriteFileAtomic(path, buf.Bytes(), 0o600)
}

// SourcePath reports where the config was loaded from.
func (c *Config) SourcePath() string { return c.path }

// IsMasterSynonym reports whether account names the default/master account:
// ""/"main"/"master"/"default", case-insensitively.
func IsMasterSynonym(account string) bool {
	switch strings.ToLower(account) {
	case "", "main", "master", "default":
		return true
	}
	return false
}

// CanonicalAccount maps the master synonyms to the canonical "main" alias — the
// name `onboard`/`init` store the default agent key under (wallet keyringUser) —
// so a keychain lookup keyed on the raw --account value can never miss the
// onboarded key (e.g. `--account master` → agent:main). Real aliases pass
// through unchanged.
func CanonicalAccount(account string) string {
	if IsMasterSynonym(account) {
		return "main"
	}
	return account
}

// IsReservedAlias reports whether name collides (case-insensitively) with the
// master synonyms. ResolveAddress consults the synonyms BEFORE [accounts], so
// an alias literally named main/master/default would be accepted on add but
// silently resolve to the MASTER address forever — `account add` rejects it.
func IsReservedAlias(name string) bool { return IsMasterSynonym(name) }

// ResolveAddress maps an account alias to an address, falling back to the master
// address. "main"/"master"/"default"/"" all resolve to the master address. All
// READS use this — never the agent address (§4).
func (c *Config) ResolveAddress(account string) (string, error) {
	if IsMasterSynonym(account) {
		if c.Wallet.MasterAddress == "" {
			return "", fmt.Errorf("wallet.master_address is not configured")
		}
		return c.Wallet.MasterAddress, nil
	}
	if addr, ok := c.Accounts[account]; ok {
		return addr, nil
	}
	return "", fmt.Errorf("unknown account %q (not in [accounts] and not master)", account)
}

// ExpandPath expands a leading ~ to the user's home directory.
func ExpandPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// pathConfinedTo errors if `configured` (after ~-expansion, joining a relative
// path onto base, and cleaning) resolves outside base. ExpandPath only handles
// ~, so confinement is enforced here — a relative `../x` is joined BEFORE Clean
// so it can't sneak out, and an absolute path outside base is rejected outright
// (audit #91 / T3-path). Empty is allowed (means "use the default").
func pathConfinedTo(field, configured, base string) error {
	if configured == "" {
		return nil
	}
	p := ExpandPath(configured)
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	p = filepath.Clean(p)
	base = filepath.Clean(base)
	if p != base && !strings.HasPrefix(p, base+string(filepath.Separator)) {
		return fmt.Errorf("%s %q escapes the config directory %q — keep it under that dir or set DELIVERATOR_HOME to its parent", field, configured, base)
	}
	return nil
}
