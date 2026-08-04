package core

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	hl "github.com/erickuhn19/deliverator/internal/hl"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
	"github.com/erickuhn19/deliverator/internal/state"
	"github.com/erickuhn19/deliverator/internal/wallet"
)

// maxInfoBodyBytes caps an /info response body so a malicious/buggy endpoint
// (or MITM — we don't pin) can't OOM the process; we fail closed on overflow
// (audit #91 / S8). 16 MiB dwarfs any real /info payload.
const maxInfoBodyBytes = 16 << 20

// Options configures a Client for a single one-shot invocation.
type Options struct {
	Account     string
	RefreshMeta bool
	NoAudit     bool
	DryRun      bool
	Strict      bool
	Timeout     time.Duration
}

// guardConfig is the subset of configuration a long-running client may reload
// without changing its network, account, endpoints, or market universe.
type guardConfig struct {
	risk       config.Risk
	automation config.Automation
}

// guardGeneration identifies the on-disk config a guard snapshot came from.
// (mtime, size) is enough to notice any `config set`, which rewrites the file
// atomically, and it costs one stat rather than a parse per request.
type guardGeneration struct {
	ModTimeMs int64
	Size      int64
	Loaded    bool
}

// String is what a rejection quotes so a stale cache is visible in the error
// itself, rather than only by diffing against a fresh CLI fork.
func (g guardGeneration) String() string {
	if !g.Loaded {
		return "startup (config generation unknown)"
	}
	return fmt.Sprintf("config generation %d (mtime %s)",
		g.ModTimeMs, time.UnixMilli(g.ModTimeMs).UTC().Format("2006-01-02T15:04:05Z"))
}

