package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
)

// clientOpts builds core options from the resolved global flags.
func clientOpts() core.Options {
	return core.Options{
		Account:     flagAccount,
		RefreshMeta: flagRefreshMeta,
		NoAudit:     flagNoAudit,
		DryRun:      flagDryRun,
		Strict:      flagStrict,
		Timeout:     flagTimeout,
	}
}

// newClient builds the client a command acts through. It is a var (not a plain
// func) so tests can swap in a fake core.ClientAPI and exercise RunE handlers
// offline — no network, no keychain (#102). Production always returns *core.Client.
var newClient = func(ctx context.Context) (core.ClientAPI, error) {
	c, err := core.New(ctx, Cfg, clientOpts())
	if err != nil {
		return nil, err
	}
	// HIP-4 outcome markets rotate daily and cost an extra fetch, so they load on
	// demand rather than via a config flag that defaults off (which made
	// `markets --class outcome` silently return empty). If this command names a
	// "#<enc>" coin or asks for --class outcome|all, load the outcome universe now
	// so it resolves/signs/lists.
	if argsReferenceOutcomes() {
		if err := c.EnsureOutcomes(ctx); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// argsReferenceOutcomes reports whether this invocation touches HIP-4 outcomes —
// a "#<enc>" coin token (the unambiguous outcome marker; nothing else starts with
// '#') or a `--class outcome|all` selector. Used to lazily load the outcome
// universe (see newClient).
func argsReferenceOutcomes() bool {
	for i, a := range os.Args {
		if strings.HasPrefix(a, "#") {
			return true
		}
		if a == "--class" && i+1 < len(os.Args) {
			v := strings.ToLower(os.Args[i+1])
			if v == "outcome" || v == "all" {
				return true
			}
		}
		if strings.EqualFold(a, "--class=outcome") || strings.EqualFold(a, "--class=all") {
			return true
		}
	}
	return false
}

// cmdCtx bounds a whole command. It must cover the multi-call commands (e.g.
// portfolio = 3 reads, connect = meta fetch + skew), each itself bounded by the
// per-request HTTP timeout.
func cmdCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), flagTimeout*6+10*time.Second)
}

// legCtx bounds a single write "leg" (one Place/Close/Cancel). panic and bracket
// flows give each leg its own budget so one slow leg can't starve the rest.
func legCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), flagTimeout+5*time.Second)
}

// asError coerces any error into a categorized *output.Error, preserving the
// category of a wrapped *output.Error.
func asError(err error) *output.Error {
	var oe *output.Error
	if errors.As(err, &oe) {
		return oe
	}
	return output.Unknown("error", err.Error())
}

// AlertEmitter is wired in PersistentPreRunE; nil/disabled until then.
var AlertEmitter *output.Emitter

func fail(cmd string, err error) error {
	oe := asError(err)
	// Best-effort RED-state alert (no-op unless a webhook is configured and the
	// category is in the alert set). Synchronous but short-timeout: the CLI exits
	// right after, so the POST must finish first; it never changes the outcome.
	if AlertEmitter.Enabled() && Cfg != nil {
		AlertEmitter.Fire(output.AlertEvent{
			Ts: output.Now(), ExitCode: oe.ExitCode(), Category: string(oe.Category),
			Code: oe.Code, Message: oe.Message, Cmd: cmd, Network: Cfg.Network, Account: flagAccount,
		})
	}
	return output.Fail(cmd, oe, RootMeta(0))
}

func emit(cmd string, data any, warnings ...string) {
	output.Emit(output.Response{Cmd: cmd, Data: data, Warnings: warnings, Meta: RootMeta(0)})
}

// runRead is the standard read-command shell: build a client, call fn, emit.
func runRead(cmd string, fn func(context.Context, core.ClientAPI) (any, error)) error {
	ctx, cancel := cmdCtx()
	defer cancel()
	c, err := newClient(ctx)
	if err != nil {
		return fail(cmd, err)
	}
	data, err := fn(ctx, c)
	if err != nil {
		return fail(cmd, err)
	}
	emit(cmd, data)
	return nil
}

// runReadWarn is runRead for a read that also returns top-level warnings (e.g.
// snapshot listing which sections failed) — the read still succeeds overall.
func runReadWarn(cmd string, fn func(context.Context, core.ClientAPI) (any, []string, error)) error {
	ctx, cancel := cmdCtx()
	defer cancel()
	c, err := newClient(ctx)
	if err != nil {
		return fail(cmd, err)
	}
	data, warnings, err := fn(ctx, c)
	if err != nil {
		return fail(cmd, err)
	}
	emit(cmd, data, warnings...)
	return nil
}

func ptrI64(v int64) *int64 { return &v }

// floatIfSet returns &v only when the named flag was explicitly set on the command
// line, else nil — so a bound filter (e.g. --min-pnl) distinguishes "0 means zero"
// from "unset, don't filter".
func floatIfSet(f *pflag.FlagSet, name string, v float64) *float64 {
	if f.Changed(name) {
		return &v
	}
	return nil
}

// requireSubcommand is the RunE for grouping commands that have subcommands but
// no action of their own (config, account, builder, referral, stream, and root).
// Without it, cobra falls back to Help() — printing raw usage text to stdout and
// returning nil — so a bare `deliverator --json config` (or an unknown subcommand)
// emits non-JSON on stdout and exits 0, violating the envelope contract an agent
// relies on. Under JSON we emit a validation envelope (exit 10); a human at a TTY
// still gets the help text.
func requireSubcommand(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return fail(cmd.Name(), output.Validation("unknown_subcommand",
			fmt.Sprintf("unknown subcommand %q for %q", args[0], cmd.CommandPath())).
			WithHint("run "+cmd.CommandPath()+" --help for the available subcommands"))
	}
	if output.JSONMode() {
		return fail(cmd.Name(), output.Validation("subcommand_required",
			cmd.CommandPath()+" requires a subcommand").
			WithHint("run "+cmd.CommandPath()+" --help for the available subcommands"))
	}
	return cmd.Help()
}
