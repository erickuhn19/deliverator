package cmd

// Coverage for the pure / pre-client cmd helpers the audit (#89) flagged at 0%:
// the exit-code mapping, the dead-man-switch state file, the halt sentinel, the
// JSON-mode precedence, order-request assembly, positional/argument helpers, and
// the early (no-network) validation branches of `trade` and `dms`. None of these
// touch the exchange — they run entirely on local FS + flag globals.

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
)

// --- exitCodeFor ---

func TestExitCodeFor(t *testing.T) {
	if got := exitCodeFor(nil); got != output.ExitOK {
		t.Errorf("nil err -> %d, want ExitOK(%d)", got, output.ExitOK)
	}
	// A *CmdError carries its own code through verbatim (e.g. risk rejection).
	ce := &output.CmdError{Code: output.ExitRisk}
	if got := exitCodeFor(ce); got != output.ExitRisk {
		t.Errorf("CmdError{ExitRisk} -> %d, want %d", got, output.ExitRisk)
	}
	// Any other error is the unhandled-cobra path -> validation.
	if got := exitCodeFor(errors.New("boom")); got != output.ExitValidation {
		t.Errorf("plain err -> %d, want ExitValidation(%d)", got, output.ExitValidation)
	}
}

// --- argsReferenceOutcomes (lazy outcome-load trigger) ---

func TestArgsReferenceOutcomes(t *testing.T) {
	save := os.Args
	t.Cleanup(func() { os.Args = save })
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"deliverator", "buy", "#1730", "10"}, true},      // # coin token
		{[]string{"deliverator", "close", "#6410", "--yes"}, true}, // # coin elsewhere
		{[]string{"deliverator", "markets", "--class", "outcome"}, true},
		{[]string{"deliverator", "markets", "--class", "all"}, true},
		{[]string{"deliverator", "markets", "--class=all"}, true}, // = form
		{[]string{"deliverator", "markets", "--class", "perp"}, false},
		{[]string{"deliverator", "buy", "BTC", "--notional", "20"}, false}, // no outcomes
		{[]string{"deliverator", "positions"}, false},
	}
	for _, tc := range cases {
		os.Args = tc.args
		if got := argsReferenceOutcomes(); got != tc.want {
			t.Errorf("argsReferenceOutcomes(%v) = %v, want %v", tc.args[1:], got, tc.want)
		}
	}
}

// --- order priority (--priority-bps) ---

func TestBuildOrderReqPriority(t *testing.T) {
	save := wPriority
	t.Cleanup(func() { wPriority = save })
	c := &cobra.Command{}
	c.Flags().IntVar(&wBuilderFee, "builder-fee", 0, "")
	c.Flags().IntVar(&wPriority, "priority-bps", 0, "")

	// Unset -> Priority nil (use config default downstream).
	if req := buildOrderReq(c, "BTC", core.Buy, "1"); req.Priority != nil {
		t.Fatalf("unset --priority-bps must leave Priority nil, got %v", req.Priority)
	}
	// Explicitly set -> threaded as a pointer to the bps value.
	if err := c.Flags().Set("priority-bps", "3"); err != nil {
		t.Fatal(err)
	}
	req := buildOrderReq(c, "BTC", core.Buy, "1")
	if req.Priority == nil || *req.Priority != 3 {
		t.Fatalf("changed --priority-bps must thread &3, got %v", req.Priority)
	}
}

