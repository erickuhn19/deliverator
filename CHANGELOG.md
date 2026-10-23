# Changelog

## Unreleased

### Papertrade builder codes

Session registration can name an approved Papertrade builder. The default is
still no builder: `papertrade.builder_address` is empty unless set, and open
and close do not attach a fee.

- `papertrade session register --builder` (or `papertrade.builder_address`)
  sets `RegisterSessionKey.feeAddress`. A non-zero address must already be
  approved and requires `--yes`.
- `papertrade builder` reads the live fee rate, approval, unclaimed fees, and
  application status.
- `papertrade builder apply` submits the builders-page application. It does
  not approve the builder.
- `papertrade builder claim` sends `claimBuilderFees()` from the builder
  wallet. Fees credit the exchange balance. Exit 42 carries `error.tx_hash`.

Protocol: [Builder codes](https://docs.papertrade.xyz/how/builder-codes/),
[contract addresses](https://docs.papertrade.xyz/dev/contract-addresses/).
Owner-only timelock actions (approve, remove, move fees after a lost key) are
not commands.
