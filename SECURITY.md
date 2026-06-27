# Security Policy

Deliverator signs real financial transactions. We take security reports
seriously and appreciate responsible disclosure.

## Reporting a vulnerability

**Do not open a public issue for a security vulnerability.**

Email **eric@deliverator.net** with:

- a description of the issue and its impact,
- steps to reproduce (a failing test or a minimal command sequence is ideal),
- the commit/version affected (`deliverator version`).

You'll get an acknowledgement within 72 hours and a fix or mitigation plan as
soon as the issue is confirmed. Please give us a reasonable window to ship a
fix before any public disclosure.

## The non-custodial guarantee

Deliverator's core safety property is structural, not a feature that can be
toggled: **it signs only with a Hyperliquid agent/API wallet that physically
cannot withdraw, transfer, or move funds.** Deposits, withdrawals, transfers,
and master-only actions (`approveAgent`/`approveBuilderFee`) are out of scope by
design — they require the master key, which Deliverator never holds. The
worst-case outcome of a bug or a compromised agent is *bad trades on an account
you control*, never drained funds.

That bounds the threat model but does not eliminate it. The following are very
much in scope.

## In scope

- **EIP-712 / msgpack signing correctness** — any way to make the signer produce
  a signature for an action other than the one presented, or to deviate from the
  golden `(r,s,v)` vectors.
- **Risk-cap bypass** — any way to place an order that the configured caps
  (`risk.*`, `automation.*`, `halt`, DMS) should have rejected. Enforcement lives
  in `internal/core`; surface-layer escapes are bugs.
- **Key handling** — exposure of the agent key (keychain, `DELIVERATOR_AGENT_KEY`,
  logs, the command log, audit trail, error messages, argv).
- **State integrity** — nonce reuse, audit-log or command-log tampering,
  TOCTOU/symlink races on the state files.
- **Idempotency** — any path that can cause an unintended double-fill despite the
  cloid retry protocol.

## Out of scope

- The deliberate non-custodial exclusions above (no withdraw/transfer is the
  point, not a missing feature).
- Trades that the configured caps correctly permitted — a loss from an
  authorized, in-policy trade is market risk, not a vulnerability.
- Hyperliquid API/exchange-side behavior outside this codebase.

## Supported versions

The latest `main` (and the latest tagged release once releases exist) receive
security fixes. There is no support window for older commits — update to head.
