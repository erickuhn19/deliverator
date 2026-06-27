package cmd

// RunE coverage for the `copy` command group (#102): diff (default, read-only),
// --execute (places the surviving legs), and the error / exit-code branches.
// Each fake embeds core.ClientAPI and overrides only Copy / CopyExecute; the
// fakes capture the params they receive so the test can assert the leader
// address + flags actually reach the call. cpExecute/cpYes/flagDryRun are
// process-wide globals, so cpResetFlags restores them after every subtest.

import (
	"bytes"
	"context"
	"testing"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
)

// cpResetFlags zeroes the copy-specific flag globals (and flagDryRun) so a
// subtest starts clean and other tests aren't polluted, restoring afterward.
func cpResetFlags(t *testing.T) {
	t.Helper()
	saveExec, saveYes, saveDry := cpExecute, cpYes, flagDryRun
	saveMirrored, saveCoins := cpMirrored, cpCoins
	saveScaleMode, saveScale := cpScaleMode, cpScale
	cpExecute, cpYes, flagDryRun = false, false, false
	cpMirrored, cpCoins = "", ""
	cpScaleMode, cpScale = "", 0
	t.Cleanup(func() {
		cpExecute, cpYes, flagDryRun = saveExec, saveYes, saveDry
		cpMirrored, cpCoins = saveMirrored, saveCoins
		cpScaleMode, cpScale = saveScaleMode, saveScale
	})
}

// --- diff (default, read-only) ---

// cpDiffClient returns a canned diff and records the params Copy was called with.
type cpDiffClient struct {
	core.ClientAPI
	got core.CopyParams
}

func (c *cpDiffClient) Copy(_ context.Context, p core.CopyParams) (*core.CopyDiff, error) {
	c.got = p
	return &core.CopyDiff{
		Leader:      p.Leader,
		ScaleMode:   "equity",
		ScaleFactor: "1",
		YourEquity:  "1000",
		Diff: []core.DiffLeg{
			{Coin: "BTC", Class: "open", Action: "buy", Size: "0.1", NotionalUSD: "6500"},
		},
		MirroredNow: []string{"BTC"},
	}, nil
}

// CopyExecute must NOT be called on the read-only diff path: a panic flags the bug.

func TestCopyCmdDiffHappy(t *testing.T) {
	cpResetFlags(t)
	cpMirrored = "ETH" // exercise splitCoins → Mirrored
	fc := &cpDiffClient{}
	withFakeClient(t, fc)

	env, err := runCmd(t, copyCmd, []string{"0xABCDEF0123456789abcdef0123456789ABCDEF01"})
	if err != nil {
		t.Fatalf("diff should succeed, got %v", err)
	}
	if !env.OK || env.Cmd != "copy.diff" {
		t.Fatalf("diff envelope wrong: %+v", env)
	}
	if !bytes.Contains(env.Data, []byte("BTC")) {
		t.Fatalf("diff data should carry the leg, got %s", env.Data)
	}
	// The positional leader address + --mirrored must reach the Copy call.
	if fc.got.Leader != "0xABCDEF0123456789abcdef0123456789ABCDEF01" {
		t.Fatalf("leader arg not forwarded: %q", fc.got.Leader)
	}
	if len(fc.got.Mirrored) != 1 || fc.got.Mirrored[0] != "ETH" {
		t.Fatalf("--mirrored not forwarded to params: %+v", fc.got.Mirrored)
	}
}

// --execute set but flagDryRun on → stays read-only (emits copy.diff, no
// CopyExecute call). Proves the `!cpExecute || flagDryRun` guard.
func TestCopyCmdExecuteDryRunStaysDiff(t *testing.T) {
	cpResetFlags(t)
	cpExecute = true
	flagDryRun = true
	withFakeClient(t, &cpDiffClient{})

	env, err := runCmd(t, copyCmd, []string{"0x0000000000000000000000000000000000000001"})
	if err != nil {
		t.Fatalf("dry-run execute should succeed read-only, got %v", err)
	}
	if !env.OK || env.Cmd != "copy.diff" {
		t.Fatalf("dry-run must emit copy.diff (not execute): %+v", env)
	}
}

// --- error from Copy ---

type cpDiffErrClient struct{ core.ClientAPI }

func (cpDiffErrClient) Copy(context.Context, core.CopyParams) (*core.CopyDiff, error) {
	return nil, output.Network("net_down", "leader fetch unreachable")
}

