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

	path string // resolved source path, for diagnostics
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
		Network: NetworkTestnet, // testnet-first by design (§15)
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

// ResolveAddress maps an account alias to an address, falling back to the master
// address. "main"/"master"/"" all resolve to the master address. All READS use
// this — never the agent address (§4).
func (c *Config) ResolveAddress(account string) (string, error) {
	switch strings.ToLower(account) {
	case "", "main", "master", "default":
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
