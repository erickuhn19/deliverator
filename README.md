# Deliverator

**A single-binary Go CLI: the non-custodial execution + tracking layer an
autonomous agent (OpenClaw) drives to trade and manage a Hyperliquid portfolio.**

> Codename ref: *Snow Crash* — the Deliverator. Fast, precise, no-BS.
>
> **OpenClaw** is the reference autonomous trading agent that drives Deliverator;
> Deliverator itself is agent-agnostic — any LLM agent or script that speaks the
> CLI contract (`deliverator tools` / [`AGENTS.md`](./AGENTS.md)) can drive it.

Deliverator is the safe harness you put **between an autonomous agent and a
Hyperliquid account.** It owns the hard, dangerous parts once — EIP-712 signing,
nonce management, precision rounding, builder attach, risk caps, rate-limit
pacing — and exposes them as a disciplined, machine-parseable CLI. The agent
decides *what* to trade; Deliverator makes sure the *how* is correct, bounded,
and auditable.

Because it signs with a Hyperliquid **agent/API wallet that physically cannot
withdraw**, the worst a confused or compromised agent can do is place bad
trades — never drain funds. That non-custodial guarantee is the whole point.

No backend. No telemetry. `scp` the binary to a box and go.

> ⚠️ **This places real orders with real money on a live exchange.** Deliverator
> is **testnet-first** for a reason — flip to `mainnet` only when you mean it.
> The non-custodial key means a bug can't *drain* your account, but it can still
> place trades you didn't intend. Provided under the MIT license, **with no
> warranty** ([LICENSE](./LICENSE)). You are responsible for what your agent does.

---

## Target Use Case & Positioning

Deliverator is the **execution and safety layer** for autonomous agents — particularly LLM-driven ones — that trade on Hyperliquid. The intro above covers *what* it does; this section is about *when to reach for it* instead of the alternatives.

### The problem it targets

When an LLM is in the decision loop, a handful of failure modes recur:

- Hallucinated sizes, prices, leverage, or order types
- Brittle retry logic that double-fills
- Missing or inconsistent risk checks
- Custody risk when full wallet keys are exposed to the agent

Deliverator is built to absorb exactly these. Most general-purpose Hyperliquid tooling wasn't.

### Where other tools fit

| Category | Examples | Primary strength | Best for | Limitations for autonomous agents |
|---|---|---|---|---|
| **Low-level SDKs** | Official `hyperliquid-python-sdk` | Full control, lightweight, official | Custom development & deep integration | No built-in safety, risk engine, or agent ergonomics |
| **Persistent bot frameworks** | Hummingbot (Hyperliquid connector) | Mature strategy engine + risk controls | Rule-based / market-making bots | Heavier for dynamic, decision-making agent loops |
| **Human + scripting CLIs** | `hyperliquid-cli` (TS), similar community CLIs | Nice TUI, real-time monitoring, JSON output | Human traders + light automation | Limited portfolio-level risk enforcement & agent contract |
| **Agent execution layer** | **Deliverator** | Non-custodial safety + machine-native contract | LLM agents & autonomous trading loops | Newer project (early 2026) |

### Choosing between them

- **Deliverator** — you're running an autonomous agent (especially an LLM) that must trade real capital in a loop, with non-custodial keys and strong safety guarantees, and you don't want to babysit it.
- **Official SDK** — you want maximum flexibility and are prepared to build your own safety, retry, and risk layers on top.
- **Hummingbot** — you need persistent, strategy-driven automation (especially market making) rather than dynamic, decision-making agent loops.
- **Human-oriented CLI** — the main user is a person at a terminal, or you only need occasional scripted automation.

Deliverator isn't trying to replace the Hyperliquid SDK or Hummingbot. It occupies a specific niche: a safe execution harness purpose-built for agents that trade real capital without supervision.

---

## Why it exists

The caller is an LLM, not a human. So Deliverator is:

- **Deterministic & parseable** — one JSON object per command (schema v1), NDJSON for streams.
- **Branchable** — every command returns a documented exit code (`$?`), never relies on stdout text.
- **Idempotent via protocol** — every write carries a `cloid`. The exchange does *not* reject a duplicate cloid (a blind resend places a second order), so the defined timeout-retry protocol — check order status by cloid *before* resubmitting — is what actually prevents double-fills.
- **Self-correcting** — errors are actionable text with concrete numbers ("round px to 64000"), not opaque codes.
- **Bounded** — hard risk caps enforced in core, not the surface. Switching invocation can't bypass them.
- **Self-describing** — `markets`, `schema`, `tools`, `version` let the agent discover capability at runtime.

