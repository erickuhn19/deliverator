#!/usr/bin/env bash
# secret-scan.sh — reproducible pre-public secret gate. Fails if any secret is
# found in the working tree OR full git history. Run before every public push.
# Requires: gitleaks, trufflehog. Portable to bash 3.2 (macOS default).
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
fail=0

echo "==> gitleaks (full history, .gitleaks.toml allowlist)"
if command -v gitleaks >/dev/null 2>&1; then
  gitleaks detect --source . --log-opts="--all" --redact -v || fail=1
else
  echo "  gitleaks not installed (brew install gitleaks)"; fail=1
fi

echo "==> trufflehog (verified secrets only, full history)"
if command -v trufflehog >/dev/null 2>&1; then
  trufflehog git "file://$(pwd)" --only-verified --fail 2>&1 | tail -20 || fail=1
else
  echo "  trufflehog not installed (brew install trufflehog)"; fail=1
fi

echo "==> raw private keys in NON-test Go (must be empty)"
hits=$(grep -rIlE '0x[0-9a-fA-F]{64}' --include='*.go' . | grep -v '_test\.go' || true)
if [ -n "$hits" ]; then
  echo "  FAIL: 64-hex literal in non-test Go:"; echo "$hits"; fail=1
else
  echo "  ok (none)"
fi

if [ "$fail" -ne 0 ]; then
  echo "SECRET SCAN FAILED — DO NOT push to a public repo until clean" >&2
  exit 1
fi
echo "SECRET SCAN CLEAN"
