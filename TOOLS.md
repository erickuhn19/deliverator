# Deliverator — Hyperliquid trading CLI (agent contract)

You trade and track a Hyperliquid portfolio by running `deliverator` commands.
Deliverator signs with an **agent/API wallet that cannot withdraw** — the worst a
bad call can do is place a bad trade, never move funds.

## The protocol
- **Always pass `--json`.** Parse stdout as one JSON object:
  `{schema, ok, ts, cmd, data, error, warnings, meta}`. Streams emit one object per line (NDJSON).
- **Branch on the exit code, not on text.** Read `$?` after every command.
- **Prices and sizes are strings** everywhere. Keep them as strings; do not parse to float.
- Read `warnings` — auto-rounding and applied builder fees are reported there.
- **Codegen the shapes:** `deliverator schema` dumps the envelope; `deliverator schema <command>` (e.g. `schema positions`, `schema preview`) dumps the JSON Schema of that command's `data` payload; `deliverator schema commands` lists the describable commands.

## Reading state
| Need | Command |
|---|---|
| **One-call tick read** (portfolio + rate-limit budget + ctx for held coins + builder) | `deliverator snapshot --json` |
| Full snapshot (positions, orders, balances, margin, uPnL) | `deliverator portfolio --json` |
| Positions / resting orders | `deliverator positions --json` · `deliverator orders --json` |
| One order's status | `deliverator order status --cloid <ID> --json` |
| Fills (incl. fee, builderFee, closedPnl) | `deliverator fills --json` |
| Net session P&L (did this make money?) | `deliverator pnl attribution --json` (per coin + total: realized − fees − builder fee + funding; `--since <ms>` / `--coin <C>`) |
| Market data | `deliverator book <COIN> --json` · `deliverator bbo <COIN> --json` · `deliverator mids --json` |
| Per-coin context (mark/oracle/funding/OI/premium + **impact_pxs** for slippage) | `deliverator ctx <COIN> --json` |
| Forward funding (carry signal) | `deliverator predicted-fundings [--coin <C>] --json` (next-interval forecast per coin+venue) |
| Closed-order history (filled/canceled/rejected/expired) | `deliverator historical-orders [--limit N] --json` (reconciliation + post-mortem) |
| Running TWAPs + slice fills | `deliverator twap status [--coin <C>] [--id <ID>] --json` |
| Tradable universe + precision rules | `deliverator markets --json` |
| **Find an address to copy-trade** (official leaderboard, filter/sort/drill-down) | `deliverator leaderboard --json` (see **Finding a leader to copy**) |
| Live updates | `deliverator stream events --json` (NDJSON; one object per line — see **Streams**) |
| Any other HL info endpoint | `deliverator info <type> [key=value ...] --json` (raw passthrough; `@`=your address) |

> **`snapshot`** is the one-call read for a loop tick: `data.{portfolio, limits,
> builder_status, ctx}` each carry their own `ok`/`error`, so check `section.ok`
> per section (a partial failure is a top-level `warning` listing the failed
> sections, not a failed command). `ctx` defaults to the coins you have exposure to
> (positions + open orders); pass `--coins BTC,ETH` to prime others. Balances and
> open orders live under `data.portfolio.data` (not duplicated as separate sections).

> **Risk-aware sizing:** `positions`/`portfolio` carry `distance_to_liq_pct` (per
> position, exact) and account `maintenance_margin` + `margin_ratio`. Before
> committing, `deliverator preview <coin> <buy|sell> <size> [--limit <px>] [--leverage <L>]`
> projects the resulting position, **resulting account leverage**, margin required,
> and an estimated liquidation price — without signing. Size so an order stays
> inside your margin/liq bounds. (`est_liquidation_px` is a single-position isolated
> estimate; for an existing position use `positions.liquidation_px`.)

> `info` is the escape hatch for endpoints without a dedicated command — e.g.
> `info fundingHistory coin=BTC startTime=<ms>`, `info spotMetaAndAssetCtxs`,
> `info tokenDetails tokenId=0x…`. (`historicalOrders`, `predictedFundings`, and
> `userTwapSliceFills` now have typed commands above — prefer those.) Numeric
> values are sent as numbers; `@` expands to your query address.