The agent contract is the product. See [`TOOLS.md`](./TOOLS.md) (also `deliverator tools`).

---

## Install

**Homebrew** (macOS/Linux):

```sh
brew install --cask erickuhn19/tap/deliverator
```

Or with **Go**:

```sh
go install github.com/erickuhn19/deliverator@latest
# or build from source:
go build -o deliverator .
```

Building from source requires Go 1.25+. macOS/Linux (uses `flock` + the OS keychain).

---

## Verifying a download

Every release ships sha256 `checksums.txt` and **SLSA build-provenance attestations** —
signed proof that each binary was built by this repo's CI from a specific commit (so a
tampered or substituted binary won't verify). To check a downloaded archive:

```sh
gh attestation verify deliverator_<version>_<os>_<arch>.tar.gz --repo erickuhn19/deliverator
```

`brew install` enforces integrity automatically via the cask's pinned sha256.

---

## Quickstart

**Easiest — guided setup** (creates your account via the referral link for a fee
discount, then takes your API wallet key and configures everything):

```sh
deliverator onboard
```

Or set up manually:

```sh
# 1. Generate a fresh agent key locally (stored in the OS keychain) + a config template.
deliverator init
#    → prints the agent ADDRESS and the approval steps.

# 2. In the Hyperliquid web UI, approve that agent address (API → approve agent).
#    Then point Deliverator at your MASTER address (the query target):
deliverator config set wallet.master_address 0xYOURMASTER...

# 3. Preflight.
deliverator connect          # key, account, network, clock skew, API, meta age

# 4. Read state (one call = full snapshot).
deliverator portfolio --json

# 5. Place a bounded, idempotent order (preview first).
deliverator buy BTC 0.01 --limit 64000 --alo --dry-run
deliverator buy BTC 0.01 --limit 64000 --alo --cloid 0x...   # for real

# 6. Safety.
deliverator dms set 60       # arm the dead-man's switch (refresh via cron: dms heartbeat)
deliverator halt on          # emergency stop — rejects all new orders
deliverator panic --yes      # cancel-all + flatten-all
```

Deliverator is **testnet-first** (`network = "testnet"` by default). Flip to
`mainnet` in the config only when you mean it.

---

## The agent contract

Every command emits one JSON envelope:

```json
{
  "schema": "v1",
  "ok": true,
  "ts": 1750000000000,
  "cmd": "buy",
  "data": { "cloid": "0x..", "status": "resting", "oid": 123, "coin": "BTC", "size": "0.01", "limit_px": "64000" },
  "error": null,
  "warnings": ["size rounded 0.0123456 -> 0.0123"],
  "meta": { "network": "testnet", "account": "main" }
}
```

**Prices and sizes are always strings.** On failure, `ok=false`, `data=null`,
and `error` is populated with `{code, category, message, retryable, retry_after_ms, hint}`.

### Exit codes
| Code | Meaning | Agent action |
|---|---|---|
| 0 | success | proceed |
| 10 | validation (bad args / unknown coin) | fix inputs |
| 11 | precision rejected (`--strict`) | re-round and retry |
| 20 | risk-rejected (cap/allowlist/limit-only/leverage) | respect the cap |
| 21 | halted | stop trading |
| 30 | auth/key error | operator fixes the key |
| 40 | network/unreachable | retry w/ backoff |
| 41 | rate-limited | back off (`retry_after_ms`) |
| 42 | timeout (outcome unknown) | run the retry protocol — **don't blind-resubmit** |
| 50 | exchange-rejected | read message, adjust |
| 60 | partial fill | inspect fills, decide |
| 70 | clock skew outside nonce window | fix the clock |

### Retry protocol (critical)
On **exit 42**, run `deliverator order status --cloid <id>`. If the order exists
→ it landed, don't resend. If absent → resubmit the **same** cloid. This is the
#1 way naive agents double-fill.

The exchange can take ~1–2s to index a new order by cloid, so a status check
immediately after a timeout may report "absent" for an order that actually
landed. Wait briefly and re-check (or query by `--oid`) before resubmitting.

---

## Safety model

