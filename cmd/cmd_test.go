package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
)

func TestParseSide(t *testing.T) {
	for _, s := range []string{"buy", "b", "long", "BUY"} {
		if side, err := parseSide(s); err != nil || side != core.Buy {
			t.Errorf("parseSide(%q) = %v, %v; want Buy", s, side, err)
		}
	}
	for _, s := range []string{"sell", "s", "short"} {
		if side, err := parseSide(s); err != nil || side != core.Sell {
			t.Errorf("parseSide(%q) = %v, %v; want Sell", s, side, err)
		}
	}
	if _, err := parseSide("nonsense"); err == nil {
		t.Error("parseSide(nonsense) should error")
	}
}

func TestSetConfigKey(t *testing.T) {
	cfg := config.Default()

	ok := func(k, v string) {
		t.Helper()
		if err := setConfigKey(cfg, k, v); err != nil {
			t.Errorf("setConfigKey(%q,%q) errored: %v", k, v, err)
		}
	}
	bad := func(k, v string) {
		t.Helper()
		if err := setConfigKey(cfg, k, v); err == nil {
			t.Errorf("setConfigKey(%q,%q) should error", k, v)
		}
	}

	ok("network", "mainnet")
	if cfg.Network != "mainnet" {
		t.Errorf("network not set: %q", cfg.Network)
	}
	ok("risk.max_leverage", "5")
	if cfg.Risk.MaxLeverage != 5 {
		t.Errorf("max_leverage not set: %d", cfg.Risk.MaxLeverage)
	}
	ok("automation.limit_only", "true")
	if !cfg.Automation.LimitOnly {
		t.Error("limit_only not set")
	}
	ok("automation.allowed_coins", "BTC, ETH ,SOL")
	if len(cfg.Automation.AllowedCoins) != 3 || cfg.Automation.AllowedCoins[1] != "ETH" {
		t.Errorf("allowed_coins wrong: %v", cfg.Automation.AllowedCoins)
	}
	ok("builder.fee_tenths_bps", "50")

	ok("risk.max_account_leverage", "3")
	if cfg.Risk.MaxAccountLeverage != 3 {
		t.Errorf("max_account_leverage not set: %v", cfg.Risk.MaxAccountLeverage)
	}
	ok("risk.max_net_exposure_usd", "25000")
	ok("risk.max_concentration_pct_per_coin", "40")
	ok("risk.max_drawdown_pct", "15")
	ok("risk.max_daily_loss_usd", "500")
	ok("risk.max_daily_loss_pct", "10")
	if cfg.Risk.MaxDailyLossUSD != 500 || cfg.Risk.MaxDailyLossPct != 10 {
		t.Errorf("daily-loss keys not set: %v / %v", cfg.Risk.MaxDailyLossUSD, cfg.Risk.MaxDailyLossPct)
	}
	ok("risk.max_open_positions", "5")
	ok("copy.default_scale_mode", "fixed")
	ok("copy.default_scale", "0.5")
	ok("copy.max_orders_per_cycle", "3")
	if cfg.Copy.DefaultScaleMode != "fixed" || cfg.Copy.DefaultScale != 0.5 {
		t.Errorf("copy keys not set: %v / %v", cfg.Copy.DefaultScaleMode, cfg.Copy.DefaultScale)
	}
	if cfg.Risk.MaxOpenPositions != 5 {
		t.Errorf("max_open_positions not set: %v", cfg.Risk.MaxOpenPositions)
	}

	ok("state.command_log", "/tmp/c.jsonl") // command_log is not path-confined
	ok("state.audit", "false")
	ok("state.audit_path", "a.jsonl") // audit_path IS confined to config.Dir() (#91)
	ok("state.meta_ttl_secs", "60")
	if cfg.State.CommandLog != "/tmp/c.jsonl" || cfg.State.Audit || cfg.State.AuditPath != "a.jsonl" || cfg.State.MetaTTLSecs != 60 {
		t.Errorf("state.* keys not set: %+v", cfg.State)
	}

	bad("unknown.key", "x")
	bad("state.audit", "not-a-bool")
	bad("risk.max_leverage", "not-an-int")
	bad("automation.limit_only", "not-a-bool")
	bad("risk.max_account_leverage", "not-a-float")
}

