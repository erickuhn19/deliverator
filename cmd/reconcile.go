package cmd

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
)

var (
	rcSince  int64
	rcCloids []string
)

// reconcileCmd diffs the local audit trail against live positions/orders so an
// autonomous loop can adopt reality before resuming after a crash/restart (#42).
// Exit 60 signals a divergence (an orphan order or an unknown in-flight cloid) —
// inspect before placing anything; exit 0 means local and live agree.
var reconcileCmd = &cobra.Command{
	Use:   "reconcile",
	Short: "Diff local audit state vs live positions/orders (run first after a restart)",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := cmdCtx()
		defer cancel()
		c, err := newClient(ctx)
		if err != nil {
			return fail("reconcile", err)
		}
		var suspects []string
		for _, raw := range rcCloids {
			for _, s := range strings.Split(raw, ",") {
				if s = strings.TrimSpace(s); s != "" {
					suspects = append(suspects, s)
				}
			}
		}
		res, err := c.Reconcile(ctx, core.ReconcileOpts{SinceMs: rcSince, SuspectCloids: suspects})
		if err != nil {
			return fail("reconcile", err)
		}
		emit("reconcile", res)
		if !res.Clean {
			// 60: divergence found — inspect before resuming (do not blind-resume).
			return output.ExitWith(output.ExitPartial)
		}
		return nil
	},
}

func init() {
	reconcileCmd.Flags().Int64Var(&rcSince, "since", 0, "only scan audit rows at/after this unix-ms (default: last 24h)")
	reconcileCmd.Flags().StringSliceVar(&rcCloids, "cloid", nil, "in-flight cloid(s) to resolve against live state (repeatable / comma-separated)")
	rootCmd.AddCommand(reconcileCmd)
}
