package cmd

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/tui"
)

var consoleCmd = &cobra.Command{
	Use:   "console",
	Short: "Mission control: live risk envelope + trading posture (editable) + account + activity (TUI)",
	Long: `A human-in-the-loop full-screen dashboard. The agent drives execution through
the normal CLI; this is the operator's surface — view and edit the risk envelope (with
live utilization) and the trading posture (what the agent may trade: outcome markets,
limit-only, allowed coins, sub-dexes), watch the command/audit activity feed, and glance
at equity + open positions. Risk-cap edits go through the same guarded path as ` + "`config set`" + `
(loud, operator-confirmed, never silent); posture changes take a plain confirm.
↑↓ select · e edit/toggle · r refresh · q quit.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		// One reusable client: meta fetch under a bounded startup context, then the
		// TUI reads on each tick with its own per-tick contexts.
		bctx, bcancel := context.WithTimeout(context.Background(), 30*time.Second)
		c, err := newClient(bctx)
		bcancel()
		if err != nil {
			return fail("console", err)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return tui.Run(ctx, tui.Deps{
			Client:     c,
			AuditPath:  consoleAuditPath(),
			CommandLog: commandLogPath(),
			Network:    consoleNetwork(),
			SetCap:     setCapGuarded,
			DMSArmed:   dmsArmedStatus,
		})
	},
}

// setCapGuarded runs the SAME guarded edit as `config set` (admin.go): load a FRESH
// config (never the in-memory Cfg), snapshot the prior value, mutate+validate via
// setConfigKey, save atomically. The TUI surfaces the loud operator-approved warning
// when isRiskCap. Deliverator never blocks the change — it makes it loud.
func setCapGuarded(key, val string) (old string, isRiskCap bool, err error) {
	fresh, err := config.Load(flagConfig)
	if err != nil {
		return "", false, err
	}
	target := fresh.SourcePath()
	if target == "" {
		target = config.Path()
	}
	old, isRiskCap = riskCapValue(fresh, key)
	if serr := setConfigKey(fresh, key, val); serr != nil {
		return old, isRiskCap, serr
	}
	if serr := fresh.Save(target); serr != nil {
		return old, isRiskCap, serr
	}
	return old, isRiskCap, nil
}

func consoleAuditPath() string {
	if Cfg != nil && Cfg.State.AuditPath != "" {
		return config.ExpandPath(Cfg.State.AuditPath)
	}
	return filepath.Join(config.Dir(), "audit.jsonl")
}

func consoleNetwork() string {
	if Cfg != nil {
		return Cfg.Network
	}
	return flagNetwork
}

func dmsArmedStatus() (bool, int) {
	s, ok := readDMS()
	if !ok {
		return false, 0
	}
	return s.DeadlineMs > time.Now().UnixMilli(), s.Secs
}

func init() {
	rootCmd.AddCommand(consoleCmd)
}