| Guard | Config | Behavior |
|---|---|---|
| Coin allowlist | `automation.allowed_coins` | reject non-listed (20); empty = allow all |
| Max order notional | `risk.max_order_notional_usd` | reject (20) |
| Max position notional | `risk.max_position_notional_usd` | reject (20); per-coin |
| Min order notional | `risk.min_order_notional_usd` | reject sub-minimum orders pre-flight (10); default $10, mirrors HL's floor |
| Limit-only | `automation.limit_only` | block market orders (20) |
| Max leverage | `risk.max_leverage` | cap leverage changes (20) |
| **Account leverage** | `risk.max_account_leverage` | reject if resulting gross notional / equity exceeds it (20) |
| **Net exposure** | `risk.max_net_exposure_usd` | reject if resulting \|long − short\| exceeds it (20) |
| **Per-coin concentration** | `risk.max_concentration_pct_per_coin` | reject if one coin exceeds that % of equity (20) |
| **Drawdown** | `risk.max_drawdown_pct` | reject new exposure once equity is that % below its high-water (20) |
| **Daily loss** | `risk.max_daily_loss_usd` / `_pct` | reject new exposure once loss since UTC-midnight anchor exceeds it (20) |
| **Max open positions** | `risk.max_open_positions` | reject opening a new coin once at the concurrent-position cap (20) |
| **Reduce-only flip** | (always on) | reject a reduce-only order larger than the open position — it could only cross zero (20) |
| Local rate cap | `automation.max_orders_per_min` | throttle before the exchange limit |
| Global halt | `deliverator halt on` | reject all new orders (21) |
| Dead-man's switch | `risk.dead_man_switch_secs` | schedule-cancel; refresh via `dms heartbeat` |
| **Real-time failsafe** | `deliverator watch --metric liq_distance_pct --below N --action panic\|dms\|alert` | stream-driven: trigger the action the moment the metric breaches — catches a mid-interval move the DMS/heartbeat can't |
| Dry-run | `--dry-run` | validate/round/attach, never send |

**Alerting:** set `alerting.webhook_url` (or `DELIVERATOR_ALERT_WEBHOOK`) to POST a JSON event on RED-state failures (halt/auth/timeout by default; add `risk` etc. via `alerting.categories`) — best-effort, never blocks the command. Wire it to Slack/Discord/a relay so an away operator hears within seconds.

The threat model is an LLM hallucinating a size, price, or leverage. The CLI is
the only enforcement point — every value is treated as hostile until checked.

---

## Wallet model

| Wallet | Funds | Signs | Where |
|---|---|---|---|
| **Master** | yes | `approveAgent`, `approveBuilderFee`, deposit/withdraw | Browser/hardware — **never here** |
| **Agent / API** | no | orders, cancels, modifies, leverage, margin, schedule-cancel | Deliverator (keychain) |

- **Reads use the master address.** Querying the agent address returns empty — the canonical bug.
- **The agent key lives in the OS keychain by default** — add it with `deliverator onboard` (import your API wallet key) or `deliverator init` (generate one). For headless/CI hosts with no keychain, set `DELIVERATOR_AGENT_KEY` to inject the key; it's used **only when set** and otherwise ignored, so an unset/empty env can never hide the keychain key. There is no stored `agent_key_source` config (that indirection was the original "key looks deleted" bug). If no key is available, every write fails with `auth/no_agent_key` and a hint to run `onboard`.
- Rotate with `deliverator init` (fresh address). Never reuse a deregistered agent address.
- Deposits/withdrawals/transfers are **out of scope by design** — that exclusion *is* the guarantee.

---

## Commands

Setup: `init` · `connect`/`health` · `version` · `config [get|set|path]` ·
`account [add|ls|rm|set-default]` · `markets` · `schema` · `tools`

Track (reads): `snapshot` (one-call tick: portfolio + limits + ctx[coins] + builder) ·
`portfolio` · `positions` · `orders` · `order status` · `fills` ·
`funding` · `ledger` · `balance` · `pnl` · `book` · `bbo` · `mids` · `candles` ·
`ctx` (perp + spot; carries `impact_pxs` for slippage) · `builder status` ·
`referral status` · `limits` · `predicted-fundings` (forward funding-carry signal) ·
`historical-orders` (closed-order lifecycle) · `twap status` (running TWAPs + slice fills) ·
`leaderboard` (official HL trader leaderboard — filter/sort/drill-down to find an address to `copy`) ·
`reconcile` (diff local audit vs live; run first after a restart) ·
`preview` (what-if: resulting leverage/margin/liq for an order, no signing) ·
`info <type> [k=v]` (raw passthrough to any HL info endpoint)