// Priority and a tp/sl bracket share the action grouping, so the CLI must reject
// the combo before any client/network work (mirrors HL's mutual exclusivity).
func TestRunTradePriorityBracketRejected(t *testing.T) {
	saveP, saveN, saveTp, saveSl := wPriority, wNotional, wTp, wSl
	t.Cleanup(func() { wPriority, wNotional, wTp, wSl = saveP, saveN, saveTp, saveSl })
	wPriority, wNotional, wTp, wSl = 2, 0, "64000", ""

	var buf bytes.Buffer
	output.Configure(true, &buf)
	t.Cleanup(func() { output.Configure(true, nil) })

	err := runTrade(&cobra.Command{}, "buy", "BTC", core.Buy, "0.01")
	ce, ok := err.(*output.CmdError)
	if !ok || ce.Code != output.ExitValidation {
		t.Fatalf("want validation CmdError, got %T %v", err, err)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if e := json.Unmarshal(buf.Bytes(), &env); e != nil {
		t.Fatalf("want one envelope, got %q: %v", buf.String(), e)
	}
	if env.Error.Code != "priority_bracket" {
		t.Fatalf("want priority_bracket, got %q (%s)", env.Error.Code, buf.String())
	}
}

// --- dead-man-switch state file ---

func TestDMSStateRoundTrip(t *testing.T) {
	t.Setenv("DELIVERATOR_HOME", t.TempDir())

	if _, ok := readDMS(); ok {
		t.Fatal("readDMS on a fresh home must report not-armed (ok=false)")
	}

	want := dmsState{Secs: 30, DeadlineMs: 1_700_000_030_000, SetAtMs: 1_700_000_000_000}
	writeDMS(want)

	got, ok := readDMS()
	if !ok {
		t.Fatal("readDMS after writeDMS must report ok=true")
	}
	if got != want {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, want)
	}

	// The state file holds a cancel deadline, not a secret, but it must still be
	// written 0600 (the dir is the operator's private config dir).
	if fi, err := os.Stat(dmsPath()); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("dms.json mode = %v, want 0600", fi.Mode().Perm())
	}

	// Corrupt JSON must be treated as not-armed, never a panic or partial state.
	if err := os.WriteFile(dmsPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readDMS(); ok {
		t.Fatal("corrupt dms.json must report ok=false")
	}
}

func TestDMSPathUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DELIVERATOR_HOME", home)
	if got, want := dmsPath(), filepath.Join(home, "dms.json"); got != want {
		t.Fatalf("dmsPath() = %q, want %q", got, want)
	}
}

// --- halt sentinel ---

func TestHaltedByFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DELIVERATOR_HOME", home)

	if haltedByFile() {
		t.Fatal("no halt file -> haltedByFile must be false")
	}
	halt := filepath.Join(config.Dir(), "halt")
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(halt, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !haltedByFile() {
		t.Fatal("halt file present -> haltedByFile must be true")
	}
	if err := os.Remove(halt); err != nil {
		t.Fatal(err)
	}
	if haltedByFile() {
		t.Fatal("removing the halt file must clear haltedByFile")
	}
}

// --- resolveJSONMode flag precedence ---

func TestResolveJSONModeFlagPrecedence(t *testing.T) {
	saveJSON, saveNoJSON := flagJSON, flagNoJSON
	t.Cleanup(func() { flagJSON, flagNoJSON = saveJSON, saveNoJSON })

	cfg := config.Default()

	// --no-json wins over everything, including --json.
	flagNoJSON, flagJSON = true, true
	if resolveJSONMode(cfg) {
		t.Error("--no-json must force human output even when --json is also set")
	}
	// --json (without --no-json) forces JSON regardless of TTY/config.
	flagNoJSON, flagJSON = false, true
	if !resolveJSONMode(cfg) {
		t.Error("--json must force JSON output")
	}
}

// --- buildOrderReq ---