> **Unified account / collateral (read this before gating on a balance).** Hyperliquid
> accounts are unified: your spendable collateral is reported as **`available_collateral`**
> in `balance`/`portfolio`, and it backs the **main dex AND every HIP-3 sub-dex** (`xyz:*`).
> The per-dex (and main) **`account_value` reads `0.0` when you hold no position there** —
> that means *flat*, **not** "out of funds." So decide "can I afford this trade?" from
> `available_collateral`, never from `account_value`. When collateral is shared the balance
> carries `collateral_shared: true` and each `perp_dexs` entry a `note` saying so. You can
> open a sub-dex position (e.g. `buy xyz:GOLD …`) straight from the shared collateral with
> no transfer.

> **HIP-4 outcome (prediction) markets.** Opt in with `outcomes = true` in config. Each
> market is a **binary Yes/No** leaf priced as a **probability in (0,1)** (Yes mid + No mid
> ≈ 1). Coins are `#<encoding>` where `encoding = 10*outcome + side` (side 0 = Yes, 1 = No)
> — e.g. outcome 641 → `#6410` (Yes) / `#6411` (No). **Discover** them with
> `deliverator markets --class outcome [--status open|settled]` (kept out of the default
> `markets` listing — there are hundreds and they rotate); each row carries `title`, `side`,
> `question`/`question_name` (the grouping, e.g. a tournament), `underlying`/`target_price`/
> `expiry` for price binaries, and `resolution_status`. `deliverator ctx #6410` returns the
> probability (`mid_px`), the other side's implied probability (`complement_mid`), and the
> book BBO. **Trade** them like any coin: `buy #6410 <size> --limit <px>` (sizes are integers,
> price ≤5 sig figs / ≤5 decimals in (0,1), ~$10 min). **No leverage, no liquidation** — fully
> collateralized, so max loss = `size × price` and max gain = `size × (1 − price)` — project
> these with `deliverator preview #6410 buy <size> --limit <px>` (reports `at_stake_usd` /
> `max_gain_usd`; leverage/liquidation are N/A). **To exit**, `deliverator close #6410`
> (sells the full held side) or `sell #6410 <size>`; positions otherwise **settle
> automatically** at `expiry` (Yes → 1, No → 0). **Holdings** surface as `class:"outcome"`
> rows in `positions`/`portfolio` (side, `mark_px` probability, `at_stake_usd`,
> `max_gain_usd`, `unrealized_pnl`) and raw in `balance` spot as `+<encoding>` tokens.

## Placing orders
- Market buy:  `deliverator buy <COIN> <SIZE> --cloid <ID> --json`
- Limit sell:  `deliverator sell <COIN> <SIZE> --limit <PX> --alo --cloid <ID> --json`
  (`--ioc` immediate-or-cancel, `--alo` post-only; omit `--limit` for market)
- **Linked OCO bracket:** add `--tp <PX> --sl <PX>` — entry + take-profit + stop-loss
  go out as ONE grouped action; a filled TP auto-cancels the resting SL.
- **Position-level TP/SL:** `deliverator position-tpsl <COIN> [--tp <PX>] [--sl <PX>] [--size <SZ>] --json`
  attaches reduce-only take-profit/stop-loss triggers to your **whole existing position**
  (not linked to one entry, unlike the bracket). Side is derived from the position
  (a long → SELL triggers); `--size` defaults to the full position. Reduce-only, so no
  portfolio gate / $10 floor applies; perp-only; works on `xyz:*` sub-dex coins.
- Trigger order: `--trigger <PX> [--trigger-type tp|sl] [--trigger-market]`
- Size by USD: `--notional <usd>` on `buy`/`sell`/`order` (and a `notional` field per batch leg) — Deliverator derives `size = notional / price` (the `--limit`, else the live mid), then rounds + risk-checks it. Omit the size argument; passing both a size and `--notional` is rejected.
- Reduce only: add `--reduce-only`
- TWAP (sliced over N minutes, min ~$50 / $10 per minute): `deliverator twap <COIN> <buy|sell> <SIZE> --minutes <N> --json`;
  cancel with `deliverator twap cancel --coin <COIN> --id <TWAP_ID> --json`