`positions`/`portfolio` also carry computed risk fields: `distance_to_liq_pct`
per position, and account `maintenance_margin` + `margin_ratio`.

Submit (writes):
- Orders: `buy`/`sell`/`order` — market, limit, IOC (`--ioc`), post-only (`--alo`),
  trigger (`--trigger`); `--tp`/`--sl` places a **linked OCO bracket** (one grouped
  `normalTpsl` action — a filled TP cancels the resting SL).
- Many-at-once: `batch` (a JSON array of orders) · `grid` (a limit ladder) ·
  `modify-batch` — each is **one signed action** (one nonce, atomic pre-flight).
- Manage: `modify` · `cancel` (by `--oid`/`--cloid`, the `--oids`/`--cloids` lists,
  or `--all [--coin]`) · `close` (flatten a **perp** position or sell a **spot**
  holding) · `position-tpsl` (reduce-only TP/SL attached to a **whole position**) ·
  `chase` (BBO-pegged passive-maker limit that re-prices as the book moves) ·
  `leverage` · `margin` · `twap` (+ `twap cancel` / `twap status`).
- Account: `referral apply` · `onboard`.
- Find + mirror a leader: `leaderboard` screens the official HL leaderboard (filter by window PnL/ROI/volume/account-value, `--profitable-in day,week,month` for consistency, `--sort`, paging) → pick a `data.rows[].address` → `copy <leader>` — non-custodial copy-trading; diff (read-only) by default, `--execute --yes` routes legs through the guarded order path (all risk gates apply). Stateless: pass `--mirrored`, persist `data.mirrored_now`.
- Safety: `dms` · `halt` · `panic` · `watch` (real-time failsafe: evaluate a live risk metric, trigger `alert`/`dms`/`panic` on breach — the reactive counterpart to the DMS).

Stream (live NDJSON): `stream book|bbo|trades|candles|fundings|active-asset|mids|fills|orders|webdata2|twap-fills|events`

Run `deliverator <cmd> --help` for flags. Config lives at
`~/.config/deliverator/config.toml` (see `deliverator config path`).

---

## Development

```sh
go test ./...                 # signing parity, risk gauntlet, engine, precision, envelope, nonce
go test -race ./...           # the suite is race-clean
go vet ./...
scripts/check-coverage.sh     # per-package coverage floors
go build -o deliverator .
```

Architecture: `cmd/` is a thin cobra adapter; **all correctness and safety live
in `internal/core`**, which drives `internal/hl` (the from-scratch Hyperliquid
API client + EIP-712 signer). `internal/output` owns the schema-v1 envelope +
exit codes + error catalog; `internal/config` the TOML; `internal/wallet` key
sources; `internal/state` the nonce flock + audit log.

Client: a native `internal/hl` package talks directly to the Hyperliquid API —
EIP-712 action signing ported from the official Python SDK, no third-party SDK.

See [CONTRIBUTING.md](./CONTRIBUTING.md) for the architecture invariants a change
must hold (thin surface, golden-vector signing, coverage floors, no secrets).

---

## How this is funded

Deliverator is free and MIT-licensed. It's funded by an **optional builder fee** —
**0.05%** (5 bps), on by default — routed to the project's Hyperliquid builder
address. It is **graceful and non-custodial**: the fee is only ever charged once you
sign the one-time `approveBuilderFee` with your **master wallet** (the agent key
can't — that's the whole guarantee). Until you approve it, **every order trades
fee-free** — you're never blocked, and never charged without consent. Spot *buys*
never carry it (Hyperliquid takes the taker fee in the base token).

If Deliverator is useful to you, approving the fee is the easiest way to support its
development. To opt out or self-host, repoint `builder.address` at your own builder
or set `builder.attach_mode = "manual"`. New accounts also get a referral fee
discount via `deliverator onboard`.

---

## License & security

- **License:** MIT — see [LICENSE](./LICENSE). Provided as-is, with no warranty.
- **Security:** found a vulnerability? Please report it privately — see
  [SECURITY.md](./SECURITY.md). Do not open a public issue.
- **Contributing:** [CONTRIBUTING.md](./CONTRIBUTING.md).
