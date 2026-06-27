package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
)

// watch is the real-time failsafe (#51): the reactive counterpart to the DMS.
// It consumes the user-state stream and evaluates a risk metric on every frame,
// firing a guarded action (alert/dms/panic) the moment the metric breaches a
// threshold — catching a mid-interval liquidation approach a periodic tick can't.
// Like `stream`, it emits NDJSON (one envelope per evaluation + one on trigger)
// and runs until interrupted.

// watchActionCooldownFloor is the minimum debounce for state-changing actions
// (panic/dms), so a flapping metric or a failed action can't re-fire in a tight loop.
const watchActionCooldownFloor = 5 * time.Second

var (
	watchMetric   string
	watchBelow    float64
	watchAction   string
	watchCoin     string
	watchCooldown time.Duration
	watchDMSSecs  int
)

var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Real-time failsafe: watch a risk metric, trigger alert|dms|panic on breach",
	Long: `Continuously evaluate a risk metric from the live user-state stream and fire a
guarded action when it breaches a threshold. The reactive counterpart to the
dead-man's switch (which only cancels resting orders on heartbeat lapse, never
closes a position).

Metric (v1): liq_distance_pct — the minimum, across open positions, of how far
the mark can move (as a % of mark) before a position liquidates. Configured HIP-3
sub-dex positions are included (sampled at the user-state stream's cadence); a
sub-dex that is momentarily unreadable is listed in the eval's stale_dexs and
excluded from that frame's value (degraded coverage is reported, never silent).

Actions:
  alert  POST the configured alerting webhook (requires alerting.webhook_url)
  dms    arm the dead-man's switch (schedule-cancel) — cancels resting orders
  panic  cancel all orders + flatten all positions (emergency)

Best-effort, not a guarantee: like any stream consumer the monitor is blind during
a reconnect, so a breach that begins and ends entirely within a disconnect window
is caught only on the next frame. Keep a conservative DMS armed as the backstop.

Emits NDJSON (one envelope per evaluation, one on trigger) and runs until Ctrl-C.`,
	RunE: runWatch,
}