- **Chase (passive maker):** `deliverator chase <COIN> <buy|sell> <SIZE> [--offset <PX>] [--max-reprices N] [--timeout <DUR>] --json`
  places a post-only limit pegged to the BBO and re-prices it (via modify, same cloid)
  as the touch moves — a long-running NDJSON command (one line per step) that follows
  the book instead of going stale. `--offset` sits behind the touch (buy=bid−offset,
  sell=ask+offset). Runs until filled, Ctrl-C, `--timeout`, or `--max-reprices`; cancels
  the resting order on exit unless `--leave-resting`.
- Spot: `<COIN>` may be a spot pair (`PURR/USDC` or `@<index>`) — same buy/sell.
- Outcome: `<COIN>` may be a HIP-4 outcome (`#<encoding>`) — same buy/sell; price is a probability in (0,1), no leverage/liquidation. Discover with `markets --class outcome` (see the HIP-4 note above).

### Many orders in one signed action (one nonce, atomic pre-flight)
- Batch:  `deliverator batch --file orders.json --json` (or pipe the JSON array on stdin).
  Each element: `{coin, side, size, limit?, tif?, reduce_only?, cloid?, slippage?, trigger?}`
  (sizes/prices may be strings or numbers). Any leg failing validation rejects the
  whole batch before signing; HL may still reject individual legs (per-leg results).
- Grid:   `deliverator grid <COIN> <buy|sell> --levels <N> --from <PX> --to <PX> --size <TOTAL> --json`
  — a ladder of `N` limits, `TOTAL` split evenly. Each level must clear the $10 minimum.

- **ALWAYS supply a unique `--cloid` you generate per intent** (hex, `0x` + 32 chars).
  The cloid is your idempotency key for the retry rule below. The exchange does
  NOT reject a duplicate cloid — a blind resend places a *second* order — so never
  resend without first checking order status by cloid.
- Orders below ~$10 notional are rejected pre-flight (exit 10, `risk.min_order_notional_usd`).
- Preview without sending: add `--dry-run` (validates, rounds, attaches builder, shows the exact action).

## Finding a leader to copy (leaderboard)
`deliverator leaderboard --json` pulls the **official Hyperliquid trader leaderboard** (first-party data from `stats-data.hyperliquid.xyz`, the same source the web app uses — no third-party API) and lets you filter/sort/page it to find a profitable, *active* address to feed to `copy`. Every row carries `pnl`/`roi`/`vlm` for **all four windows** (`day`/`week`/`month`/`all_time`) plus `account_value`; `--window` picks which window the sort and the metric filters act on, but all windows are always returned.

| Need | Command |
|---|---|
| Top 25 by today's PnL (default) | `deliverator leaderboard --json` |
| Best ROI this week, sizeable + active accounts only | `deliverator leaderboard --window week --sort roi --min-account-value 50000 --min-vlm 1000000 --json` |
| **Consistently** profitable (day **and** week **and** month) | `deliverator leaderboard --profitable-in day,week,month --sort pnl --window month --limit 10 --json` |
| Only profitable rows in the chosen window (pnl>0 & roi>0) | `deliverator leaderboard --window day --profitable --json` |
| Drill down on specific addresses | `deliverator leaderboard --address 0xabc…,0xdef… --json` |
| Next page | `deliverator leaderboard --limit 25 --offset 25 --json` |
| **Sane-risk** candidates trading **now** (real equity, ≤10× lev) | `deliverator leaderboard --profitable-in day,week,month --min-roi 0.35 --sort roi --limit 40 --in-market --max-live-leverage 10 --min-live-equity 10000 --max-live-equity 100000 --json` |
| Strong history, **currently in cash** (watch their next trade) | `deliverator leaderboard --profitable-in week,month --min-roi 0.35 --flat --limit 25 --json` |