func TestBuildOrderReq(t *testing.T) {
	save := struct {
		notional, slippage          float64
		limit, cloid, trigger, ttyp string
		ro, alo, ioc, tmkt          bool
		bfee                        int
	}{wNotional, wSlippage, wLimit, wCloid, wTrigger, wTriggerType, wReduceOnly, wAlo, wIoc, wTriggerMarket, wBuilderFee}
	t.Cleanup(func() {
		wNotional, wSlippage = save.notional, save.slippage
		wLimit, wCloid, wTrigger, wTriggerType = save.limit, save.cloid, save.trigger, save.ttyp
		wReduceOnly, wAlo, wIoc, wTriggerMarket = save.ro, save.alo, save.ioc, save.tmkt
		wBuilderFee = save.bfee
	})

	newCmd := func() *cobra.Command {
		c := &cobra.Command{}
		c.Flags().IntVar(&wBuilderFee, "builder-fee", 0, "")
		return c
	}

	// Defaults: Gtc, no trigger, builder-fee untouched.
	wNotional, wSlippage, wLimit, wCloid = 100, 0.02, "65000", "0xabc"
	wReduceOnly, wAlo, wIoc, wTrigger = true, false, false, ""
	req := buildOrderReq(newCmd(), "BTC", core.Buy, "0.1")
	if req.Coin != "BTC" || req.Side != core.Buy || req.Size != "0.1" {
		t.Fatalf("identity fields wrong: %+v", req)
	}
	if req.Notional != 100 || req.Limit != "65000" || req.Cloid != "0xabc" || req.Slippage != 0.02 || !req.ReduceOnly {
		t.Fatalf("flag fields not threaded: %+v", req)
	}
	if req.Tif != "Gtc" {
		t.Errorf("default Tif = %q, want Gtc", req.Tif)
	}
	if req.Trigger != nil {
		t.Errorf("no --trigger must leave Trigger nil, got %+v", req.Trigger)
	}
	if req.BuilderFee != nil {
		t.Error("an unchanged --builder-fee must leave BuilderFee nil")
	}

	// Alo and Ioc map to their TIFs (Alo wins the switch order).
	wAlo, wIoc = true, false
	if got := buildOrderReq(newCmd(), "BTC", core.Buy, "1").Tif; got != "Alo" {
		t.Errorf("--alo -> Tif %q, want Alo", got)
	}
	wAlo, wIoc = false, true
	if got := buildOrderReq(newCmd(), "BTC", core.Buy, "1").Tif; got != "Ioc" {
		t.Errorf("--ioc -> Tif %q, want Ioc", got)
	}

	// A trigger price builds a TriggerReq carrying the type/market flags.
	wAlo, wIoc = false, false
	wTrigger, wTriggerType, wTriggerMarket = "64000", "sl", true
	req = buildOrderReq(newCmd(), "BTC", core.Sell, "1")
	if req.Trigger == nil || req.Trigger.TriggerPx != "64000" || req.Trigger.Tpsl != "sl" || !req.Trigger.IsMarket {
		t.Fatalf("trigger not wired: %+v", req.Trigger)
	}

	// An explicitly-set --builder-fee threads a pointer to its value.
	c := newCmd()
	if err := c.Flags().Set("builder-fee", "7"); err != nil {
		t.Fatal(err)
	}
	req = buildOrderReq(c, "BTC", core.Buy, "1")
	if req.BuilderFee == nil || *req.BuilderFee != 7 {
		t.Fatalf("changed --builder-fee must thread &7, got %v", req.BuilderFee)
	}
}

// --- argAt ---

func TestArgAt(t *testing.T) {
	args := []string{"order", "BTC", "buy"}
	if got := argAt(args, 1); got != "BTC" {
		t.Errorf("argAt(_,1) = %q, want BTC", got)
	}
	if got := argAt(args, 9); got != "" {
		t.Errorf("argAt past the end must be \"\", got %q", got)
	}
}

// --- readBatchInput ---