// A transient global --network flag (applied to Cfg in PreRun) must NOT be
// persisted by an unrelated `config set` — else `--network mainnet config set X`
// silently flips the box to mainnet on disk.
func TestConfigSetIgnoresTransientNetworkFlag(t *testing.T) {
	t.Setenv("DELIVERATOR_HOME", t.TempDir())
	seed := config.Default()
	seed.Network = "testnet"
	if err := seed.Save(config.Path()); err != nil {
		t.Fatal(err)
	}
	// Simulate PersistentPreRunE applying `--network mainnet` onto the in-memory Cfg.
	loaded, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	loaded.Network = "mainnet"
	Cfg = loaded
	flagNetwork, flagConfig = "mainnet", ""
	t.Cleanup(func() { flagNetwork = ""; Cfg = nil })

	// Set an UNRELATED key.
	if err := configSetCmd.RunE(configSetCmd, []string{"risk.max_leverage", "7"}); err != nil {
		t.Fatalf("config set: %v", err)
	}

	after, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if after.Network != "testnet" {
		t.Fatalf("transient --network leaked to disk: got %q, want testnet", after.Network)
	}
	if after.Risk.MaxLeverage != 7 {
		t.Fatalf("named key not persisted: max_leverage=%d", after.Risk.MaxLeverage)
	}
}

// The command tree builds and registers the expected commands without panicking.
func TestCommandTree(t *testing.T) {
	have := map[string]bool{}
	for _, c := range rootCmd.Commands() {
		have[c.Name()] = true
	}
	for _, want := range []string{"buy", "sell", "order", "cancel", "close", "twap", "stream", "portfolio", "connect", "config", "onboard", "referral", "info", "leaderboard"} {
		if !have[want] {
			t.Errorf("command %q not registered", want)
		}
	}
}

func TestBuildInfoBody(t *testing.T) {
	b, err := buildInfoBody([]string{"fundingHistory", "coin=BTC", "startTime=1750000000000", "user=@"})
	if err != nil {
		t.Fatal(err)
	}
	if b["type"] != "fundingHistory" || b["coin"] != "BTC" || b["startTime"] != int64(1750000000000) || b["user"] != "@" {
		t.Fatalf("body wrong: %#v", b)
	}
	if _, err := buildInfoBody([]string{"x", "noequals"}); err == nil {
		t.Error("a param without '=' should error")
	}
}

