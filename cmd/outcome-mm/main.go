// Command outcome-mm is the HIP-4 outcome market maker — a second binary in the
// deliverator module that drives the SAME internal/core client the CLI does, so it
// inherits every guardrail (pre-trade checks, portfolio gates, the outcome
// settlement gate) and signs with the same non-withdraw agent key. It is a long-lived
// daemon: run it under launchd/systemd with restart-on-crash, not cron.
//
//	outcome-mm run                 # foreground TUI dashboard
//	outcome-mm run --headless      # daemon: no TUI, periodic JSON status on stdout
//	outcome-mm run --dry-run       # shadow: compute + render quotes, never sign
//
// It NEVER signs unless the operator has explicitly set mm.enabled = true and
// mm.dry_run = false in the config (and did not pass --dry-run) — a fresh install
// runs in shadow.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/mm/engine"
	"github.com/erickuhn19/deliverator/internal/mm/oms"
	mmtui "github.com/erickuhn19/deliverator/internal/tui/mm"
	"github.com/erickuhn19/deliverator/internal/wallet"
)

var (
	flagConfig   string
	flagNetwork  string
	flagAccount  string
	flagDryRun   bool
	flagHeadless bool
)

func main() {
	root := &cobra.Command{
		Use:           "outcome-mm",
		Short:         "HIP-4 outcome market maker (deliverator)",
		Long:          "A specialized market maker for Hyperliquid HIP-4 outcome markets. It consumes internal/core in-process, so every order passes the same risk gauntlet as the deliverator CLI and signs with the same agent key.",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&flagConfig, "config", "", "config file path (default ~/.config/deliverator/config.toml)")
	pf.StringVar(&flagNetwork, "network", "", "override network: mainnet | testnet")
	pf.StringVar(&flagAccount, "account", "", "account alias to act on (default: master)")

	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run the market maker (TUI, or --headless daemon)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run()
		},
	}
	runCmd.Flags().BoolVar(&flagDryRun, "dry-run", false, "shadow mode: compute + render quotes, never sign")
	runCmd.Flags().BoolVar(&flagHeadless, "headless", false, "no TUI; emit periodic JSON status (for a daemon)")
	root.AddCommand(runCmd)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "outcome-mm:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(flagConfig)
	if err != nil {
		return err
	}
	if flagNetwork != "" {
		cfg.Network = flagNetwork
		if verr := cfg.Validate(); verr != nil {
			return verr
		}
	}
	mmc := cfg.MMOrDefault()

	// Build the guarded client (loads/caches meta; does not load the signing key —
	// that happens lazily on the first write).
	bctx, bcancel := context.WithTimeout(context.Background(), 30*time.Second)
	client, err := core.New(bctx, cfg, core.Options{Account: flagAccount, DryRun: flagDryRun, Timeout: 15 * time.Second})
	bcancel()
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	if err := preflight(cfg, client); err != nil {
		return err
	}

	// Load the daily-rotating outcome universe so "#<enc>" coins resolve and sign.
	octx, ocancel := context.WithTimeout(context.Background(), 30*time.Second)
	oerr := client.EnsureOutcomes(octx)
	ocancel()
	if oerr != nil {
		return fmt.Errorf("load outcome markets: %w", oerr)
	}

	feed := oms.NewFeed(mmc.Selection.PriceableUnderlyings, 0.94)
	eng := engine.New(engine.Deps{Client: client, Cfg: cfg, Feed: feed, DryRun: flagDryRun, ConfigPath: flagConfig})

	effectiveDry := flagDryRun || mmc.DryRun || !mmc.Enabled
	logStartup(cfg, mmc, effectiveDry)

	// SIGINT/SIGTERM → cancel → engine teardown cancels every resting quote. The DMS
	// armed inside the engine is the backstop if this graceful path itself fails.
	sctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(sctx)
	defer cancel()

	if flagHeadless {
		go statusLogger(ctx, eng)
		return eng.Run(ctx)
	}

	// TUI mode: engine in the background, dashboard in the foreground. Quitting the
	// dashboard cancels the engine and waits for its teardown.
	errc := make(chan error, 1)
	go func() { errc <- eng.Run(ctx) }()
	tuiErr := mmtui.Run(ctx, mmtui.Deps{
		Engine:   eng,
		Panic:    func(c context.Context) error { _, e := client.Panic(c); return e },
		SetHalt:  core.SetHalt,
		SetMMKey: guardedSetMMKey,
		// The money switch: flip runtime signing, then persist so a restart keeps it.
		SetLive: func(on bool) error {
			if err := eng.SetLive(on); err != nil {
				return err
			}
			if err := guardedSetMMKey("mm.enabled", strconv.FormatBool(on)); err != nil {
				return err
			}
			return guardedSetMMKey("mm.dry_run", strconv.FormatBool(!on))
		},
		Network:   cfg.Network,
		AuditPath: auditPath(cfg),
		Selection: mmc.Selection, // seed pin/blacklist/max-active mirrors from config
		Strategy:  mmc.Strategy,  // seed the size mirror
	})
	cancel()
	<-errc // let the engine finish cancelling resting quotes
	return tuiErr
}

