// Package cmd implements the Deliverator command tree (cobra). Commands are thin
// adapters: they parse flags, call internal/core, and emit a schema-v1 envelope
// via internal/output. All safety + correctness lives in core, never here (§3.6).
package cmd

import (
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
	"github.com/erickuhn19/deliverator/internal/state"
)

// Global flags (persistent on root, available to every command).
var (
	flagConfig      string
	flagJSON        bool
	flagNoJSON      bool
	flagNetwork     string
	flagAccount     string
	flagDryRun      bool
	flagStrict      bool
	flagRefreshMeta bool
	flagNoAudit     bool
	flagYes         bool
	flagTimeout     time.Duration
)

// Process-wide state resolved once in PersistentPreRunE.
var (
	// Cfg is the loaded, validated config. Never nil after PreRun.
	Cfg *config.Config
	// ToolsMarkdown holds the embedded TOOLS.md, injected by main().
	ToolsMarkdown string
)

var rootCmd = &cobra.Command{
	Use:           "deliverator",
	Short:         "Non-custodial Hyperliquid execution + tracking CLI for autonomous agents",
	Long:          "Deliverator is the safe harness between an autonomous agent and a Hyperliquid account.\nIt signs with an agent/API wallet that cannot withdraw, enforces hard risk caps, and\nspeaks a deterministic, machine-parseable JSON contract. See `deliverator tools`.",
	SilenceErrors: true, // we render errors as envelopes ourselves
	SilenceUsage:  true,
	// A bare `deliverator` (or an unknown command) must emit an envelope under
	// --json, not cobra's raw help text + exit 0. See requireSubcommand.
	RunE: requireSubcommand,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Load config over defaults. A missing file is fine (fresh box).
		cfg, err := config.Load(flagConfig)
		if err != nil {
			return output.Fail("config",
				output.Validation("config_invalid", err.Error()).
					WithHint("fix "+config.Path()+" or pass --config"),
				output.Meta{Network: flagNetwork, Account: accountOrDefault()})
		}
		if flagNetwork != "" {
			cfg.Network = flagNetwork
			if verr := cfg.Validate(); verr != nil {
				return output.Fail("config",
					output.Validation("network_invalid", verr.Error()),
					output.Meta{Account: accountOrDefault()})
			}
		}
		Cfg = cfg

		// Wire the RED-state alert webhook (env overrides config). Off by default.
		webhook := cfg.Alerting.WebhookURL
		if env := os.Getenv("DELIVERATOR_ALERT_WEBHOOK"); env != "" {
			webhook = env
		}
		AlertEmitter = output.NewEmitter(webhook, cfg.Alerting.Categories, cfg.Alerting.TimeoutSec)

		// Re-resolve output mode now that config (json_when_not_tty) is known.
		output.Configure(resolveJSONMode(cfg), cmd.OutOrStdout())
		return nil
	},
}

// Execute runs the root command and maps the result to a process exit code.
// The failure envelope is always written before exit; main must not re-print.
func Execute() {
	// Bootstrap output mode so even pre-PreRun errors (bad flags) emit JSON.
	output.Configure(resolveJSONMode(config.Default()), os.Stdout)

	err := rootCmd.Execute()
	// A *CmdError already emitted its failure envelope at the call site; anything
	// else unhandled (cobra arg parsing, unknown command, …) needs one here.
	if err != nil {
		if _, ok := err.(*output.CmdError); !ok {
			_ = output.Fail("deliverator",
				output.Validation("args", err.Error()).WithHint("see --help"),
				RootMeta(0))
		}
	}
	code := exitCodeFor(err)
	logInvocation(code) // best-effort command log for live oversight
	os.Exit(code)
}

// commandLogPath resolves where to append the per-invocation command log:
// $DELIVERATOR_COMMAND_LOG (session override) else state.command_log. "" = off.
func commandLogPath() string {
	if p := os.Getenv("DELIVERATOR_COMMAND_LOG"); p != "" {
		return config.ExpandPath(p)
	}
	if Cfg != nil && Cfg.State.CommandLog != "" {
		return config.ExpandPath(Cfg.State.CommandLog)
	}
	return ""
}

// logInvocation appends one JSONL line — timestamp, full argv, exit code — to the
// command log when configured, so a human can watch every command the CLI (or an
// agent driving it) runs, live, in a second terminal. Best-effort: a logging
// failure never changes the command's outcome. argv never contains secrets (the
// key lives in the keychain / stdin, never on the command line).
func logInvocation(exit int) {
	path := commandLogPath()
	if path == "" {
		return
	}
	state.NewAudit(path, true).Append(map[string]any{
		"ts": output.Now(), "argv": os.Args[1:], "exit": exit, "ok": exit == 0,
	})
}

// exitCodeFor maps a command result to a process exit code: success is ExitOK, a
// *output.CmdError carries its own code, and any other non-nil error is a
// validation failure (the unhandled-cobra path). Pure so it is unit-testable.
func exitCodeFor(err error) int {
	if err == nil {
		return output.ExitOK
	}
	if ce, ok := err.(*output.CmdError); ok {
		return ce.Code
	}
	return output.ExitValidation
}

func init() {
	pf := rootCmd.PersistentFlags()
	pf.StringVar(&flagConfig, "config", "", "config file path (default ~/.config/deliverator/config.toml)")
	pf.BoolVar(&flagJSON, "json", false, "force machine JSON output")
	pf.BoolVar(&flagNoJSON, "no-json", false, "force human tables (for an operator at a shell)")
	pf.StringVar(&flagNetwork, "network", "", "override network: mainnet | testnet")
	pf.StringVar(&flagAccount, "account", "", "account alias to act on (default: master)")
	pf.BoolVar(&flagDryRun, "dry-run", false, "validate/round/attach but never sign or send")
	pf.BoolVar(&flagStrict, "strict", false, "reject on precision instead of auto-rounding (exit 11)")
	pf.BoolVar(&flagRefreshMeta, "refresh-meta", false, "force-refresh the cached market metadata")
	pf.BoolVar(&flagNoAudit, "no-audit", false, "do not append to the local audit log")
	pf.BoolVar(&flagYes, "yes", false, "skip the confirmation on destructive human commands")
	pf.DurationVar(&flagTimeout, "timeout", 15*time.Second, "per-request timeout")
}

// resolveJSONMode decides JSON vs human output:
//   - explicit --no-json / --json win;
//   - a TTY defaults to human;
//   - a pipe follows config.json_when_not_tty (default true).
func resolveJSONMode(cfg *config.Config) bool {
	if flagNoJSON {
		return false
	}
	if flagJSON {
		return true
	}
	if isTerminal(os.Stdout) {
		return false
	}
	return cfg.Automation.JSONWhenNotTTY
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func accountOrDefault() string {
	if flagAccount == "" {
		return "main"
	}
	return flagAccount
}

// RootMeta builds the per-call meta block from resolved global state.
func RootMeta(weight int) output.Meta {
	net := flagNetwork
	if Cfg != nil && net == "" {
		net = Cfg.Network
	}
	return output.Meta{Network: net, Account: accountOrDefault(), WeightUsed: weight}
}