// currentGuardGeneration stamps the config file the client is starting from, so
// the first ReloadGuardsIfChanged has a real baseline to compare against instead
// of reloading once spuriously.
func currentGuardGeneration(cfg *config.Config) guardGeneration {
	path := config.Path()
	if cfg != nil && cfg.SourcePath() != "" {
		path = cfg.SourcePath()
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Size() == 0 {
		return guardGeneration{}
	}
	return guardGeneration{ModTimeMs: fi.ModTime().UnixMilli(), Size: fi.Size(), Loaded: true}
}

func guardConfigFrom(cfg *config.Config) *guardConfig {
	if cfg == nil {
		return &guardConfig{}
	}
	auto := cfg.Automation
	auto.AllowedCoins = append([]string(nil), cfg.Automation.AllowedCoins...)
	return &guardConfig{risk: cfg.Risk, automation: auto}
}

// Client is the only thing that talks to Hyperliquid. The CLI is a thin adapter
// over it (§3.6, §12). It owns meta caching, nonce coordination, signing, and
// the raw /info calls internal/hl doesn't surface as typed methods.
type Client struct {
	cfg     *config.Config
	opts    Options
	network string
	infoURL string
	signURL string
	lbURL   string // public trader-leaderboard source (stats-data host)
	httpc   *http.Client

	// guards is the risk/automation subset read by the gates, held behind a mutex
	// rather than read straight off cfg: a long-running client serves gate reads
	// from several goroutines, and racing on the config struct is a data race in
	// the one place that must be right. Non-nil for production clients; hand-built
	// unit clients leave it nil and fall back to cfg, which keeps the simple test
	// fixtures working.
	//
	// The runtime swap is ReloadGuardsIfChanged, whose consumer is `serve` (#41):
	// a server loaded risk config at startup and then enforced it from memory
	// forever, rejecting 3,000+ placements against a cap the operator had already
	// raised on disk. guardGen stamps the config file the current guards came
	// from, so a rejection can say WHICH generation it enforced.
	guardMu  sync.RWMutex
	guards   *guardConfig
	guardGen guardGeneration

	meta *MetaStore
	info *hl.Info

	// Meta freshness AT USE (#38). state.meta_ttl_secs was evaluated exactly once,
	// in New: for a fork-per-command CLI "was the cache fresh at startup" and "is
	// it fresh now" are the same question, but `serve` holds the answer for days.
	// A stale szDecimals does not fail loudly — it mis-rounds a SIGNED order.
	//
	// metaTTL is 0 for hand-built clients that bypass New (most unit fixtures),
	// and 0 means REFRESH DISABLED: a test client has no live endpoint to refresh
	// against, and silently reaching for one would turn unit tests into network
	// tests. Only New sets it.
	metaMu      sync.Mutex
	metaTTL     time.Duration
	metaPath    string
	metaAttempt time.Time // last refresh ATTEMPT, success or not (backoff anchor)
	metaLastErr error     // last refresh failure, surfaced as a warning while stale

	queryAddr string // master/sub address for READS (never the agent address, §4)
	vaultAddr string // "" for master; sub-account address otherwise

	nonce *state.NonceLock
	audit *state.Audit

	// lazily initialized only when a write needs to sign, and guarded by exMu:
	// under `serve` several connection goroutines can reach the first write at
	// once, and the HIP-4 roll re-registers the signer's asset ids underneath
	// them (see reregisterSignerUniverse). Nothing here is safe to touch unlocked.
	exMu       sync.Mutex
	agent      *wallet.Agent
	ex         *hl.Exchange
	signerWarn string // non-empty if the loaded signer is misconfigured (T3-keybind)

	// builder-fee approval memo (graceful attach, §17.2). The master-approved max
	// (maxBuilderFee) rarely changes and a CLI invocation is short-lived, so it is
	// read once per builder and reused — no per-order read. See resolveBuilderApproved.
	// Keyed on builder address alone, valid ONLY because queryAddr is immutable per
	// Client (set once in New, never reassigned); if queryAddr ever becomes mutable
	// (multi-account reuse), key the memo on (queryAddr, builder) instead.
	builderApprMu  sync.Mutex
	builderApprFor string    // builder address the memo is for (lowercased)
	builderApprMax int       // approved max fee (tenths-bps); valid only when builderApprOK
	builderApprOK  bool      // true once successfully fetched
	builderApprAt  time.Time // fetch time (TTL anchor)
}

func (c *Client) currentGuards() guardConfig {
	c.guardMu.RLock()
	defer c.guardMu.RUnlock()
	if c.guards != nil {
		return *c.guards
	}
	if c.cfg == nil {
		return guardConfig{}
	}
	return *guardConfigFrom(c.cfg)
}

func (c *Client) riskConfig() config.Risk { return c.currentGuards().risk }

func (c *Client) automationConfig() config.Automation { return c.currentGuards().automation }

// signerWarnings returns the one-shot signer-binding warning (audit #91 /
// T3-keybind), or nil. Every write path prepends it to its warnings so a
// dangerous signer setup surfaces in the result envelope.
func (c *Client) signerWarnings() []string {
	c.exMu.Lock()
	defer c.exMu.Unlock()
	if c.signerWarn == "" {
		return nil
	}
	return []string{c.signerWarn}
}

// signerWarnFor returns the keybind warning when the loaded agent address IS the
// configured master address — i.e. the master key (which can withdraw) was
// loaded as the signing agent, defeating the non-custodial design that expects a
// separate, withdrawal-incapable API wallet. Returns "" otherwise. It is the one
// agent↔master mismatch checkable locally without false positives (a real agent
// wallet always has a different address); HL enforces the rest at submit time.
func signerWarnFor(masterAddr, agentAddr string) string {
	if masterAddr != "" && strings.EqualFold(agentAddr, masterAddr) {
		return "loaded key is your MASTER key (it can withdraw) — the non-custodial design expects a separate API/agent wallet; re-run `deliverator onboard` with an approved agent key"
	}
	return ""
}

func signURLFor(network string) string {
	if network == config.NetworkMainnet {
		return hl.MainnetAPIURL // MUST be the exact constant to sign as mainnet
	}
	return hl.TestnetAPIURL
}

// New constructs a Client: resolves URLs + query address, loads or fetches the
// market metadata, and wires the nonce lock + audit log. It does NOT load the
// signing key — that happens lazily on the first write.
func New(ctx context.Context, cfg *config.Config, opts Options) (*Client, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 15 * time.Second
	}
	c := &Client{
		cfg:      cfg,
		opts:     opts,
		network:  cfg.Network,
		signURL:  signURLFor(cfg.Network),
		httpc:    &http.Client{Timeout: opts.Timeout},
		guards:   guardConfigFrom(cfg),
		guardGen: currentGuardGeneration(cfg),
	}
	c.infoURL = c.signURL
	if cfg.Endpoints.InfoURL != "" {
		c.infoURL = cfg.Endpoints.InfoURL
	}
	c.lbURL = leaderboardURLFor(cfg.Network)
	if cfg.Endpoints.LeaderboardURL != "" {
		c.lbURL = cfg.Endpoints.LeaderboardURL
	}

	// Reads target the master (or sub-account) address — never the agent (§4).
	// An explicitly-passed account that does not resolve is a HARD error: with a
	// silently-empty queryAddr, reads mis-target and env-key writes would sign
	// for the MASTER account with the per-coin position/flip gates blinded to
	// the live book. Only the master synonyms stay tolerant when the master is
	// unset (the no-flag case requireQueryAddr already guards with exit 30).
	addr, aerr := cfg.ResolveAddress(opts.Account)
	if aerr != nil && !config.IsMasterSynonym(opts.Account) {
		return nil, output.Validation("unknown_account", aerr.Error()).
			WithHint(accountsHint(cfg))
	}
	c.queryAddr = addr
	if c.queryAddr != "" && !strings.EqualFold(c.queryAddr, cfg.Wallet.MasterAddress) {
		c.vaultAddr = c.queryAddr // a sub-account/vault, not the master
	}

	// Meta: use a fresh cache, else fetch and persist.
	metaPath := filepath.Join(config.Dir(), "meta.json")
	ttl := time.Duration(cfg.State.MetaTTLSecs) * time.Second
	// The same TTL now also governs refresh AT USE (#38). Only New sets these, so
	// a hand-built client keeps metaTTL == 0 and never reaches for the network.
	c.metaTTL, c.metaPath = ttl, metaPath
	if ms, ok := LoadMetaCache(metaPath, c.network); ok && !opts.RefreshMeta && ms.Age() < ttl {
		info, err := safeNewInfo(ctx, c.infoURL, c.httpc, ms.Meta(), ms.SpotMeta())
		if err != nil {
			return nil, mapNetwork("api_unreachable", err)
		}
		c.meta, c.info = ms, info
	} else {
		info, err := safeNewInfo(ctx, c.infoURL, c.httpc, nil, nil) // internal/hl fetches metas internally
		if err != nil {
			return nil, mapNetwork("api_unreachable", err)
		}
		meta, err := info.Meta(ctx)
		if err != nil {
			return nil, mapNetwork("meta_fetch", err)
		}
		spot, _ := info.SpotMeta(ctx) // spot is optional; nil is fine
		c.meta = NewMetaStore(c.network, meta, spot, time.Now())
		_ = c.meta.Save(metaPath)
		c.info = info
	}

	// HIP-3 builder sub-dexes are loaded fresh each init (they aren't part of the
	// cached main meta) and registered into the read Info + meta store.
	if err := c.loadPerpDexs(ctx); err != nil {
		return nil, mapNetwork("perp_dex_load", err)
	}

	// HIP-4 outcome markets rotate daily (settled ones drop out), so they are
	// loaded fresh and registered so "#<enc>" coins resolve and sign. config.outcomes
	// EAGER-loads them at init (so reads like positions surface held outcome tokens
	// without a "#" arg); otherwise they load ON DEMAND via EnsureOutcomes when a
	// command actually references an outcome (see cmd.newClient). Either way no
	// config flag is required to trade or list them.
	if cfg.Outcomes {
		if err := c.loadOutcomes(ctx); err != nil {
			return nil, mapNetwork("outcome_load", err)
		}
	}

	c.nonce = state.NewNonceLock(filepath.Join(config.Dir(), "nonce.lock"))
	c.audit = state.NewAudit(config.ExpandPath(cfg.State.AuditPath), cfg.State.Audit && !opts.NoAudit)
	return c, nil
}