// Every command must honor the envelope contract under --json. `tools` is the
// one command that prints free-form markdown, so it must emit a JSON envelope
// (markdown in data.content) in JSON mode and raw markdown for a human TTY.
func TestToolsRespectsJSONMode(t *testing.T) {
	ToolsMarkdown = "# heading\n\nbody\n"
	t.Cleanup(func() { ToolsMarkdown = "" })

	// JSON mode: stdout must parse as one envelope carrying the markdown.
	var buf bytes.Buffer
	output.Configure(true, &buf)
	if err := toolsCmd.RunE(toolsCmd, nil); err != nil {
		t.Fatal(err)
	}
	var env struct {
		Schema string `json:"schema"`
		OK     bool   `json:"ok"`
		Cmd    string `json:"cmd"`
		Data   struct {
			Format  string `json:"format"`
			Content string `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("tools --json must be one JSON object, got %q: %v", buf.String(), err)
	}
	if !env.OK || env.Cmd != "tools" || env.Data.Format != "markdown" || env.Data.Content != ToolsMarkdown {
		t.Fatalf("envelope wrong: %+v", env)
	}

	// Human mode: raw markdown, not JSON.
	buf.Reset()
	output.Configure(false, &buf)
	t.Cleanup(func() { output.Configure(true, nil) })
	if err := toolsCmd.RunE(toolsCmd, nil); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != ToolsMarkdown {
		t.Fatalf("human mode must print raw markdown, got %q", got)
	}
	if strings.HasPrefix(strings.TrimSpace(buf.String()), "{") {
		t.Fatal("human mode must not emit JSON")
	}
}

// A grouping command (config/account/builder/referral/stream/root) invoked bare
// or with an unknown subcommand must emit a validation envelope (exit 10) under
// --json, never cobra's raw help + exit 0. Humans still get the help text.
func TestRequireSubcommandHonorsEnvelopeContract(t *testing.T) {
	parents := []*cobra.Command{configCmd, accountCmd, builderCmd, referralCmd, streamCmd, rootCmd}

	envErr := func(t *testing.T, args []string, wantCode string) {
		t.Helper()
		for _, p := range parents {
			var buf bytes.Buffer
			output.Configure(true, &buf)
			err := requireSubcommand(p, args)
			ce, ok := err.(*output.CmdError)
			if !ok || ce.Code != output.ExitValidation {
				t.Fatalf("%s bare/unknown must return a validation CmdError, got %T %v", p.Name(), err, err)
			}
			var env struct {
				OK    bool `json:"ok"`
				Error struct {
					Code     string `json:"code"`
					Category string `json:"category"`
				} `json:"error"`
			}
			if e := json.Unmarshal(buf.Bytes(), &env); e != nil {
				t.Fatalf("%s must emit one JSON envelope, got %q: %v", p.Name(), buf.String(), e)
			}
			if env.OK || env.Error.Category != "validation" || env.Error.Code != wantCode {
				t.Fatalf("%s envelope wrong: %s", p.Name(), buf.String())
			}
		}
	}

	envErr(t, nil, "subcommand_required")              // bare parent
	envErr(t, []string{"fnord"}, "unknown_subcommand") // unknown subcommand

	// Human mode: print help (to cobra's own writer), no error, no JSON.
	var help bytes.Buffer
	output.Configure(false, nil)
	t.Cleanup(func() { output.Configure(true, nil) })
	configCmd.SetOut(&help)
	t.Cleanup(func() { configCmd.SetOut(nil) })
	if err := requireSubcommand(configCmd, nil); err != nil {
		t.Fatalf("human bare parent must not error: %v", err)
	}
	if !strings.Contains(help.String(), "Usage:") || strings.HasPrefix(strings.TrimSpace(help.String()), "{") {
		t.Fatalf("human mode must print help, not JSON: %q", help.String())
	}
}

// floatIfSet must return nil unless the flag was explicitly set — so a filter
// bound of 0 (e.g. --min-pnl 0) is honored while an absent flag means "no filter".
func TestFloatIfSet(t *testing.T) {
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	var v float64
	fs.Float64Var(&v, "min-pnl", 0, "")
	if got := floatIfSet(fs, "min-pnl", v); got != nil {
		t.Fatalf("unset flag should yield nil, got %v", *got)
	}
	if err := fs.Parse([]string{"--min-pnl", "0"}); err != nil {
		t.Fatal(err)
	}
	if got := floatIfSet(fs, "min-pnl", v); got == nil || *got != 0 {
		t.Fatalf("explicit --min-pnl 0 should yield &0, got %v", got)
	}
}

// The leaderboard command exposes its alias and sane defaults.
func TestLeaderboardCmdWiring(t *testing.T) {
	if leaderboardCmd.Flags().Lookup("window").DefValue != "day" {
		t.Error("window default should be day")
	}
	if leaderboardCmd.Flags().Lookup("sort").DefValue != "pnl" {
		t.Error("sort default should be pnl")
	}
	hasAlias := false
	for _, a := range leaderboardCmd.Aliases {
		if a == "lb" {
			hasAlias = true
		}
	}
	if !hasAlias {
		t.Error("leaderboard should have alias lb")
	}
}

func TestOnboardAddrRegex(t *testing.T) {
	if !onboardAddrRe.MatchString("0x9ccAcA47f0318FaeF9C8175767a15AEe1586177e") {
		t.Error("valid checksummed address should match")
	}
	for _, bad := range []string{"", "0x123", "9ccAcA47f0318FaeF9C8175767a15AEe1586177e", "0xZZZ"} {
		if onboardAddrRe.MatchString(bad) {
			t.Errorf("invalid address %q should not match", bad)
		}
	}
}