// preflight fails closed before quoting: it requires an agent key for live signing,
// a reachable API, and a clock inside the nonce window. A large but tolerable skew is
// a warning; a dangerous one aborts.
func preflight(cfg *config.Config, client core.ClientAPI) error {
	live := !flagDryRun && cfg.MMOrDefault().Enabled && !cfg.MMOrDefault().DryRun
	if !wallet.Has(config.CanonicalAccount(flagAccount)) {
		if live {
			return fmt.Errorf("no agent key for account %q — run `deliverator onboard` (or set %s for headless) before live quoting",
				accountOrMain(), wallet.EnvKeyVar)
		}
		fmt.Fprintln(os.Stderr, "outcome-mm: warning: no agent key found — shadow mode only until `deliverator onboard`")
	}
	if cfg.Wallet.MasterAddress == "" {
		return fmt.Errorf("wallet.master_address is not set — reads (positions, book) require it")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if skew, err := client.MeasureSkew(ctx); err == nil {
		switch {
		case skew > 30000 || skew < -30000:
			return fmt.Errorf("clock skew %d ms is outside the nonce window — fix NTP before quoting (signed orders would be rejected)", skew)
		case skew > 2000 || skew < -2000:
			fmt.Fprintf(os.Stderr, "outcome-mm: warning: clock skew %d ms — check NTP\n", skew)
		}
	}
	if client.Halted() {
		fmt.Fprintln(os.Stderr, "outcome-mm: warning: global halt is active — no orders will be placed until `deliverator halt off`")
	}
	return nil
}

// guardedSetMMKey mirrors the console's setCapGuarded: load a FRESH config, apply the
// [mm] edit, validate, and save atomically — never mutating the running config.
func guardedSetMMKey(key, val string) error {
	fresh, err := config.Load(flagConfig)
	if err != nil {
		return err
	}
	target := fresh.SourcePath()
	if target == "" {
		target = config.Path()
	}
	if err := fresh.SetMMKey(key, val); err != nil {
		return err
	}
	if err := fresh.Validate(); err != nil {
		return err
	}
	return fresh.Save(target)
}

func logStartup(cfg *config.Config, mmc *config.MM, dry bool) {
	mode := "LIVE"
	reason := ""
	if dry {
		mode = "SHADOW (dry-run)"
		switch {
		case flagDryRun:
			reason = " [--dry-run]"
		case !mmc.Enabled:
			reason = " [mm.enabled=false]"
		case mmc.DryRun:
			reason = " [mm.dry_run=true]"
		}
	}
	fmt.Fprintf(os.Stderr, "outcome-mm: %s%s | network=%s | active<=%d | quote=%dms | scan=%ds | blackout=%dm\n",
		mode, reason, cfg.Network, mmc.Selection.MaxActiveMarkets, mmc.QuoteIntervalMs,
		mmc.Selection.ScanIntervalSecs, mmc.Settle.BlackoutMins)
}

// statusLogger emits a compact JSON status line periodically in headless mode, for a
// daemon's log pipeline.
func statusLogger(ctx context.Context, eng *engine.Engine) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	enc := json.NewEncoder(os.Stdout)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			v := eng.View()
			line := map[string]any{
				"ts": time.Now().UTC().Format(time.RFC3339), "running": v.Running, "dry_run": v.DryRun,
				"paused": v.Paused, "halted": v.Halted, "warmup": v.Warmup, "equity": v.Equity,
				"active": len(v.Active), "pnl_net": v.PnL.Net, "pnl_realized": v.PnL.Realized,
				"pnl_open": v.PnL.Open, "fees": v.PnL.Fees, "last_error": v.LastError,
			}
			_ = enc.Encode(line)
		}
	}
}

func auditPath(cfg *config.Config) string {
	if cfg.State.AuditPath != "" {
		return config.ExpandPath(cfg.State.AuditPath)
	}
	return filepath.Join(config.Dir(), "audit.jsonl")
}

func accountOrMain() string {
	if flagAccount == "" {
		return "main"
	}
	return flagAccount
}