func TestReadBatchInput(t *testing.T) {
	p := filepath.Join(t.TempDir(), "orders.json")
	if err := os.WriteFile(p, []byte(`[{"coin":"BTC"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := readBatchInput(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `[{"coin":"BTC"}]` {
		t.Fatalf("file content wrong: %q", b)
	}
	if _, err := readBatchInput(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("a nonexistent path must error")
	}
}

// --- runTrade pre-client validation ---

// runTrade rejects bad size sourcing before it ever builds a client, so these
// paths are exercisable offline and must return a validation CmdError + envelope.
func TestRunTradeSizeValidation(t *testing.T) {
	saveN, saveTp, saveSl := wNotional, wTp, wSl
	t.Cleanup(func() { wNotional, wTp, wSl = saveN, saveTp, saveSl })

	check := func(name, size string, notional float64, tp, sl, wantCode string) {
		t.Helper()
		wNotional, wTp, wSl = notional, tp, sl
		var buf bytes.Buffer
		output.Configure(true, &buf)
		t.Cleanup(func() { output.Configure(true, nil) })

		err := runTrade(&cobra.Command{}, "order", "BTC", core.Buy, size)
		ce, ok := err.(*output.CmdError)
		if !ok || ce.Code != output.ExitValidation {
			t.Fatalf("%s: want validation CmdError, got %T %v", name, err, err)
		}
		var env struct {
			OK    bool `json:"ok"`
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if e := json.Unmarshal(buf.Bytes(), &env); e != nil {
			t.Fatalf("%s: want one JSON envelope, got %q: %v", name, buf.String(), e)
		}
		if env.OK || env.Error.Code != wantCode {
			t.Fatalf("%s: envelope code = %q, want %q (%s)", name, env.Error.Code, wantCode, buf.String())
		}
	}

	check("both", "0.1", 100, "", "", "size_xor_notional")              // size arg AND --notional
	check("neither", "", 0, "", "", "missing_size")                     // no size, no --notional
	check("notional+bracket", "", 100, "64000", "", "notional_bracket") // --notional with --tp
}

// --- dms command: status + pre-client set validation ---

func TestDMSCommandStatusAndValidation(t *testing.T) {
	t.Setenv("DELIVERATOR_HOME", t.TempDir())
	// The set/heartbeat path reads Cfg.Risk.DeadManSwitchSecs (default 0) before
	// validating, so Cfg must be non-nil even on the early-reject branches.
	Cfg = config.Default()
	t.Cleanup(func() { Cfg = nil })

	statusArmed := func(t *testing.T) bool {
		t.Helper()
		var buf bytes.Buffer
		output.Configure(true, &buf)
		t.Cleanup(func() { output.Configure(true, nil) })
		if err := dmsCmd.RunE(dmsCmd, []string{"status"}); err != nil {
			t.Fatalf("dms status: %v", err)
		}
		var env struct {
			Data struct {
				Armed bool `json:"armed"`
			} `json:"data"`
		}
		if e := json.Unmarshal(buf.Bytes(), &env); e != nil {
			t.Fatalf("dms status must emit one envelope, got %q: %v", buf.String(), e)
		}
		return env.Data.Armed
	}

	if statusArmed(t) {
		t.Fatal("dms status on a fresh home must report armed=false")
	}
	// A future deadline reads back as armed.
	writeDMS(dmsState{Secs: 60, DeadlineMs: 1_700_000_000_000 + 9_000_000_000_000, SetAtMs: 1_700_000_000_000})
	if !statusArmed(t) {
		t.Fatal("a future deadline must report armed=true")
	}

	// set-subcommand validation returns before any client/network is built.
	badSet := func(name, wantCode string, args ...string) {
		t.Helper()
		var buf bytes.Buffer
		output.Configure(true, &buf)
		t.Cleanup(func() { output.Configure(true, nil) })
		err := dmsCmd.RunE(dmsCmd, args)
		ce, ok := err.(*output.CmdError)
		if !ok || ce.Code != output.ExitValidation {
			t.Fatalf("%s: want validation CmdError, got %T %v", name, err, err)
		}
		var env struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if e := json.Unmarshal(buf.Bytes(), &env); e != nil {
			t.Fatalf("%s: want one envelope, got %q: %v", name, buf.String(), e)
		}
		if env.Error.Code != wantCode {
			t.Fatalf("%s: code = %q, want %q", name, env.Error.Code, wantCode)
		}
	}
	badSet("missing secs", "missing_secs", "set")
	badSet("non-integer", "bad_secs", "set", "abc")
	badSet("below minimum", "bad_secs", "set", "4")
}