- **Sort:** `--sort pnl|roi|vlm|account_value|prize` · `--order desc|asc` (default `pnl`/`desc`).
- **Filters** (all optional; bounds compare against the `--window` window): `--min/max-pnl` (USD), `--min/max-roi` (**fraction**, `0.1` = 10%), `--min/max-vlm` (USD — volume is the best proxy for *recent activity*), `--min/max-account-value`, `--min-prize`, `--named` (has a public name).
- **Live state** (`--live`, or implied by any filter below): enriches each **returned** row with its CURRENT positions via one `clearinghouseState` read — bounded by `--live-scan` (default 25, max 100). The board's `account_value` is **stale**; live filters use real-time data. `--in-market` (holding a position now) / `--flat` (in cash — watch their next trade; mutually exclusive with `--in-market`) · `--max-live-leverage N` (drop the 40× degens) · `--min/max-live-equity` (real equity, USD). Each enriched row gains a `data.rows[].live {equity, open_positions, max_leverage, coins}`; `data.live_scanned` reports how many were checked. Requires a bounded `--limit` (not `0`).
- **Output:** `data.rows[]` ranked (`rank` is 1-based within the matched+sorted set, so it survives paging), with `data.{total_rows, matched, returned, live_scanned}`.
- **Caching:** the ~32 MB board is cached locally and revalidated with a cheap conditional GET (`leaderboard_ttl_secs`, default 300s), so repeat calls are near-instant and don't re-download it.
- **Pick → copy:** choose a `data.rows[].address` and hand it to `deliverator copy <addr>` (below). A good copy candidate is profitable across **multiple** windows (not just one lucky day) with non-zero recent `vlm` (still trading) and an `account_value` large enough that its sizing scales sanely to yours.
- **Verify before copying:** the leaderboard is a screen, not a green light. Confirm the candidate with `deliverator copy <addr>` (read-only diff — shows their live book) and `deliverator info clearinghouseState user=<addr>` / `info userFills user=<addr>` before `--execute`.

