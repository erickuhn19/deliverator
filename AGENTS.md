# AGENTS.md

Orientation for an **autonomous agent (or contributor)** driving Deliverator. The
authoritative, always-current contract is `deliverator tools` (the embedded
[`TOOLS.md`](./TOOLS.md)) and `deliverator schema`. This file is the short version.

## What you are driving

Deliverator is a non-custodial CLI between an LLM agent and a **real-money**
Hyperliquid account, and the same kind of harness for Papertrade isolated perps
on HyperEVM. You (the agent) decide *what* to trade; Deliverator enforces
that the *how* is correct, bounded, and auditable. It signs with an agent/API
wallet that **physically cannot withdraw** on either venue — the worst you can do
is place bad trades, never drain funds. Do not try to defeat that; it's the point.

## Non-negotiable rules

1. **Branch on the exit code, never on stdout text.** Every command emits one
   schema-v1 JSON envelope and a documented exit code (`$?`). Parse `data`/`error`;
   match on `error.code`. Text is for humans.
2. **Every write carries a `cloid`.** Hyperliquid does **not** dedup a repeated
   cloid — a blind resend places a *second* order. Idempotency is *your* job via the
   retry protocol below.
3. **Retry protocol (exit 42 = outcome unknown):** run
   `deliverator order status --cloid <id>` (or `--oid`). If the order exists, it
   landed — do **not** resend. If absent, wait briefly (HL takes ~1–2s to index) and
   re-check before resubmitting the **same** cloid. This is the #1 way naive agents
   double-fill. The failure envelope's `error.cloids` carries the cloid(s) — even
   auto-generated ones — and exit 42 covers any post-send transport failure, not
   just timeouts (pre-send failures are exit 40, safe to retry). Hyperliquid only:
   a Papertrade write has no cloid. Use the Papertrade retry rule below.
4. **Respect the risk caps (exit 20).** They are enforced in core; switching how you
   invoke the binary cannot bypass them. A cap rejection means stop, not retry.
5. **Stop on halt (exit 21).** A global halt or armed dead-man's switch is rejecting
   new orders — do not loop on it.
6. **Discover, don't assume.** Use `deliverator markets`, `schema`, and `version` at
   runtime to learn the tradable universe, the envelope shape, and capabilities.
   Schema `v1` is additive-only; detect breaking changes at runtime.
7. **Reconcile after a restart.** Run `deliverator reconcile` FIRST when resuming —
   it diffs your local audit log against live exchange state.

## The envelope

```json
{ "schema":"v1", "ok":true, "ts":1750000000000, "cmd":"buy",
  "data":{ "cloid":"0x..","status":"resting","oid":123,"coin":"BTC","size":"0.01" },
  "error":null, "warnings":["size rounded 0.0123456 -> 0.0123"],
  "meta":{ "network":"mainnet","account":"main" } }
```

Prices and sizes are **strings**. On failure: `ok=false`, `data=null`, and `error`
is `{code, category, message, retryable, retry_after_ms, hint}` — plus `cloids`
(the signed client order id(s)) on Hyperliquid write failures, and `intent_ids`
on Papertrade write failures. Both arrays are additive.

## Exit codes (the contract)

`0` ok · `10` validation · `11` precision (`--strict`) · `20` risk-rejected ·
`21` halted · `30` auth/key · `40` network · `41` rate-limited (`retry_after_ms`) ·
`42` timeout — run the retry protocol · `50` exchange-rejected · `60` partial fill ·
`70` clock skew. Full table + per-code agent actions: `deliverator tools`.

## Operating safely

- **Preview / dry-run first** for anything non-trivial: `--dry-run` validates,
  rounds, and attaches without signing or sending.
- **Builder fee:** Deliverator attaches a small builder fee only when the trader's
  master wallet has approved it; otherwise orders trade fee-free. You never sign that
  approval — it's master-only.
- **Risk caps may be changed** via `deliverator config set risk.*`, but the CLI will
  remind whoever runs it to keep the human in the loop. Don't silently widen a cap.
  `config set network` warns the same loud way — flipping the testnet/mainnet
  boundary retargets every subsequent command, so never do it mid-loop on your own.

## Papertrade

`deliverator papertrade` trades isolated perps on HyperEVM (chain 999). That is
real money at the Hyperliquid mid, not a simulator and not Hyperliquid testnet.
There is no order book and no cloid. `size` on the wire is margin. The agent key
signs opens, closes, and cancels, and cannot withdraw. Registering it is a
one-time master signature via `DELIVERATOR_MASTER_KEY`, which is not stored.
`serve` does not expose this venue. Default risk caps still apply, so 1000× is
rejected while `risk.max_leverage` is 10.

A Papertrade builder code is the `feeAddress` on that registration, not a flag
on open or close. The default is no builder. Set `papertrade.builder_address`
or pass `session register --builder` (a non-zero builder needs `--yes`, and the
address must already be approved). `papertrade builder` inspects the code.
`builder apply` only submits the off-chain application. `builder claim` sends
`claimBuilderFees()` from the builder wallet and does not withdraw.

On exit 42 (or a post-sign exit 40), `error.intent_ids` names the intent. Run
`deliverator papertrade intent status --intent <id>`. If it is absent, run
`deliverator papertrade resend --intent <id>` so the same signed body is posted
again. A new `papertrade open` is a new position.

## Contributing to this repo

See [CONTRIBUTING.md](./CONTRIBUTING.md): the surface (`cmd/`) is a thin adapter; all
correctness and safety live in `internal/core`; signing is golden-vector-locked
(`internal/hl/sign_test.go` — do not edit a vector to make a test pass). Keep the
envelope and exit-code matrix stable; schema is additive-only.
