package cmd

// Shared harness for testing command RunE handlers offline (#102): the newClient
// seam is swapped for a fake core.ClientAPI, so handlers run with no network and
// no keychain. Each test defines its OWN tiny fake by embedding core.ClientAPI
// and overriding just the methods it exercises (unstubbed methods panic if
// called — which is the right failure for an unexpected call).

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
)

// envelope mirrors the schema-v1 output envelope for assertions.
type envelope struct {
	Schema   string          `json:"schema"`
	OK       bool            `json:"ok"`
	Cmd      string          `json:"cmd"`
	Data     json.RawMessage `json:"data"`
	Warnings []string        `json:"warnings"`
	Error    struct {
		Code     string `json:"code"`
		Category string `json:"category"`
	} `json:"error"`
}

// withFakeClient routes the newClient seam to f for the duration of the test.
func withFakeClient(t *testing.T, f core.ClientAPI) {
	t.Helper()
	prev := newClient
	newClient = func(ctx context.Context) (core.ClientAPI, error) { return f, nil }
	t.Cleanup(func() { newClient = prev })
}

// withClientErr makes the newClient seam fail — exercises a handler's
// build-client error path (keychain missing, meta fetch failed, etc.).
func withClientErr(t *testing.T, err error) {
	t.Helper()
	prev := newClient
	newClient = func(ctx context.Context) (core.ClientAPI, error) { return nil, err }
	t.Cleanup(func() { newClient = prev })
}

// runCmd invokes a RunE with JSON output captured, returning the parsed envelope
// and the RunE error (a *output.CmdError carries the exit code). Ensures Cfg is
// non-nil so emit()/RootMeta work.
func runCmd(t *testing.T, c *cobra.Command, args []string) (envelope, error) {
	t.Helper()
	if Cfg == nil {
		Cfg = config.Default()
		t.Cleanup(func() { Cfg = nil })
	}
	var buf bytes.Buffer
	output.Configure(true, &buf)
	t.Cleanup(func() { output.Configure(true, nil) })
	runErr := c.RunE(c, args)
	var env envelope
	if buf.Len() > 0 {
		if e := json.Unmarshal(buf.Bytes(), &env); e != nil {
			t.Fatalf("%s output must be one JSON envelope, got %q: %v", c.Name(), buf.String(), e)
		}
	}
	return env, runErr
}

// resetWriteFlags zeroes the trade flag globals buildOrderReq reads, so a write
// test isn't polluted by another test's flag state. Restores them after.
func resetWriteFlags(t *testing.T) {
	t.Helper()
	save := struct {
		limit, tp, sl, trigger, ttype, cloid string
		ioc, alo, ro, tmkt                   bool
		bfee, prio                           int
		slip, notional                       float64
	}{wLimit, wTp, wSl, wTrigger, wTriggerType, wCloid, wIoc, wAlo, wReduceOnly, wTriggerMarket, wBuilderFee, wPriority, wSlippage, wNotional}
	wLimit, wTp, wSl, wTrigger, wTriggerType, wCloid = "", "", "", "", "tp", ""
	wIoc, wAlo, wReduceOnly, wTriggerMarket = false, false, false, false
	wBuilderFee, wPriority, wSlippage, wNotional = 0, 0, 0, 0
	t.Cleanup(func() {
		wLimit, wTp, wSl, wTrigger, wTriggerType, wCloid = save.limit, save.tp, save.sl, save.trigger, save.ttype, save.cloid
		wIoc, wAlo, wReduceOnly, wTriggerMarket = save.ioc, save.alo, save.ro, save.tmkt
		wBuilderFee, wPriority, wSlippage, wNotional = save.bfee, save.prio, save.slip, save.notional
	})
}

// --- Example tests establishing the pattern (read happy/error, write exit codes) ---

type midsOKClient struct{ core.ClientAPI }

func (midsOKClient) Mids(context.Context) (map[string]string, error) {
	return map[string]string{"BTC": "65000"}, nil
}

func TestMidsCmdReadHappy(t *testing.T) {
	withFakeClient(t, midsOKClient{})
	env, err := runCmd(t, midsCmd, nil)
	if err != nil {
		t.Fatalf("mids should succeed, got %v", err)
	}
	if !env.OK || env.Cmd != "mids" {
		t.Fatalf("envelope wrong: %+v", env)
	}
	if !bytes.Contains(env.Data, []byte("BTC")) {
		t.Fatalf("data should carry mids, got %s", env.Data)
	}
}

type midsErrClient struct{ core.ClientAPI }

func (midsErrClient) Mids(context.Context) (map[string]string, error) {
	return nil, output.Network("net_down", "unreachable")
}

// A method error flows through fail() to a categorized failure envelope + exit.
func TestReadMethodErrorEnvelope(t *testing.T) {
	withFakeClient(t, midsErrClient{})
	env, err := runCmd(t, midsCmd, nil)
	ce, ok := err.(*output.CmdError)
	if !ok || ce.Code != output.ExitNetwork {
		t.Fatalf("want network CmdError (exit %d), got %T %v", output.ExitNetwork, err, err)
	}
	if env.OK || env.Error.Category != "network" || env.Error.Code != "net_down" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

// A build-client failure (keychain/meta) surfaces as a failure envelope too.
func TestReadClientBuildError(t *testing.T) {
	withClientErr(t, output.Auth("no_agent_key", "run onboard"))
	env, err := runCmd(t, midsCmd, nil)
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitAuth {
		t.Fatalf("want auth CmdError, got %T %v", err, err)
	}
	if env.OK || env.Error.Code != "no_agent_key" {
		t.Fatalf("failure envelope wrong: %+v", env)
	}
}

type placeClient struct {
	core.ClientAPI
	res *core.PlaceResult
}

func (p placeClient) Place(context.Context, core.OrderReq) (*core.PlaceResult, []string, error) {
	return p.res, []string{"builder fee 0.050% applied"}, nil
}

// A fully-filled buy → exit 0; a partial fill → exit 60 (ExitPartial), with the
// success envelope emitted either way.
func TestBuyCmdFillAndPartialExitCodes(t *testing.T) {
	resetWriteFlags(t)

	withFakeClient(t, placeClient{res: &core.PlaceResult{Status: "filled", Size: "0.1", FilledSz: "0.1", Coin: "BTC"}})
	env, err := runCmd(t, buyCmd, []string{"BTC", "0.1"})
	if err != nil {
		t.Fatalf("full fill should be exit 0, got %v", err)
	}
	if !env.OK || env.Cmd != "buy" {
		t.Fatalf("buy envelope wrong: %+v", env)
	}

	withFakeClient(t, placeClient{res: &core.PlaceResult{Status: "filled", Size: "0.1", FilledSz: "0.05", Coin: "BTC"}})
	env, err = runCmd(t, buyCmd, []string{"BTC", "0.1"})
	if ce, ok := err.(*output.CmdError); !ok || ce.Code != output.ExitPartial {
		t.Fatalf("partial fill must return exit %d, got %T %v", output.ExitPartial, err, err)
	}
	if !env.OK {
		t.Fatalf("partial fill must still emit the success envelope, got %+v", env)
	}
}
