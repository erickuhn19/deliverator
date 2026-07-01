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
	"path/filepath"
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

	meta *MetaStore
	info *hl.Info

	queryAddr string // master/sub address for READS (never the agent address, §4)
	vaultAddr string // "" for master; sub-account address otherwise

	nonce *state.NonceLock
	audit *state.Audit

	// lazily initialized only when a write needs to sign
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

// signerWarnings returns the one-shot signer-binding warning (audit #91 /
// T3-keybind), or nil. Every write path prepends it to its warnings so a
// dangerous signer setup surfaces in the result envelope.
func (c *Client) signerWarnings() []string {
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
		cfg:     cfg,
		opts:    opts,
		network: cfg.Network,
		signURL: signURLFor(cfg.Network),
		httpc:   &http.Client{Timeout: opts.Timeout},
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
	c.queryAddr, _ = cfg.ResolveAddress(opts.Account)
	if c.queryAddr != "" && !strings.EqualFold(c.queryAddr, cfg.Wallet.MasterAddress) {
		c.vaultAddr = c.queryAddr // a sub-account/vault, not the master
	}

	// Meta: use a fresh cache, else fetch and persist.
	metaPath := filepath.Join(config.Dir(), "meta.json")
	ttl := time.Duration(cfg.State.MetaTTLSecs) * time.Second
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
	return nil
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

func safeNewExchange(ctx context.Context, key *ecdsa.PrivateKey, url string, httpc *http.Client, meta *hl.Meta, vault, account string, spot *hl.SpotMeta) (ex *hl.Exchange, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	ex = hl.NewExchange(ctx, key, url, meta, vault, account, spot, nil,
		hl.ExchangeOptClientOptions(hl.ClientOptHTTPClient(httpc)))
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

// exchange lazily loads the agent key and builds the signing client.
func (c *Client) exchange(ctx context.Context) (*hl.Exchange, error) {
	if c.ex != nil {
		return c.ex, nil
	}
	ag, err := wallet.Load(c.opts.Account)
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
	ex, err := safeNewExchange(ctx, ag.Key, c.signURL, c.httpc, c.meta.Meta(), c.vaultAddr, account, c.meta.SpotMeta())
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