## Copy-trading (mirror a leader)
`deliverator copy <leader_address>` mirrors a leader's public perp book onto your account, scaled to your equity. **Diff-first:** by default it only *shows* the trades that would bring your book to the leader's (read-only, signs nothing). Add `--execute --yes` to place the surviving legs through the **same guarded order path** — so every account-wide risk gate (leverage, net-exposure, concentration, drawdown, daily-loss, open-positions) applies per leg automatically.
- **Scaling:** `--scale-mode equity` (default) sizes each position to `your_equity / leader_equity * scale`; `--scale-mode fixed` uses `leader_size * scale`. `--scale <x>` and `--coins BTC,ETH` (restrict) shape it.
- **Stateless / your loop owns the state.** Pass the coins you're currently mirroring via `--mirrored BTC,ETH` (from your `STATE.md`); copy returns `data.mirrored_now` (the leader's current coins) — persist it and pass it back next tick. This is what makes the leader's **exits** mirror: a coin in `--mirrored` that the leader has since closed gets a `close` leg; a coin in neither the leader's book nor `--mirrored` is **ignored** (your own positions are never touched).
- **Loop:** each tick → `copy <leader> --mirrored <saved> --execute --yes` → save `data.mirrored_now`. On **exit 42** (a leg's outcome is unknown), copy returns `data.unknown_cloids` and stops — feed those to `reconcile --cloid …` next tick, do **not** blind-resubmit.
- **Exit codes:** 10 bad leader / missing `--yes`; 20 a leg hit a risk gate (reported per-leg in `data.legs`, others still run); 42 outcome-unknown leg (reconcile next); 60 some legs rejected/deferred.
- **Honest caveats:** you are always *late* to the leader's trades; the leader's leverage liquidates you too; v1 is **perps + main dex only**; mirroring a short fund opens **shorts**. Preview with the default diff (or `--execute --dry-run`) before committing.

## Managing
- Modify a resting order: `deliverator modify --cloid <ID> --limit <PX> [--size <SZ>] --json`
- Modify many at once: `deliverator modify-batch --file modifies.json --json`
  (array of `{oid|cloid, size?, limit?}`; one signed action, atomic pre-flight).
- Cancel: `deliverator cancel --cloid <ID> --json` · by lists: `--oids 1,2,3` / `--cloids 0x..,0x..`
  (one action) · all: `deliverator cancel --all [--coin <COIN>] --json`. A cancel of an
  already-gone order is reported in `data.failed[]`, not a hard error.
- Close: `deliverator close <COIN> --market --json` flattens a **perp** position, or
  **sells a spot holding** when `<COIN>` is a spot pair (sized from your balance;
  `--size` for a partial exit). Closes bypass the max caps / allowlist (you can always exit).
- Leverage / margin: `deliverator leverage <COIN> <X> --json` · `deliverator margin <COIN> <USD> --add --json`

> Builder-fee coverage: the configured builder fee attaches to `buy`/`sell`/`order`/`close` and bracket legs. Three cases earn **no** builder fee (each emits a warning): Hyperliquid's `twapOrder` and `modify` actions have no builder field (TWAP and modify orders carry no fee; a modify turns a fee-bearing order into a non-fee one); and a **spot BUY** pays its taker fee in the base token, so HL applies no (quote-denominated) builder fee — **spot sells and all perps do earn it.** To monetize spot flow, prefer the sell side or perps.

## Exit codes
```
0  ok                 proceed
10 bad args           fix inputs
11 precision (strict) re-round and retry
20 risk-blocked       respect the cap — do NOT retry as-is
21 halted             stop trading
30 auth               operator must fix the key
40 network            retry with backoff
41 rate-limited       wait error.retry_after_ms, then retry
42 timeout (UNKNOWN)  DO NOT blindly resubmit — run the retry rule below
50 exchange-rejected  read error.message, adjust
60 partial fill       inspect fills, decide
70 clock skew         operator must fix the clock
```

## Retry rule (critical — this is how naive agents double-fill)
On **exit 42** (timeout, outcome unknown):
1. Run `deliverator order status --cloid <ID> --json`.
2. If the order exists (resting or filled) → it landed; **do not resend.**
3. If it is absent → resubmit with the **same** `--cloid`.

The exchange can take ~1–2s to index a new order by cloid. If status is "absent"
within a couple seconds of placing, **wait briefly and re-check** (or query by
`--oid` if you have it) before resubmitting — a too-eager recheck can report an
order that *did* land as absent and trick you into a double-fill.

## Reconcile after a restart (run this FIRST when resuming)
Before placing anything after a (re)start or a missed tick, adopt on-chain reality:
`deliverator reconcile --json` — diffs the local audit trail against live
positions + open orders (all dexes). It reports `orphan_orders` (live orders with
no audit record of placing them), `closed_since` (audit-resting orders no longer
live — informational, pull `fills` to update PnL), and your live `open_orders` /
`positions`. **Exit 60** means a divergence exists — inspect before trading;
**exit 0** means local and live agree.

Pass any in-flight cloids you're unsure about (e.g. an order that hit **exit 42**)
as `--cloid 0x..,0x..` and reconcile resolves each against live state with a
recommended `action`: `resting`/`filled` → **adopt** (don't resend), `absent` →
**resubmit** the same cloid, terminal/`error` → **inspect**. This is the batch
form of the exit-42 retry rule above — use it to clear all suspects in one read.

## Rate limits
Actions are rate-limited **per address** (~1 request per 1 USDC traded; 10,000 initial buffer;
when exhausted, 1 request / 10s). Info reads are cheap; actions are the scarce resource.
- Check `deliverator limits --json` before a burst.
- Batch cancels (`cancel --all`). Prefer `stream` over polling.
- On exit 41, honor `error.retry_after_ms` (≥10000 when address-throttled).

## Streams (NDJSON; one object per line)
`deliverator stream <book|bbo|trades|candles|fundings|active-asset|mids|fills|orders|webdata2|twap-fills|events>`
emits one envelope per line and runs until interrupted. The socket auto-reconnects
and resubscribes. There are **no sequence numbers** on the Hyperliquid feed, so the
consumer is responsible for dedup and gap-recovery:

- **Dedup keys.** A reconnect (or overlap with a cold-start snapshot) can redeliver
  events. Dedup **fills by `tid`** and **order updates by `oid` + `status`** (the same
  oid legitimately reappears as it moves open→filled/canceled — the pair is the key).
- **Reconnect marker = possible gap.** On every drop the stream injects a control
  line `{"channel":"reconnect","event":{"reason":…,"backoff_ms":…,"resync":true,"hint":…}}`.
  `resync:true` means a gap may have occurred while disconnected: re-snapshot
  (`portfolio` / `orders` / `fills --since <last_ts>`) and reconcile against what you've
  already seen using the dedup keys above. The socket has no replay.
- **Cold start has no backfill.** A freshly-subscribed `fills`/`orders` stream only
  delivers events from the subscribe moment forward. To avoid a gap at startup:
  snapshot first and record a watermark (`fills` newest `time`, live `orders`), THEN
  subscribe, and drop any streamed event already in the snapshot (by `tid` / `oid`+`status`).

## Real-time failsafe (reactive counterpart to the DMS)
`deliverator watch --metric liq_distance_pct --below <pct> --action <alert|dms|panic> [--coin <C>] [--cooldown 60s] [--dms-secs N]`
consumes the user-state stream and evaluates the metric on every frame, firing the
action the moment it breaches — catching a mid-interval liquidation approach a
periodic tick can't. Emits NDJSON (one envelope per evaluation, one `watch.trigger`
on breach) and runs until interrupted. `--cooldown` debounces a flapping metric;
`--dry-run` reports what it *would* do without signing. `--action alert` requires
`alerting.webhook_url`. Unlike the DMS (which only cancels resting orders on a
heartbeat lapse), `--action panic` actually flattens.

> It is best-effort, not a guarantee: like any stream consumer it is blind during a
> reconnect (see **Streams** above), so a breach contained entirely within a
> disconnect window is caught only on the next frame. Keep a conservative DMS armed
> as the backstop — the two are complementary (reactive metric breach + passive
> heartbeat lapse), not redundant.

## Safety levers
- `deliverator dms set 60` arms a dead-man's switch; refresh with `deliverator dms heartbeat` (cron).
  If the heartbeat lapses, the exchange auto-cancels your resting orders.
- `deliverator watch …` (above) is the reactive failsafe — it *acts* on a live metric breach, not on a heartbeat lapse.
- `deliverator halt on` rejects all new orders instantly (exit 21). `deliverator halt off` resumes.
- `deliverator panic --yes` cancels all orders, cancels running TWAPs, and flattens all positions (every dex). It then re-verifies: `complete:false` (and a non-zero exit) + a `degraded:[dex]` list mean the teardown could NOT be confirmed flat — re-run / inspect that dex.

## Account-wide risk gates (core-enforced, exit 20)
Beyond the per-order caps, the operator may set **portfolio-level** gates that bound the whole book — you cannot bypass them by switching command. They evaluate the *resulting* book (current positions + this order); reduce-only/close orders are exempt. A breach is exit 20 with a concrete message — **respect it, don't retry as-is** (reduce size or stop):
- `risk.max_account_leverage` — resulting gross notional / equity.
- `risk.max_net_exposure_usd` — resulting |long − short|.
- `risk.max_concentration_pct_per_coin` — one coin as a % of equity.
- `risk.max_drawdown_pct` — pauses new exposure once equity falls that % below its high-water (persists until you recover or the operator resets it).
- `risk.max_daily_loss_usd` / `_pct` — pauses new exposure after that much loss since the UTC-midnight anchor; resets at UTC midnight.
- `risk.max_open_positions` — rejects opening a NEW coin once you already hold that many positions (adding to an existing one is fine).

These are the account's survival floor — when one trips, the loop should stop opening risk, not size around it.

**Reduce-only flip (always on):** a reduce-only order whose size exceeds the current open position is rejected (`reduce_only_flip`, exit 20) — it could only fill by crossing zero into opposite exposure. Size a reduce-only/close at most to the open position; drop reduce-only to open the opposite side deliberately.

## Hard rules
- **Never attempt withdrawals or transfers** — unsupported by design (non-custodial guarantee).
- **Respect exit 20 and 21** — those are safety limits, not transient errors. Never retry around them.
- Treat `--dry-run` output as authoritative for what *would* be sent before you commit.