// accountsHint lists the configured [accounts] aliases (alias lookup is
// case-sensitive) so a typo'd --account is correctable straight from the
// failure envelope.
func accountsHint(cfg *config.Config) string {
	aliases := make([]string, 0, len(cfg.Accounts))
	for a := range cfg.Accounts {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	if len(aliases) == 0 {
		return "no [accounts] aliases are configured — omit --account for the master, or add one: `deliverator account add`"
	}
	return "configured aliases (case-sensitive): " + strings.Join(aliases, ", ") +
		" — or omit --account for the master; `deliverator account ls`"
}

// isPerpDexWildcard reports whether a perp_dexs entry is the "opt into everything"
// token — "all" or "*" (case-insensitive) — rather than a specific sub-dex name.
func isPerpDexWildcard(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "all" || s == "*"
}

// expandPerpDexs resolves the configured perp_dexs list against the sub-dex names
// available on the network. If the config contains the "all"/"*" wildcard, it becomes
// every non-empty available name (index 0 = "" is the core dex, excluded), in network
// order (deterministic). Otherwise the configured list is returned unchanged.
func expandPerpDexs(configured, available []string) []string {
	wild := false
	for _, d := range configured {
		if isPerpDexWildcard(d) {
			wild = true
			break
		}
	}
	if !wild {
		return configured
	}
	out := make([]string, 0, len(available))
	for _, n := range available {
		if strings.TrimSpace(n) != "" {
			out = append(out, n)
		}
	}
	return out
}

// loadPerpDexs loads each configured builder sub-dex (HIP-3) and registers its
// coins (as "<dex>:<coin>") into the meta store and the read Info, so they are
// tradable and sign with the correct asset id. The "all"/"*" wildcard opts into
// every sub-dex live on the network (resolved from the fetched dex names).
func (c *Client) loadPerpDexs(ctx context.Context) error {
	if len(c.cfg.PerpDexs) == 0 {
		return nil
	}
	names, err := c.info.PerpDexNames(ctx)
	if err != nil {
		return err
	}
	// Resolve the "all"/"*" wildcard in place so every downstream consumer (reads,
	// writes, watch, risk view) sees the concrete sub-dex list with no special-casing.
	// c.cfg is the client's own *config.Config and is never re-saved, so this in-memory
	// expansion has no persistence side effect; the on-disk sentinel stays "all".
	c.cfg.PerpDexs = expandPerpDexs(c.cfg.PerpDexs, names)
	idxByName := make(map[string]int, len(names))
	for i, n := range names {
		if n != "" {
			idxByName[strings.ToLower(n)] = i
		}
	}
	for _, dex := range c.cfg.PerpDexs {
		d := strings.ToLower(strings.TrimSpace(dex))
		idx, ok := idxByName[d]
		if !ok {
			return fmt.Errorf("perp dex %q not found", dex)
		}
		m, err := c.info.MetaForDex(ctx, d)
		if err != nil {
			return err
		}
		c.meta.AddPerpDex(idx, m)
		c.info.RegisterPerpDex(idx, m)
	}
	return nil
}

// loadOutcomes loads the live HIP-4 outcome universe and registers its binary
// Yes/No legs (as "#<encoding>") into the meta store and the read Info, so they
// resolve and sign with the correct asset id. Outcome markets rotate (settled ones
// drop out), so they are fetched fresh each init rather than cached.
func (c *Client) loadOutcomes(ctx context.Context) error {
	om, err := c.info.OutcomeMeta(ctx)
	if err != nil {
		return err
	}
	if om == nil || len(om.Outcomes) == 0 {
		return nil
	}
	c.meta.AddOutcomes(om)
	c.info.RegisterOutcomes(om)
	c.reregisterSignerUniverse(om)
	return nil
}

// reregisterSignerUniverse teaches an ALREADY-BUILT signer the reloaded HIP-4
// asset ids.
//
// The signer carries its own hl.Info, seeded exactly once in exchange() on the
// process's first write. Refreshing only c.meta and c.info left that copy on the
// previous day's universe, so after a roll a placement resolved through the
// gates, got SIGNED, and then died in newCreateOrderAction with "coin #<enc> not
// found in info" — mapped to exit 50 (exchange-rejected, NON-retryable), which
// serve's unknown_coin retry cannot recover. That reproduced the very outage the
// reactive refresh was added to fix. See #43.
//
// No-op before the first write, when there is no signer to correct.
func (c *Client) reregisterSignerUniverse(om *hl.OutcomeMeta) {
	if om == nil {
		return
	}
	c.exMu.Lock()
	defer c.exMu.Unlock()
	if c.ex == nil {
		return
	}
	c.ex.Info().RegisterOutcomes(om)
}

// EnsureOutcomes lazily loads the HIP-4 outcome universe if it isn't already
// loaded. It is the on-demand counterpart to the eager `config.outcomes` load:
// a command that references a "#<enc>" coin or lists `--class outcome|all` calls
// this so outcomes resolve/sign without the operator pre-enabling a flag. The
// daily-rotating outcome set (hundreds of markets, one extra /info fetch) is thus
// fetched only when actually needed. Idempotent: a no-op once loaded.
func (c *Client) EnsureOutcomes(ctx context.Context) error {
	if c.meta.OutcomeMeta() != nil {
		return nil
	}
	return c.loadOutcomes(ctx)
}

// RefreshOutcomes reloads the HIP-4 universe unconditionally.
//
// EnsureOutcomes caches for the process lifetime, which is right for a one-shot
// CLI and WRONG for anything long-lived: outcome markets are DAILY, so a server
// that loaded the universe at startup stops recognising the coin the moment the
// binary rolls. Every order then fails "unknown coin #<enc>" — while the
// decision layer, reading a stream that resolves coins independently, believes
// it is quoting normally.
//
// That is not hypothetical: it cost a full session of trading. A downstream
// maker sat at 398 consecutive placement failures against a market it could see
// perfectly well.
func (c *Client) RefreshOutcomes(ctx context.Context) error {
	return c.loadOutcomes(ctx)
}

// safeNewInfo / safeNewExchange convert internal/hl's panic-on-meta-fetch-failure
// into a normal error (NewInfo/NewExchange panic if a nil meta must be fetched
// and the network call fails).
func safeNewInfo(ctx context.Context, url string, httpc *http.Client, meta *hl.Meta, spot *hl.SpotMeta) (info *hl.Info, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	info = hl.NewInfo(ctx, url, true /*skipWS*/, meta, spot, nil,
		hl.InfoOptClientOptions(hl.ClientOptHTTPClient(httpc)))
	return
}

func safeNewExchange(ctx context.Context, key *ecdsa.PrivateKey, url string, httpc *http.Client, meta *hl.Meta, vault, account string, spot *hl.SpotMeta, useWS bool) (ex *hl.Exchange, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	ex = hl.NewExchange(ctx, key, url, meta, vault, account, spot, nil,
		hl.ExchangeOptClientOptions(hl.ClientOptHTTPClient(httpc)),
		hl.ExchangeOptWebsocket(useWS))
	return
}

// Info returns the read client.
func (c *Client) Info() *hl.Info { return c.info }

// Meta returns the market metadata store.
func (c *Client) Meta() *MetaStore { return c.meta }

// Network returns the active network.
func (c *Client) Network() string { return c.network }

// QueryAddr returns the read target address (master/sub).
func (c *Client) QueryAddr() string { return c.queryAddr }

// AgentAddress returns the loaded agent address, or "" if no write has occurred.
func (c *Client) AgentAddress() string {
	c.exMu.Lock()
	defer c.exMu.Unlock()
	if c.agent == nil {
		return ""
	}
	return c.agent.Address
}

// requireQueryAddr ensures reads have a configured target (§4).
func (c *Client) requireQueryAddr() error {
	if c.queryAddr == "" {
		return output.Auth("no_address",
			"no query address: set wallet.master_address (or pass --account) — reads use the MASTER address, never the agent").
			WithHint("deliverator config set wallet.master_address 0x...")
	}
	return nil
}

// RequireQueryAddr is the exported guard for command-layer callers (e.g. the
// `info @` expansion) that resolve the query address themselves but must fail
// with the same auth error (exit 30) as a dedicated read when none is set.
func (c *Client) RequireQueryAddr() error { return c.requireQueryAddr() }

// metaRetryFloor bounds how often a FAILING refresh is retried, so a venue
// outage cannot turn every order into an extra /info round-trip against an
// endpoint that is already unhealthy.
const metaRetryFloor = 30 * time.Second

// ensureMetaFresh honours state.meta_ttl_secs at USE rather than at construction.
//
// It is deliberately best-effort: a refresh failure leaves the existing universe
// in place and records a warning rather than blocking. Refusing to trade because
// metadata could not be re-fetched would convert a read problem into an outage,
// and the previous universe is still the best information available. What it must
// never do is let a SILENTLY stale szDecimals round a signed order — hence the
// warning surfaced alongside every write while the refresh is failing.
//
// Disabled (a no-op) when metaTTL <= 0 or there is no Info to fetch through,
// which covers every hand-built test client.
func (c *Client) ensureMetaFresh(ctx context.Context) {
	if c.metaTTL <= 0 || c.info == nil || c.meta == nil {
		return
	}
	if c.meta.Age() < c.metaTTL {
		return // the common path: one time comparison, no lock
	}

	c.metaMu.Lock()
	defer c.metaMu.Unlock()
	// Re-check under the lock: several connection goroutines can arrive at the
	// same expiry, and only the first should fetch.
	if c.meta.Age() < c.metaTTL {
		return
	}
	if !c.metaAttempt.IsZero() && time.Since(c.metaAttempt) < metaRetryFloor {
		return // a recent attempt failed; do not hammer the endpoint per order
	}
	c.metaAttempt = time.Now()

	meta, err := c.info.Meta(ctx)
	if err != nil {
		c.metaLastErr = err
		return // keep the working universe
	}
	// Spot is fetched separately and is allowed to fail: MetaStore.Refresh keeps
	// the previous spot universe rather than deleting it.
	spot, _ := c.info.SpotMeta(ctx)
	if err := c.meta.Refresh(meta, spot, time.Now()); err != nil {
		c.metaLastErr = err
		return
	}
	c.metaLastErr = nil
	if c.metaPath != "" {
		_ = c.meta.Save(c.metaPath)
	}
}

// metaStaleWarnings reports that the universe is past its TTL and the refresh is
// failing, so a write signed against possibly-stale precision says so in its
// envelope instead of being silently wrong.
func (c *Client) metaStaleWarnings() []string {
	if c.metaTTL <= 0 || c.meta == nil {
		return nil
	}
	c.metaMu.Lock()
	lastErr := c.metaLastErr
	c.metaMu.Unlock()
	if lastErr == nil {
		return nil
	}
	age := c.meta.Age()
	if age < c.metaTTL {
		return nil
	}
	return []string{fmt.Sprintf(
		"market metadata is %s old (past the %s meta_ttl_secs) and the refresh is failing (%v) — "+
			"sizes/prices are being rounded with possibly-stale szDecimals",
		age.Round(time.Second), c.metaTTL, lastErr)}
}

// GuardGeneration reports which on-disk config the gates are currently
// enforcing, for the rejection envelope.
func (c *Client) GuardGeneration() string {
	c.guardMu.RLock()
	defer c.guardMu.RUnlock()
	return c.guardGen.String()
}

// ReloadGuardsIfChanged re-reads the risk/automation config when the file on
// disk has changed, and swaps the snapshot the gates read.
//
// WHY. `deliverator config set` writes the file and exits; a long-lived `serve`
// had already captured risk config at startup and kept enforcing it from memory.
// The operator raised max_drawdown_pct on disk, `deliverator risk` (a fresh
// fork) correctly reported the new value, and the socket rejected 3,000+
// placements against the old one for two days. The fork path and the serve path
// disagreeing is what made it invisible. See #41.
//
// FAILING CLOSED IS THE WHOLE POINT. Config is where the risk caps live, so
// "could not read it" must never become "there are no caps". A missing, empty,
// or unparseable file KEEPS THE CURRENT GUARDS rather than resetting to a zero
// config — every cap is `0 = off`, so a truncated file would silently disable
// the account's entire risk envelope. A zero-length file is explicitly treated
// like a missing one: it parses as valid TOML into an all-zero Config, which is
// exactly the gutted-file shape that must not be honoured.
//
// A legitimate widening (the case that motivated this) IS applied — but it is
// reported, never silent, per the standing rule that a cap must not widen
// quietly.
//
// Returns the warnings to attach to the request's envelope.
func (c *Client) ReloadGuardsIfChanged(path string) []string {
	if path == "" {
		path = config.Path()
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Size() == 0 {
		c.guardMu.Lock()
		defer c.guardMu.Unlock()
		if !c.guardGen.Loaded {
			return nil // never successfully stamped; nothing new to say
		}
		what := "unreadable"
		if err == nil {
			what = "zero-length"
		}
		c.guardGen.Loaded = false
		return []string{fmt.Sprintf(
			"risk config at %s is %s — KEEPING the previously loaded caps rather than "+
				"falling back to an empty config (every cap is `0 = off`, so a truncated "+
				"file would disable the whole risk envelope)", path, what)}
	}

	gen := guardGeneration{ModTimeMs: fi.ModTime().UnixMilli(), Size: fi.Size(), Loaded: true}

	c.guardMu.RLock()
	cur := c.guardGen
	c.guardMu.RUnlock()
	if cur.Loaded && cur.ModTimeMs == gen.ModTimeMs && cur.Size == gen.Size {
		return nil // unchanged — the common path, one stat and done
	}

	cfg, lerr := config.Load(path)
	if lerr != nil || cfg == nil {
		c.guardMu.Lock()
		defer c.guardMu.Unlock()
		c.guardGen.Loaded = false
		return []string{fmt.Sprintf(
			"risk config at %s changed but does not parse (%v) — KEEPING the previously "+
				"loaded caps; fix the file, the gates are still enforcing the last good one",
			path, lerr)}
	}

	next := guardConfigFrom(cfg)

	c.guardMu.Lock()
	prev := c.guards
	c.guards = next
	c.guardGen = gen
	c.guardMu.Unlock()

	changes := describeRiskChanges(prev, next)
	if len(changes) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("risk config reloaded from disk (%s): %s",
		gen.String(), strings.Join(changes, "; "))}
}

// describeRiskChanges names every risk cap that moved between two snapshots,
// calling out the safety-reducing direction explicitly. A widened or disabled
// cap must be legible in the envelope — silently loosening the account's limits
// is the failure mode this whole path is built to avoid.
func describeRiskChanges(prev, next *guardConfig) []string {
	if prev == nil || next == nil {
		return nil
	}
	var out []string
	for _, f := range []struct {
		name     string
		old, new float64
	}{
		{"risk.max_drawdown_pct", prev.risk.MaxDrawdownPct, next.risk.MaxDrawdownPct},
		{"risk.max_daily_loss_pct", prev.risk.MaxDailyLossPct, next.risk.MaxDailyLossPct},
		{"risk.max_daily_loss_usd", prev.risk.MaxDailyLossUSD, next.risk.MaxDailyLossUSD},
		{"risk.max_account_leverage", prev.risk.MaxAccountLeverage, next.risk.MaxAccountLeverage},
		{"risk.max_concentration_pct_per_coin", prev.risk.MaxConcentrationPctPerCoin, next.risk.MaxConcentrationPctPerCoin},
		{"risk.max_order_notional_usd", prev.risk.MaxOrderNotionalUSD, next.risk.MaxOrderNotionalUSD},
	} {
		if f.old == f.new {
			continue
		}
		switch {
		case f.new == 0:
			out = append(out, fmt.Sprintf("%s %g -> OFF (gate disabled)", f.name, f.old))
		case f.old == 0:
			out = append(out, fmt.Sprintf("%s OFF -> %g (gate enabled)", f.name, f.new))
		case f.new > f.old:
			out = append(out, fmt.Sprintf("%s %g -> %g (WIDENED)", f.name, f.old, f.new))
		default:
			out = append(out, fmt.Sprintf("%s %g -> %g (tightened)", f.name, f.old, f.new))
		}
	}
	return out
}

// exchange lazily loads the agent key and builds the signing client.
func (c *Client) exchange(ctx context.Context) (*hl.Exchange, error) {
	c.exMu.Lock()
	defer c.exMu.Unlock()
	if c.ex != nil {
		return c.ex, nil
	}
	// Canonicalize the master synonyms (""/main/master/default, any case) to the
	// "main" alias onboard/init store the default key under — a raw "--account
	// master" must not miss the keychain entry "agent:main" and fail exit 30 on
	// a correctly onboarded box.
	ag, err := wallet.Load(config.CanonicalAccount(c.opts.Account))
	if err != nil {
		if errors.Is(err, wallet.ErrNoAgentKey) {
			return nil, output.Auth("no_agent_key", err.Error()).
				WithHint("run `deliverator onboard` to add your API wallet key to the keychain")
		}
		return nil, output.Auth("agent_key", "load agent key: "+err.Error()).
			WithHint("the agent key is read from the OS keychain — re-run `deliverator onboard` if it is missing or keychain access was denied")
	}
	c.agent = ag
	account := c.queryAddr
	if account == "" {
		account = ag.Address
	}
	// Assert the agent↔account binding (audit #91 / T3-keybind). HL enforces the
	// approval at submit time (a wrong agent is rejected — fail-closed), and it
	// exposes no approved-agents list to check locally, so we (a) record the
	// binding to the audit trail on the one key-load per process, making a
	// wrong-account session reviewable, and (b) raise a false-positive-free
	// warning for the one locally-detectable misconfig: the loaded "agent" key IS
	// the master key (it can withdraw — the non-custodial design expects a
	// separate, withdrawal-incapable API wallet).
	c.signerWarn = signerWarnFor(c.cfg.Wallet.MasterAddress, ag.Address)
	c.audit.Append(map[string]any{"action": "signer_bind", "agent": ag.Address, "account": account, "master_key": c.signerWarn != ""})
	// transport = "ws" routes SIGNED ACTIONS over the socket. Read from cfg, not
	// the hot-reloadable guard snapshot: the transport is fixed for the life of
	// the connection, and swapping the pipe under in-flight writes is not a thing
	// a config reload should be able to do.
	useWS := c.cfg != nil && c.cfg.Transport == "ws"
	ex, err := safeNewExchange(ctx, ag.Key, c.signURL, c.httpc, c.meta.Meta(), c.vaultAddr, account, c.meta.SpotMeta(), useWS)
	if err != nil {
		return nil, output.Network("exchange_init", "build signer: "+err.Error()).Retry()
	}
	// Teach the signer the HIP-3 sub-dex asset ids so "<dex>:<coin>" orders sign
	// with the right asset (the Exchange builds its Info from main meta + spot).
	for _, e := range c.meta.PerpDexEntries() {
		ex.Info().RegisterPerpDex(e.Index, e.Meta)
	}
	// Likewise teach it the HIP-4 outcome asset ids so "#<encoding>" orders sign.
	if om := c.meta.OutcomeMeta(); om != nil {
		ex.Info().RegisterOutcomes(om)
	}
	c.ex = ex
	return ex, nil
}

// InfoPost issues a raw POST to /info — used for endpoints internal/hl doesn't surface
// (userRateLimit, maxBuilderFee). Decodes the JSON response into out.
func (c *Client) InfoPost(ctx context.Context, body map[string]any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.infoURL, "/")+"/info", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return output.RateLimit("ip_rate_limited", "Hyperliquid returned 429 (per-IP weight exceeded)").WithRetryAfter(2000)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return output.Exchange("info_http", fmt.Sprintf("info request failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg))))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	// Cap the success path like the error path above: read one byte past the cap
	// to detect an oversized body and fail closed, rather than decoding straight
	// off an unbounded network reader (audit #91 / S8).
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxInfoBodyBytes+1))
	if err != nil {
		return err
	}
	if len(respBody) > maxInfoBodyBytes {
		return output.Exchange("info_too_large", fmt.Sprintf("info response exceeded %d-byte limit", maxInfoBodyBytes))
	}
	return json.Unmarshal(respBody, out)
}

