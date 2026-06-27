package output

// Category groups every error so the agent (and the exit code) can react
// deterministically. See spec §5.3. Messages are written to be acted on by a
// model — concrete numbers, no opaque codes.
type Category string

const (
	CatValidation Category = "validation"
	CatPrecision  Category = "precision"
	CatRisk       Category = "risk"
	CatHalt       Category = "halt"
	CatAuth       Category = "auth"
	CatNetwork    Category = "network"
	CatRateLimit  Category = "rate_limit"
	CatTimeout    Category = "timeout"
	CatExchange   Category = "exchange"
	CatPartial    Category = "partial"
	CatClock      Category = "clock"
	CatUnknown    Category = "unknown"
)

// Error is the machine-actionable error payload embedded in a failure envelope.
type Error struct {
	Code         string   `json:"code"`
	Category     Category `json:"category"`
	Message      string   `json:"message"`
	Retryable    bool     `json:"retryable"`
	RetryAfterMs *int64   `json:"retry_after_ms"`
	Hint         string   `json:"hint,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// ExitCode maps the error's category to the process exit code (§5.2).
func (e *Error) ExitCode() int {
	switch e.Category {
	case CatValidation:
		return ExitValidation
	case CatPrecision:
		return ExitPrecision
	case CatRisk:
		return ExitRisk
	case CatHalt:
		return ExitHalt
	case CatAuth:
		return ExitAuth
	case CatNetwork:
		return ExitNetwork
	case CatRateLimit:
		return ExitRateLimit
	case CatTimeout:
		return ExitTimeout
	case CatExchange:
		return ExitExchange
	case CatPartial:
		return ExitPartial
	case CatClock:
		return ExitClock
	default:
		return ExitUnknown
	}
}

// NewError builds an error in the given category.
func NewError(category Category, code, message string) *Error {
	return &Error{Code: code, Category: category, Message: message}
}

// WithHint attaches a concrete, model-actionable hint ("round px to 64000.1").
func (e *Error) WithHint(hint string) *Error { e.Hint = hint; return e }

// WithRetryAfter marks the error retryable after the given delay (ms). Used for
// rate limits (≥10000 when address-throttled) and backoff-able network errors.
func (e *Error) WithRetryAfter(ms int64) *Error {
	e.Retryable = true
	e.RetryAfterMs = &ms
	return e
}

// Retry marks the error retryable without a fixed delay (caller backs off).
func (e *Error) Retry() *Error { e.Retryable = true; return e }

// Per-category constructors — terse call sites at the command layer.
func Validation(code, msg string) *Error { return NewError(CatValidation, code, msg) }
func Precision(code, msg string) *Error  { return NewError(CatPrecision, code, msg) }
func Risk(code, msg string) *Error       { return NewError(CatRisk, code, msg) }
func Halt(code, msg string) *Error       { return NewError(CatHalt, code, msg) }
func Auth(code, msg string) *Error       { return NewError(CatAuth, code, msg) }
func Network(code, msg string) *Error    { return NewError(CatNetwork, code, msg) }
func RateLimit(code, msg string) *Error  { return NewError(CatRateLimit, code, msg) }
func Timeout(code, msg string) *Error    { return NewError(CatTimeout, code, msg) }
func Exchange(code, msg string) *Error   { return NewError(CatExchange, code, msg) }
func Partial(code, msg string) *Error    { return NewError(CatPartial, code, msg) }
func Clock(code, msg string) *Error      { return NewError(CatClock, code, msg) }
func Unknown(code, msg string) *Error    { return NewError(CatUnknown, code, msg) }