func runWatch(cmd *cobra.Command, args []string) error {
	if strings.ToLower(watchMetric) != string(core.WatchLiqDistancePct) {
		return fail("watch", output.Validation("bad_metric",
			"only --metric "+string(core.WatchLiqDistancePct)+" is supported").
			WithHint("use --metric "+string(core.WatchLiqDistancePct)))
	}
	if watchBelow <= 0 {
		return fail("watch", output.Validation("bad_threshold", "--below must be a positive number"))
	}
	action := strings.ToLower(watchAction)
	switch action {
	case "alert", "dms", "panic":
	default:
		return fail("watch", output.Validation("bad_action", "--action must be alert|dms|panic"))
	}
	if action == "alert" && !AlertEmitter.Enabled() {
		return fail("watch", output.Validation("no_webhook",
			"--action alert needs a webhook — set alerting.webhook_url or DELIVERATOR_ALERT_WEBHOOK"))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c, err := newClient(ctx)
	if err != nil {
		return fail("watch", err)
	}

	// A state-changing action (dms/panic) must not re-fire on every breaching frame
	// (~1-2s) — a failed/partial action would loop. Floor the cooldown for those.
	cooldown := watchCooldown
	var warnings []string
	if (action == "panic" || action == "dms") && cooldown < watchActionCooldownFloor {
		cooldown = watchActionCooldownFloor
		warnings = append(warnings, fmt.Sprintf("cooldown raised to %s (floor for --action %s)", cooldown, action))
	}
	cfg := core.WatchConfig{
		Metric:   core.WatchLiqDistancePct,
		Below:    watchBelow,
		Coin:     watchCoin,
		Cooldown: cooldown,
	}

	// Announce the armed monitor so the agent (and the audit trail) records what's
	// running before any evaluation arrives. Distinct cmd ("watch.start") so an
	// NDJSON consumer can tell the announce line from the per-frame "watch" evals.
	emit("watch.start", map[string]any{
		"armed": true, "metric": watchMetric, "below": watchBelow,
		"action": action, "coin": watchCoin, "cooldown_secs": int(cooldown.Seconds()),
		"dry_run": flagDryRun,
	}, warnings...)

	onEval := func(ev core.WatchEval) { emit("watch", ev) }
	onBreach := func(ev core.WatchEval) { fireWatchAction(c, action, ev) }

	if serr := c.Watch(ctx, cfg, onEval, onBreach); serr != nil {
		return fail("watch", serr)
	}
	return nil
}

// fireWatchAction dispatches the breach action. It never returns an error (a
// long-running monitor must not die on one failed action); the outcome is emitted
// as a watch.trigger envelope instead. Under --dry-run it reports what it would
// do without signing.
func fireWatchAction(c core.ClientAPI, action string, ev core.WatchEval) {
	base := map[string]any{
		"action": action, "metric": ev.Metric, "value": ev.Value,
		"threshold": ev.Threshold, "worst_coin": ev.WorstCoin,
	}
	if flagDryRun {
		base["fired"] = false
		base["dry_run"] = true
		emit("watch.trigger", base)
		return
	}

	// Each action gets its own bounded context: the stream ctx is long-lived, and
	// a failsafe action must not hang the monitor.
	actx, cancel := context.WithTimeout(context.Background(), flagTimeout*6+10*time.Second)
	defer cancel()

	switch action {
	case "alert":
		AlertEmitter.FireAlways(output.AlertEvent{
			Ts: output.Now(), ExitCode: 20, Category: string(output.CatRisk),
			Code:    "watch_breach",
			Message: fmt.Sprintf("%s=%s below %s (worst: %s)", ev.Metric, ev.Value, ev.Threshold, ev.WorstCoin),
			Cmd:     "watch", Network: Cfg.Network, Account: flagAccount,
		})
		base["fired"] = true
	case "dms":
		secs := watchDMSSecs
		if secs < 5 {
			secs = Cfg.Risk.DeadManSwitchSecs
		}
		if secs < 5 {
			base["fired"] = false
			base["error"] = "dms secs must be >= 5; set --dms-secs or risk.dead_man_switch_secs"
			break
		}
		deadline := time.Now().Add(time.Duration(secs) * time.Second).UnixMilli()
		if err := c.ScheduleCancel(actx, ptrI64(deadline)); err != nil {
			base["fired"] = false
			base["error"] = asError(err).Message
			break
		}
		writeDMS(dmsState{Secs: secs, DeadlineMs: deadline, SetAtMs: time.Now().UnixMilli()})
		core.AuditDMS(Cfg, flagNoAudit, "watch", secs, deadline)
		base["fired"] = true
		base["dms_secs"] = secs
		base["deadline_ms"] = deadline
	case "panic":
		res, err := c.Panic(actx)
		if err != nil {
			base["fired"] = false
			base["error"] = asError(err).Message
			break
		}
		base["fired"] = true
		base["complete"] = res.Complete
		base["result"] = res
	}
	emit("watch.trigger", base)
}

func init() {
	watchCmd.Flags().StringVar(&watchMetric, "metric", string(core.WatchLiqDistancePct), "metric to watch (liq_distance_pct)")
	watchCmd.Flags().Float64Var(&watchBelow, "below", 0, "trigger when the metric drops below this value")
	watchCmd.Flags().StringVar(&watchAction, "action", "alert", "action on breach: alert|dms|panic")
	watchCmd.Flags().StringVar(&watchCoin, "coin", "", "restrict the metric to one coin (default: all positions)")
	watchCmd.Flags().DurationVar(&watchCooldown, "cooldown", 60*time.Second, "minimum gap between triggers (debounce)")
	watchCmd.Flags().IntVar(&watchDMSSecs, "dms-secs", 0, "schedule-cancel window for --action dms (default: risk.dead_man_switch_secs)")
	rootCmd.AddCommand(watchCmd)
}
