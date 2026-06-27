package output

// Coverage for the agent-contract boundary the audit (#89) flagged: the per-category
// error constructors, retry-field propagation, the human render path, JSON-output
// safety (adversarial message content), and the clock-skew-adjusted timestamp.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestErrorConstructorCategories(t *testing.T) {
	cases := []struct {
		e   *Error
		cat Category
	}{
		{Validation("c", "m"), CatValidation},
		{Precision("c", "m"), CatPrecision},
		{Risk("c", "m"), CatRisk},
		{Halt("c", "m"), CatHalt},
		{Auth("c", "m"), CatAuth},
		{Network("c", "m"), CatNetwork},
		{RateLimit("c", "m"), CatRateLimit},
		{Timeout("c", "m"), CatTimeout},
		{Exchange("c", "m"), CatExchange},
		{Partial("c", "m"), CatPartial},
		{Clock("c", "m"), CatClock},
		{Unknown("c", "m"), CatUnknown},
	}
	for _, c := range cases {
		if c.e.Category != c.cat {
			t.Errorf("constructor produced category %q, want %q", c.e.Category, c.cat)
		}
		if c.e.Code != "c" || c.e.Message != "m" || c.e.Error() != "m" {
			t.Errorf("constructor set wrong fields: %+v", c.e)
		}
	}
}

func TestRetryFieldPropagation(t *testing.T) {
	e := RateLimit("rl", "slow down").WithRetryAfter(2000)
	if !e.Retryable || e.RetryAfterMs == nil || *e.RetryAfterMs != 2000 {
		t.Fatalf("WithRetryAfter should set retryable + retry_after_ms: %+v", e)
	}
	n := Network("net", "down").Retry()
	if !n.Retryable || n.RetryAfterMs != nil {
		t.Fatalf("Retry should be retryable with no retry_after_ms: %+v", n)
	}
	if Validation("v", "bad").Retryable {
		t.Error("a validation error is not retryable by default")
	}
}

func TestRenderHumanSuccessAndError(t *testing.T) {
	var buf bytes.Buffer
	Configure(false, &buf)
	defer Configure(true, nil)

	Emit(Response{Cmd: "buy", Data: map[string]any{"oid": 1}, Warnings: []string{"rounded"}})
	out := buf.String()
	if !strings.Contains(out, "✓ buy") || !strings.Contains(out, "⚠ rounded") || !strings.Contains(out, "oid") {
		t.Fatalf("human success render wrong:\n%s", out)
	}

	buf.Reset()
	Fail("order", RateLimit("rl", "slow").WithHint("back off").WithRetryAfter(2000), Meta{})
	out = buf.String()
	for _, want := range []string{"✗ order", "error [rate_limit/rl]: slow", "hint: back off", "retry after: 2000 ms"} {
		if !strings.Contains(out, want) {
			t.Errorf("human error render missing %q in:\n%s", want, out)
		}
	}
	// The error branch returns early — success Data must not be printed alongside it.
	if strings.Contains(out, "oid") {
		t.Error("error render must not print success data")
	}
}

func TestRenderJSONSafetyAdversarialMessage(t *testing.T) {
	var buf bytes.Buffer
	Configure(true, &buf)
	defer Configure(true, nil)

	// HTML, quotes, a newline, a tab, and a 0x-hex blob: must serialize to exactly
	// ONE NDJSON line that round-trips (a raw newline would split the agent's stream).
	msg := "line1\nline2 </script> \"q\"\tend 0x" + strings.Repeat("ab", 32)
	Fail("buy", Exchange("rej", msg), Meta{})
	line := buf.Bytes()
	if n := bytes.Count(line, []byte("\n")); n != 1 {
		t.Fatalf("output must be exactly one NDJSON line, got %d newlines", n)
	}
	var env struct {
		Error struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		t.Fatalf("envelope is not parseable JSON: %v\n%s", err, line)
	}
	if env.Error.Message != msg {
		t.Errorf("message not preserved round-trip:\n got:  %q\n want: %q", env.Error.Message, msg)
	}
	if !bytes.Contains(line, []byte("</script>")) {
		t.Error("HTML escaping must be OFF (keep `<` literal for the agent)")
	}
}

func TestSetClockSkewShiftsNow(t *testing.T) {
	SetClockSkew(3_600_000) // +1h
	defer SetClockSkew(0)
	if d := Now() - time.Now().UnixMilli(); d < 3_500_000 || d > 3_700_000 {
		t.Fatalf("Now() should reflect the +1h skew, delta=%d ms", d)
	}
}