func TestCopyCmdDiffMethodError(t *testing.T) {
	cpResetFlags(t)
	withFakeClient(t, cpDiffErrClient{})

	env, err := runCmd(t, copyCmd, []string{"0x0000000000000000000000000000000000000002"})
	ce, ok := err.(*output.CmdError)
	if !ok || ce.Code != output.ExitNetwork {
		t.Fatalf("want network CmdError (exit %d), got %T %v", output.ExitNetwork, err, err)
	}
	if env.OK || env.Cmd != "copy" || env.Error.Category != "network" || env.Error.Code != "net_down" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

// --- --execute without --yes → validation refusal ---

func TestCopyCmdExecuteWithoutYes(t *testing.T) {
	cpResetFlags(t)
	cpExecute = true // cpYes stays false
	withFakeClient(t, &cpDiffClient{})

	env, err := runCmd(t, copyCmd, []string{"0x0000000000000000000000000000000000000003"})
	ce, ok := err.(*output.CmdError)
	if !ok || ce.Code != output.ExitValidation {
		t.Fatalf("want validation CmdError (exit %d), got %T %v", output.ExitValidation, err, err)
	}
	if env.OK || env.Cmd != "copy" || env.Error.Category != "validation" || env.Error.Code != "confirm" {
		t.Fatalf("refusal envelope wrong: %+v", env)
	}
}

// --- --execute --yes happy path (full completion → exit 0) ---

type cpExecClient struct {
	core.ClientAPI
	gotExecParams core.CopyParams
	res           *core.CopyExecuteResult
}

func (c *cpExecClient) Copy(_ context.Context, p core.CopyParams) (*core.CopyDiff, error) {
	return &core.CopyDiff{
		Leader:      p.Leader,
		ScaleMode:   "equity",
		ScaleFactor: "1",
		YourEquity:  "1000",
		Diff:        []core.DiffLeg{{Coin: "BTC", Class: "open", Action: "buy", Size: "0.1"}},
		MirroredNow: []string{"BTC"},
	}, nil
}

func (c *cpExecClient) CopyExecute(_ context.Context, diff *core.CopyDiff, p core.CopyParams) (*core.CopyExecuteResult, error) {
	c.gotExecParams = p
	return c.res, nil
}

func TestCopyCmdExecuteHappy(t *testing.T) {
	cpResetFlags(t)
	cpExecute, cpYes = true, true
	fc := &cpExecClient{res: &core.CopyExecuteResult{
		Leader:      "0x0000000000000000000000000000000000000004",
		Legs:        []core.LegResult{{Coin: "BTC", Class: "open", Action: "buy", Status: "filled"}},
		MirroredNow: []string{"BTC"},
		Executed:    1,
		Complete:    true,
	}}
	withFakeClient(t, fc)

	env, err := runCmd(t, copyCmd, []string{"0x0000000000000000000000000000000000000004"})
	if err != nil {
		t.Fatalf("complete execute should be exit 0, got %v", err)
	}
	if !env.OK || env.Cmd != "copy.execute" {
		t.Fatalf("execute envelope wrong: %+v", env)
	}
	if !bytes.Contains(env.Data, []byte("filled")) {
		t.Fatalf("execute data should carry the leg result, got %s", env.Data)
	}
	if fc.gotExecParams.Leader != "0x0000000000000000000000000000000000000004" {
		t.Fatalf("leader not forwarded to CopyExecute: %q", fc.gotExecParams.Leader)
	}
}

// --- --execute --yes, partial completion → exit 60 (ExitPartial), envelope still OK ---

func TestCopyCmdExecutePartialExitCode(t *testing.T) {
	cpResetFlags(t)
	cpExecute, cpYes = true, true
	fc := &cpExecClient{res: &core.CopyExecuteResult{
		Leader:      "0x0000000000000000000000000000000000000005",
		Legs:        []core.LegResult{{Coin: "BTC", Class: "open", Action: "buy", Status: "rejected"}},
		MirroredNow: []string{"BTC"},
		Executed:    0,
		Complete:    false, // some legs rejected/deferred
	}}
	withFakeClient(t, fc)

	env, err := runCmd(t, copyCmd, []string{"0x0000000000000000000000000000000000000005"})
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitPartial {
		t.Fatalf("incomplete execute must return exit %d, got %T %v", output.ExitPartial, err, err)
	}
	if !env.OK || env.Cmd != "copy.execute" {
		t.Fatalf("partial execute must still emit success envelope: %+v", env)
	}
}

// --- --execute --yes, outcome-unknown legs → exit 42 (ExitTimeout) + warning ---

func TestCopyCmdExecuteUnknownCloidsExitCode(t *testing.T) {
	cpResetFlags(t)
	cpExecute, cpYes = true, true
	fc := &cpExecClient{res: &core.CopyExecuteResult{
		Leader:        "0x0000000000000000000000000000000000000006",
		Legs:          []core.LegResult{{Coin: "BTC", Class: "open", Action: "buy", Status: "unknown", Cloid: "0xdead"}},
		MirroredNow:   []string{"BTC"},
		UnknownCloids: []string{"0xdead"},
		Executed:      1,
		Complete:      true, // UnknownCloids takes precedence over Complete
	}}
	withFakeClient(t, fc)

	env, err := runCmd(t, copyCmd, []string{"0x0000000000000000000000000000000000000006"})
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitTimeout {
		t.Fatalf("outcome-unknown legs must return exit %d, got %T %v", output.ExitTimeout, err, err)
	}
	if !env.OK || env.Cmd != "copy.execute" {
		t.Fatalf("unknown-cloid execute must still emit success envelope: %+v", env)
	}
	if len(env.Warnings) == 0 {
		t.Fatalf("outcome-unknown legs must emit a reconcile warning, got %+v", env.Warnings)
	}
}

// --- error from CopyExecute (after a successful diff) ---

type cpExecErrClient struct{ core.ClientAPI }

func (cpExecErrClient) Copy(context.Context, core.CopyParams) (*core.CopyDiff, error) {
	return &core.CopyDiff{Diff: []core.DiffLeg{{Coin: "BTC"}}}, nil
}

func (cpExecErrClient) CopyExecute(context.Context, *core.CopyDiff, core.CopyParams) (*core.CopyExecuteResult, error) {
	return nil, output.Risk("at_stake_cap", "would breach account at-stake cap")
}

func TestCopyCmdExecuteMethodError(t *testing.T) {
	cpResetFlags(t)
	cpExecute, cpYes = true, true
	withFakeClient(t, cpExecErrClient{})

	env, err := runCmd(t, copyCmd, []string{"0x0000000000000000000000000000000000000007"})
	ce, ok := err.(*output.CmdError)
	if !ok || ce.Code != output.ExitRisk {
		t.Fatalf("want risk CmdError (exit %d), got %T %v", output.ExitRisk, err, err)
	}
	if env.OK || env.Cmd != "copy" || env.Error.Category != "risk" || env.Error.Code != "at_stake_cap" {
		t.Fatalf("execute failure envelope wrong: %+v", env)
	}
}

// --- build-client failure (keychain/meta) on copy ---

func TestCopyCmdClientBuildError(t *testing.T) {
	cpResetFlags(t)
	withClientErr(t, output.Auth("no_agent_key", "run onboard"))

	env, err := runCmd(t, copyCmd, []string{"0x0000000000000000000000000000000000000008"})
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitAuth {
		t.Fatalf("want auth CmdError, got %T %v", err, err)
	}
	if env.OK || env.Cmd != "copy" || env.Error.Code != "no_agent_key" {
		t.Fatalf("build-error envelope wrong: %+v", env)
	}
}

// --- pure helpers: orDefault / orDefaultF / orDefaultI ---

func TestCopyOrDefaultHelpers(t *testing.T) {
	if got := orDefault("set", "fallback"); got != "set" {
		t.Fatalf("orDefault should keep non-empty value, got %q", got)
	}
	if got := orDefault("", "fallback"); got != "fallback" {
		t.Fatalf("orDefault should fall back on empty, got %q", got)
	}
	if got := orDefaultF(2.5, 1.0); got != 2.5 {
		t.Fatalf("orDefaultF should keep non-zero value, got %v", got)
	}
	if got := orDefaultF(0, 1.0); got != 1.0 {
		t.Fatalf("orDefaultF should fall back on zero, got %v", got)
	}
	if got := orDefaultI(7, 3); got != 7 {
		t.Fatalf("orDefaultI should keep non-zero value, got %d", got)
	}
	if got := orDefaultI(0, 3); got != 3 {
		t.Fatalf("orDefaultI should fall back on zero, got %d", got)
	}
}