// MeasureSkew returns serverMs − localMs derived from the response Date header,
// used by `connect` and the clock guard. A failure returns (0, err).
func (c *Client) MeasureSkew(ctx context.Context) (int64, error) {
	b, _ := json.Marshal(map[string]any{"type": "exchangeStatus"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.infoURL, "/")+"/info", bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	local := time.Now()
	resp, err := c.httpc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	dateHdr := resp.Header.Get("Date")
	if dateHdr == "" {
		return 0, fmt.Errorf("no Date header")
	}
	serverT, err := http.ParseTime(dateHdr)
	if err != nil {
		return 0, err
	}
	// Account for round-trip: compare server time to the midpoint of the request.
	mid := local.Add(time.Since(local) / 2)
	return serverT.UnixMilli() - mid.UnixMilli(), nil
}

// writeWarnings is what every signing path prepends to its result warnings: the
// signer-binding warning plus, when the market universe is past its TTL and the
// refresh is failing, a note that precision may be stale. Both describe the
// conditions under which the order that just got signed might be wrong, which is
// exactly what belongs in that envelope.
func (c *Client) writeWarnings() []string {
	return append(c.signerWarnings(), c.metaStaleWarnings()...)
}

// UniverseGeneration names the HIP-4 universe the engine is currently enforcing,
// and — critically — whether the SIGNER agrees with it.
//
// The stamp alone would be a half-measure. #43 was precisely a case where the
// meta store and the signer's hl.Info held DIFFERENT universes: the read path
// resolved a rolled-to coin, the order passed every gate, got signed, and then
// died at "coin not found in info" (exit 50, non-retryable). Reporting the meta
// store's view while the signer holds another would have described that outage as
// healthy. So this cross-checks the two resolvers and says so when they disagree.
//
// Before the first write there is no signer to check, which is reported honestly
// rather than as agreement.
func (c *Client) UniverseGeneration() string {
	if c.meta == nil {
		return "universe not loaded"
	}
	stamp := c.meta.UniverseStamp()

	c.exMu.Lock()
	ex := c.ex
	c.exMu.Unlock()
	if ex == nil {
		return stamp.String() + ", signer not yet built"
	}

	// Every coin the read path resolves must also resolve on the signer.
	info := ex.Info()
	var missing []string
	for _, coin := range c.meta.OutcomeCoins() {
		if _, ok := info.CoinToAsset(strings.ToUpper(coin)); !ok {
			missing = append(missing, coin)
			if len(missing) >= 3 {
				break
			}
		}
	}
	if len(missing) == 0 {
		return stamp.String() + ", signer in sync"
	}
	return fmt.Sprintf("%s, SIGNER OUT OF SYNC — %s do not resolve for signing "+
		"(an order on them would pass the gates, get signed, then be rejected exit 50)",
		stamp.String(), strings.Join(missing, ", "))
}
