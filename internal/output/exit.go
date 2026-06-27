// Package output owns the agent contract surface: the schema-v1 JSON envelope,
// the exit-code matrix, and the machine-actionable error catalog. It is the
// single place that decides what an OpenClaw agent sees and how it branches.
package output

import "fmt"

// Exit codes. The agent branches on these via $? — never on stdout text.
// See spec §5.2. Keep this matrix stable; it is part of the contract.
const (
	ExitOK         = 0  // success — proceed
	ExitUnknown    = 1  // unknown — log, surface to operator
	ExitValidation = 10 // bad args / unknown coin — fix inputs
	ExitPrecision  = 11 // precision rejected (--strict only) — re-round and retry
	ExitRisk       = 20 // risk-rejected (cap/allowlist/limit-only/leverage) — respect the cap
	ExitHalt       = 21 // global halt active — stop trading
	ExitAuth       = 30 // auth/key error — operator must fix key
	ExitNetwork    = 40 // network/unreachable — retry w/ backoff
	ExitRateLimit  = 41 // rate-limited (address or IP) — back off (retry_after_ms)
	ExitTimeout    = 42 // outcome unknown — run retry protocol §5.4, do NOT blind-resubmit
	ExitExchange   = 50 // exchange-rejected (margin/oracle/etc.) — read message, adjust
	ExitPartial    = 60 // partial fill — inspect fills, decide
	ExitClock      = 70 // clock skew outside nonce window — operator must fix clock
)

// CmdError signals main() to terminate with a specific exit code. By the time a
// RunE returns one, the failure envelope has already been written to stdout, so
// main must NOT print it again — only os.Exit(Code).
type CmdError struct {
	Code int
}

func (e *CmdError) Error() string { return fmt.Sprintf("exit %d", e.Code) }

// ExitWith returns a *CmdError carrying code without emitting anything. Use it
// for success-shaped outcomes that still need a non-zero branch (e.g. a partial
// fill → exit 60): emit the success envelope first, then return ExitWith(60).
func ExitWith(code int) error {
	if code == ExitOK {
		return nil
	}
	return &CmdError{Code: code}
}
