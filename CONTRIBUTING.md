# Contributing to Deliverator

Thanks for your interest. Deliverator is the non-custodial execution layer an
autonomous agent drives to trade a real Hyperliquid account, so correctness and
safety are not negotiable. This guide describes the invariants a change must
hold and how to verify them.

## Build & test

```sh
go build -o deliverator .          # single binary, no codegen
go test ./...                      # signing parity, risk gauntlet, engine, precision, envelope, nonce
go test -race ./...                # the suite is race-clean; keep it that way
go vet ./...
gofmt -l .                         # must print nothing
scripts/check-coverage.sh          # per-package coverage floors (below)
```

Requires Go 1.25+. A PR is ready when all of the above pass.

## Architecture invariants

These are the rules that keep the agent contract trustworthy. A change that
breaks one needs an extraordinarily good reason.

- **`cmd/` is a thin cobra adapter — nothing else.** It parses flags, calls
  `internal/core`, and emits an envelope. **All correctness and safety live in
  `internal/core`**, never in the surface. A risk check in `cmd` is a bug,
  because switching how the binary is invoked must not bypass it.
- **One JSON envelope per command (schema v1).** Every command — success *and*
  failure — emits exactly one `internal/output` envelope. Prices and sizes are
  always strings. The exit-code matrix (see the README) is a public API: agents
  branch on `$?`, so changing a code is a breaking change.
- **Signing is golden-vector-locked.** `internal/hl` ports EIP-712 action signing
  from the official Hyperliquid Python SDK and pins `(r,s,v)` golden vectors per
  action family. If you touch wire encoding, msgpack, or signing, the vectors
  must still pass — and if a vector *legitimately* must change, explain in the PR
  exactly why the old bytes were wrong. Never edit a vector to make a test green.
- **Idempotency via cloid + retry protocol.** Writes carry a `cloid`; the
  exchange does not dedup it, so the check-status-before-resubmit protocol is what
  prevents double-fills. Don't add a write path that resubmits blindly.
- **Non-custodial scope is fixed.** Do not add deposit/withdraw/transfer or
  master-only signing. That exclusion is the product's core guarantee.

## Coverage floors

`scripts/check-coverage.sh` fails the build if a package drops below its floor:

| Package | Floor |
|---|---|
| `cmd` | 65% |
| `internal/core`, `internal/hl`, `internal/state` | 70% |
| `internal/config` | 75% |
| `internal/output`, `internal/wallet` | 85% |

Add tests rather than lowering a floor. If a floor genuinely must move, change it
in the script in the same PR and say why. Tests should be non-vacuous — assert on
the value that matters, not just that the call returned.

## Secrets — never

- The agent key lives in the OS keychain (or `DELIVERATOR_AGENT_KEY` for headless
  hosts). It must never appear on the command line, in the audit log, in the
  command log, in test fixtures, or in a commit. argv is logged for oversight —
  keep keys out of argv.
- No real addresses tied to live funds in fixtures; use the test vectors.

## Live verification

Write paths are proven against **mainnet with real (small) orders** — that's how
we caught wire bugs that unit tests can't. The CLI is **testnet-first**
(`network = "testnet"` by default); keep it that way, and never change the
default network or a risk default casually.

## Pull requests

- Keep PRs small and focused; one concern each.
- Match the surrounding style — comment density, naming, idiom.
- Describe the safety impact: does this touch signing, risk caps, key handling,
  or the envelope/exit-code contract? If so, call it out explicitly.
- All checks green (`test -race`, `vet`, `gofmt`, coverage floors) before review.

## Reporting security issues

Do **not** use a public issue or PR for a vulnerability — see
[SECURITY.md](./SECURITY.md).
