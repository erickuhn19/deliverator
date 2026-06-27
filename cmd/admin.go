package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
	"github.com/erickuhn19/deliverator/internal/wallet"
)

// ---- tools: print the OpenClaw contract (raw markdown, for self-install) ----

var toolsCmd = &cobra.Command{
	Use:   "tools",
	Short: "Print TOOLS.md — the agent contract (raw markdown for humans, JSON envelope under --json)",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Honor the envelope contract under --json so an agent piping `tools --json`
		// gets parseable JSON, not raw markdown; humans still get the markdown.
		if output.JSONMode() {
			emit("tools", map[string]any{"format": "markdown", "content": ToolsMarkdown})
			return nil
		}
		fmt.Fprint(output.Writer(), ToolsMarkdown)
		return nil
	},
}

// ---- schema: describe the output envelope (schema v1) ----

var schemaCmd = &cobra.Command{
	Use:   "schema [command]",
	Short: "Dump the JSON output schema: the envelope, or a command's data payload",
	Long: `With no argument, dump the envelope schema (schema/ok/ts/cmd/data/error/
warnings/meta) + exit codes. With a command name, dump the JSON Schema of THAT
command's data payload, e.g. "schema positions" / "schema preview" — so an agent
can codegen or validate against the exact output shape. Run "schema commands" to
list the commands with a typed data schema.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			name := args[0]
			if name == "commands" || name == "list" {
				emit("schema", map[string]any{"describable": describableCommands()})
				return nil
			}
			payload, ok := describeCommand(name)
			if !ok {
				return fail("schema", output.Validation("unknown_command",
					"no typed data schema for "+name).
					WithHint("run `deliverator schema commands` to list describable commands"))
			}
			emit("schema", map[string]any{
				"command":    name,
				"note":       "JSON Schema of the `data` payload only; the envelope wraps it (see bare `schema`). HL prices/sizes are strings; counters/timestamps are numbers.",
				"dataSchema": payload,
			})
			return nil
		}
		emit("schema", map[string]any{
			"schema_version": output.SchemaVersion,
			"envelope": map[string]any{
				"schema":   "string (e.g. v1)",
				"ok":       "bool",
				"ts":       "int64 server-aligned unix ms",
				"cmd":      "string",
				"data":     "object|array|null (command-specific — `schema <command>` describes it)",
				"error":    "object|null {code,category,message,retryable,retry_after_ms,hint}",
				"warnings": "string[] (rounding, builder fee, etc.)",
				"meta":     "object {network,account,weight_used}",
			},
			"prices_and_sizes": "always strings",
			"describe_command": "deliverator schema <command> (e.g. positions, preview); `schema commands` lists them",
			"exit_codes": map[string]int{
				"ok": 0, "unknown": 1, "validation": 10, "precision": 11, "risk": 20,
				"halt": 21, "auth": 30, "network": 40, "rate_limit": 41, "timeout": 42,
				"exchange": 50, "partial": 60, "clock": 70,
			},
		})
		return nil
	},
}

// ---- connect / health: preflight ----

var connectCmd = &cobra.Command{
	Use:     "connect",
	Aliases: []string{"health"},
	Short:   "Preflight: key, account, network, clock skew, API, meta age",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := cmdCtx()
		defer cancel()
		data := map[string]any{
			"network":        Cfg.Network,
			"config_path":    Cfg.SourcePath(),
			"master_address": Cfg.Wallet.MasterAddress,
			"key_source":     wallet.ActiveSource(), // "env" override, else "keychain"
		}
		warnings := []string{}

		// Agent key presence (address only; never the secret).
		if ag, err := wallet.Load(flagAccount); err == nil {
			data["agent_address"] = ag.Address
			data["agent_key"] = "present"
		} else {
			data["agent_key"] = "missing"
			warnings = append(warnings, "no agent key — run `deliverator onboard` (or set DELIVERATOR_AGENT_KEY for headless) before trading")
		}

		c, err := newClient(ctx)
		if err != nil {
			data["api_reachable"] = false
			emit("connect", data, append(warnings, "API unreachable: "+asError(err).Message)...)
			return output.ExitWith(output.ExitNetwork)
		}
		data["api_reachable"] = true
		data["query_address"] = c.QueryAddr()
		data["meta_age_secs"] = int(c.Meta().Age().Seconds())
		data["markets"] = len(c.Meta().Markets())
		data["halted"] = c.Halted()

		if skew, serr := c.MeasureSkew(ctx); serr == nil {
			data["clock_skew_ms"] = skew
			output.SetClockSkew(skew)
			if skew > 300000 || skew < -300000 {
				warnings = append(warnings, fmt.Sprintf("clock skew %d ms is large — check NTP", skew))
			}
		}
		if Cfg.Wallet.MasterAddress == "" {
			warnings = append(warnings, "wallet.master_address not set — reads will fail (set it to your MASTER address)")
		}
		emit("connect", data, warnings...)
		return nil
	},
}

// ---- config ----

var configCmd = &cobra.Command{Use: "config", Short: "Get/set config, or print its path", RunE: requireSubcommand}

var configPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Print the config file path",
	RunE: func(cmd *cobra.Command, args []string) error {
		emit("config.path", map[string]any{"path": config.Path()})
		return nil
	},
}

var configGetCmd = &cobra.Command{
	Use:   "get",
	Short: "Print the loaded config",
	RunE: func(cmd *cobra.Command, args []string) error {
		emit("config.get", Cfg)
		return nil
	},
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a config key (dotted) and save",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Mutate a FRESHLY-LOADED config, never the in-memory Cfg: PersistentPreRunE
		// applies transient global flags (e.g. --network) onto Cfg, and saving that
		// would silently persist them. A `--network mainnet config set <other>` must
		// not flip the box to mainnet on disk.
		fresh, err := config.Load(flagConfig)
		if err != nil {
			return fail("config.set", output.Validation("config_invalid", err.Error()))
		}
		// Write back to the SAME file we loaded from, so --config / $DELIVERATOR_CONFIG
		// is honored instead of silently editing the default config (#110).
		target := fresh.SourcePath()
		if target == "" {
			target = config.Path()
		}
		// Snapshot the prior value of a risk cap so the change can be surfaced loudly.
		oldVal, isRiskCap := riskCapValue(fresh, args[0])
		if err := setConfigKey(fresh, args[0], args[1]); err != nil {
			return fail("config.set", output.Validation("bad_config", err.Error()))
		}
		if err := fresh.Save(target); err != nil {
			return fail("config.set", output.Unknown("save", err.Error()))
		}
		var warnings []string
		if isRiskCap {
			// Deliverator never BLOCKS a cap change — the agent stays unobstructed —
			// but a safety limit moving must be LOUD so the human operator stays in
			// the loop. Surface it, never silently.
			warnings = append(warnings, fmt.Sprintf(
				"risk cap changed: %s %s → %s — Deliverator does NOT block this; confirm the account operator approved adjusting this safety limit.",
				args[0], oldVal, args[1],
			))
		}
		emit("config.set", map[string]any{"key": args[0], "value": args[1], "saved": target}, warnings...)
		return nil
	},
}

// riskCapValue returns the current value of a risk.* cap key as a string and
// whether key names a risk cap. It lets `config set` surface the before→after of
// a safety-limit change — loud so the operator is in the loop, never blocking
// (agent-in-the-loop policy; #110).
func riskCapValue(c *config.Config, key string) (string, bool) {
	r := c.Risk
	v := func(x any) string { return fmt.Sprintf("%v", x) }
	switch key {
	case "risk.max_order_notional_usd":
		return v(r.MaxOrderNotionalUSD), true
	case "risk.max_position_notional_usd":
		return v(r.MaxPositionNotionalUSD), true
	case "risk.min_order_notional_usd":
		return v(r.MinOrderNotionalUSD), true
	case "risk.max_leverage":
		return v(r.MaxLeverage), true
	case "risk.dead_man_switch_secs":
		return v(r.DeadManSwitchSecs), true
	case "risk.max_account_leverage":
		return v(r.MaxAccountLeverage), true
	case "risk.max_net_exposure_usd":
		return v(r.MaxNetExposureUSD), true
	case "risk.max_concentration_pct_per_coin":
		return v(r.MaxConcentrationPctPerCoin), true
	case "risk.max_drawdown_pct":
		return v(r.MaxDrawdownPct), true
	case "risk.max_daily_loss_usd":
		return v(r.MaxDailyLossUSD), true
	case "risk.max_daily_loss_pct":
		return v(r.MaxDailyLossPct), true
	case "risk.max_open_positions":
		return v(r.MaxOpenPositions), true
	case "risk.max_priority_bps":
		return v(r.MaxPriorityBps), true
	}
	return "", false
}

func setConfigKey(cfg *config.Config, key, val string) error {
	atoi := func() (int, error) { return strconv.Atoi(val) }
	atof := func() (float64, error) { return strconv.ParseFloat(val, 64) }
	switch key {
	case "network":
		cfg.Network = val
	case "wallet.master_address":
		cfg.Wallet.MasterAddress = val
	case "state.command_log":
		cfg.State.CommandLog = val
	case "state.audit_path":
		cfg.State.AuditPath = val
	case "state.audit":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		cfg.State.Audit = b
	case "state.meta_ttl_secs":
		n, err := atoi()
		if err != nil {
			return err
		}
		cfg.State.MetaTTLSecs = n
	case "builder.address":
		cfg.Builder.Address = val
	case "builder.fee_tenths_bps":
		n, err := atoi()
		if err != nil {
			return err
		}
		cfg.Builder.FeeTenthsBps = n
	case "builder.attach_mode":
		cfg.Builder.AttachMode = val
	case "risk.max_order_notional_usd":
		f, err := atof()
		if err != nil {
			return err
		}
		cfg.Risk.MaxOrderNotionalUSD = f
	case "risk.max_position_notional_usd":
		f, err := atof()
		if err != nil {
			return err
		}
		cfg.Risk.MaxPositionNotionalUSD = f
	case "risk.min_order_notional_usd":
		f, err := atof()
		if err != nil {
			return err
		}
		cfg.Risk.MinOrderNotionalUSD = f
	case "risk.max_leverage":
		n, err := atoi()
		if err != nil {
			return err
		}
		cfg.Risk.MaxLeverage = n
	case "risk.dead_man_switch_secs":
		n, err := atoi()
		if err != nil {
			return err
		}
		cfg.Risk.DeadManSwitchSecs = n
	case "risk.max_account_leverage":
		f, err := atof()
		if err != nil {
			return err
		}
		cfg.Risk.MaxAccountLeverage = f
	case "risk.max_net_exposure_usd":
		f, err := atof()
		if err != nil {
			return err
		}
		cfg.Risk.MaxNetExposureUSD = f
	case "risk.max_concentration_pct_per_coin":
		f, err := atof()
		if err != nil {
			return err
		}
		cfg.Risk.MaxConcentrationPctPerCoin = f
	case "risk.max_drawdown_pct":
		f, err := atof()
		if err != nil {
			return err
		}
		cfg.Risk.MaxDrawdownPct = f
	case "risk.max_daily_loss_usd":
		f, err := atof()
		if err != nil {
			return err
		}
		cfg.Risk.MaxDailyLossUSD = f
	case "risk.max_daily_loss_pct":
		f, err := atof()
		if err != nil {
			return err
		}
		cfg.Risk.MaxDailyLossPct = f
	case "risk.max_open_positions":
		n, err := atoi()
		if err != nil {
			return err
		}
		cfg.Risk.MaxOpenPositions = n
	case "alerting.webhook_url":
		cfg.Alerting.WebhookURL = val
	case "alerting.categories":
		cfg.Alerting.Categories = nil
		for _, c := range strings.Split(val, ",") {
			if c = strings.TrimSpace(c); c != "" {
				cfg.Alerting.Categories = append(cfg.Alerting.Categories, c)
			}
		}
	case "alerting.timeout_sec":
		n, err := atoi()
		if err != nil {
			return err
		}
		cfg.Alerting.TimeoutSec = n
	case "copy.default_scale_mode":
		cfg.Copy.DefaultScaleMode = val
	case "copy.default_scale":
		f, err := atof()
		if err != nil {
			return err
		}
		cfg.Copy.DefaultScale = f
	case "copy.min_diff_usd":
		f, err := atof()
		if err != nil {
			return err
		}
		cfg.Copy.MinDiffUSD = f
	case "copy.min_liq_distance_pct":
		f, err := atof()
		if err != nil {
			return err
		}
		cfg.Copy.MinLiqDistancePct = f
	case "copy.max_leverage":
		n, err := atoi()
		if err != nil {
			return err
		}
		cfg.Copy.MaxLeverage = n
	case "copy.max_orders_per_cycle":
		n, err := atoi()
		if err != nil {
			return err
		}
		cfg.Copy.MaxOrdersPerCycle = n
	case "automation.limit_only":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		cfg.Automation.LimitOnly = b
	case "automation.max_orders_per_min":
		n, err := atoi()
		if err != nil {
			return err
		}
		cfg.Automation.MaxOrdersPerMin = n
	case "automation.allowed_coins":
		if strings.TrimSpace(val) == "" {
			cfg.Automation.AllowedCoins = nil
		} else {
			parts := strings.Split(val, ",")
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}
			cfg.Automation.AllowedCoins = parts
		}
	case "perp_dexs":
		if strings.TrimSpace(val) == "" {
			cfg.PerpDexs = nil
		} else {
			parts := strings.Split(val, ",")
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}
			cfg.PerpDexs = parts
		}
	default:
		return fmt.Errorf("unknown or unsettable key %q", key)
	}
	return cfg.Validate()
}

// ---- account ----

var (
	aAlias   string
	aAddress string
)

var accountCmd = &cobra.Command{Use: "account", Short: "Manage sub-account aliases", RunE: requireSubcommand}

var accountLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List accounts",
	RunE: func(cmd *cobra.Command, args []string) error {
		emit("account.ls", map[string]any{"master": Cfg.Wallet.MasterAddress, "accounts": Cfg.Accounts})
		return nil
	},
}

var accountAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a sub-account alias → address",
	RunE: func(cmd *cobra.Command, args []string) error {
		if aAlias == "" || aAddress == "" {
			return fail("account.add", output.Validation("missing", "pass --alias and --address"))
		}
		if Cfg.Accounts == nil {
			Cfg.Accounts = map[string]string{}
		}
		Cfg.Accounts[aAlias] = aAddress
		if err := Cfg.Validate(); err != nil {
			return fail("account.add", output.Validation("bad_address", err.Error()))
		}
		if err := Cfg.Save(config.Path()); err != nil {
			return fail("account.add", output.Unknown("save", err.Error()))
		}
		emit("account.add", map[string]any{"alias": aAlias, "address": aAddress})
		return nil
	},
}

var accountRmCmd = &cobra.Command{
	Use:   "rm",
	Short: "Remove a sub-account alias (and its keychain key)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if aAlias == "" {
			return fail("account.rm", output.Validation("missing", "pass --alias"))
		}
		delete(Cfg.Accounts, aAlias)
		_ = wallet.Delete(aAlias)
		if err := Cfg.Save(config.Path()); err != nil {
			return fail("account.rm", output.Unknown("save", err.Error()))
		}
		emit("account.rm", map[string]any{"alias": aAlias, "removed": true})
		return nil
	},
}

var accountSetDefaultCmd = &cobra.Command{
	Use:   "set-default",
	Short: "Set the master (default) address",
	RunE: func(cmd *cobra.Command, args []string) error {
		if aAddress == "" {
			return fail("account.set-default", output.Validation("missing", "pass --address"))
		}
		Cfg.Wallet.MasterAddress = aAddress
		if err := Cfg.Validate(); err != nil {
			return fail("account.set-default", output.Validation("bad_address", err.Error()))
		}
		if err := Cfg.Save(config.Path()); err != nil {
			return fail("account.set-default", output.Unknown("save", err.Error()))
		}
		emit("account.set-default", map[string]any{"master_address": aAddress})
		return nil
	},
}

// ---- init ----

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Generate an agent key in the keychain and print approval steps",
	RunE: func(cmd *cobra.Command, args []string) error {
		// A fresh box gets a commented config template; the agent key is generated
		// straight into the OS keychain (the only source).
		if _, statErr := os.Stat(config.Path()); os.IsNotExist(statErr) {
			if werr := writeConfigTemplate(); werr != nil {
				return fail("init", output.Unknown("config_write", werr.Error()))
			}
		}
		ag, err := wallet.Generate(flagAccount)
		if err != nil {
			return fail("init", output.Auth("keygen", err.Error()))
		}
		data := map[string]any{
			"agent_address": ag.Address,
			"network":       Cfg.Network,
			"key_source":    "keychain",
			"config_path":   config.Path(),
			"next_steps": []string{
				"1. In the Hyperliquid web UI, approve this agent: API > approve agent address " + ag.Address,
				"2. Set your MASTER address: deliverator config set wallet.master_address 0x...",
				"3. (optional) approve the 0.05% Deliverator builder fee to fund development — orders trade fee-free until you do (`deliverator builder status`)",
				"4. Verify: deliverator connect",
			},
		}
		// The secret is never surfaced — it lives only in the keychain. (To import an
		// existing API wallet key instead of generating one, use `deliverator onboard`.)
		emit("init", data, "the agent wallet CANNOT withdraw — deposits/withdrawals stay in the browser")
		return nil
	},
}

func writeConfigTemplate() error {
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(config.Path(), []byte(configTemplate), 0o600)
}

const configTemplate = `# Deliverator config. Secrets live in the keychain, never here.
network = "testnet"            # mainnet | testnet

[wallet]
master_address = ""            # QUERY target — your MASTER address. NEVER the agent address.
# The agent signing key is stored ONLY in the OS keychain — add it with
# ` + "`deliverator onboard`" + ` (import) or ` + "`deliverator init`" + ` (generate).

[builder]
# Deliverator ships its builder fee ON by default to help fund the project — but it is
# GRACEFUL: it is only ever charged after you sign the one-time master approveBuilderFee
# (the non-custodial agent key cannot sign that). Until then, orders are placed fee-free,
# so you are never blocked from trading. Approve it to support development, repoint
# address at your own builder, or set attach_mode = "manual" to opt out.
address = "0x2D2AEf445717466eD8DBEfd2751cDc42369Fba75"  # Deliverator builder EOA (override with your own)
fee_tenths_bps = 50            # 50 = 0.05%; perps cap 100
attach_mode = "all"            # all | manual

[risk]
max_order_notional_usd = 10000
max_position_notional_usd = 50000  # per-coin
min_order_notional_usd = 10    # reject sub-minimum orders pre-flight; 0 = no floor (HL rejects ~<$10)
max_leverage = 10
dead_man_switch_secs = 0       # 0 = off
# Account-wide portfolio gates (0 = off). Enforced before signing against the
# resulting book; reduce-only/close legs are exempt. "Equity" = perp account_value,
# falling back to available USDC collateral when flat (unified account).
max_account_leverage = 0           # gross notional / equity
max_net_exposure_usd = 0           # |long − short| in USD
max_concentration_pct_per_coin = 0 # one coin's notional as % of equity
max_drawdown_pct = 0               # % drop from the equity high-water (persistent)
max_daily_loss_usd = 0             # loss since the UTC-day equity anchor (USD)
max_daily_loss_pct = 0             # same as %
max_open_positions = 0             # cap on concurrent open positions (0 = off)

[automation]
allowed_coins = []             # empty = allow all
limit_only = false
max_orders_per_min = 120
json_when_not_tty = true

[state]
meta_ttl_secs = 3600
audit = true                   # append every signed action to audit.jsonl (the money trail)
# command_log = ""             # also log EVERY command (argv + exit) here; "deliverator logs -f" to watch live

[alerting]
webhook_url = ""               # POST a JSON alert on RED-state failures; "" = off (env DELIVERATOR_ALERT_WEBHOOK overrides)
categories = []                # which error categories fire; empty = halt,auth,timeout
timeout_sec = 5                # webhook POST timeout

[copy]
default_scale_mode = "equity"  # equity (proportional to your equity) | fixed (leader size * scale)
default_scale = 1.0            # multiplier on the leader's sizes
min_diff_usd = 0               # skip diff legs below this $ delta (0 = off)
min_liq_distance_pct = 0       # skip open/increase legs whose est liq is closer than this % (0 = off)
max_leverage = 0               # per-leg size clip hint, NOT a backstop (0 = off; the risk.* gates are the backstop)
max_orders_per_cycle = 0       # cap legs executed per cycle (0 = no cap)

[accounts]
# vault1 = "0x..."
`

func init() {
	configCmd.AddCommand(configPathCmd, configGetCmd, configSetCmd)

	accountAddCmd.Flags().StringVar(&aAlias, "alias", "", "account alias")
	accountAddCmd.Flags().StringVar(&aAddress, "address", "", "account address")
	accountRmCmd.Flags().StringVar(&aAlias, "alias", "", "account alias")
	accountSetDefaultCmd.Flags().StringVar(&aAddress, "address", "", "master address")
	accountCmd.AddCommand(accountLsCmd, accountAddCmd, accountRmCmd, accountSetDefaultCmd)

	rootCmd.AddCommand(toolsCmd, schemaCmd, connectCmd, configCmd, accountCmd, initCmd)
}
