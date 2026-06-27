package core

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	hl "github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/output"
)

// mapNetwork must split a read-transport error into the right category so an
// agent reacts correctly: a 429 -> rate_limit (41, back off), a timeout ->
// timeout (42), an already-typed *output.Error passes through, and anything else
// stays a retryable network error (40).
func TestMapNetworkCategorizes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		cat  output.Category
		exit int
	}{
		{"429 via APIError status", hl.APIError{Status: 429, Code: 0, Message: "rate limited"}, output.CatRateLimit, output.ExitRateLimit},
		{"429 via non-JSON status string", fmt.Errorf("status 429: too many requests"), output.CatRateLimit, output.ExitRateLimit},
		{"context deadline", fmt.Errorf("fetch mids: %w", context.DeadlineExceeded), output.CatTimeout, output.ExitTimeout},
		{"timeout string", errors.New("Client.Timeout exceeded while awaiting headers"), output.CatTimeout, output.ExitTimeout},
		{"plain network", errors.New("connection refused"), output.CatNetwork, output.ExitNetwork},
		{"typed passthrough", output.Auth("no_address", "x"), output.CatAuth, output.ExitAuth},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertErr(t, mapNetwork("all_mids", tc.err), tc.cat, tc.exit)
		})
	}
}

// The 429 rate-limit must carry a retry_after_ms so the agent waits the right
// amount before retrying, not just "retryable".
func TestMapNetwork429HasRetryAfter(t *testing.T) {
	var oe *output.Error
	if !errors.As(mapNetwork("x", hl.APIError{Status: 429}), &oe) {
		t.Fatal("want *output.Error")
	}
	if !oe.Retryable || oe.RetryAfterMs == nil || *oe.RetryAfterMs <= 0 {
		t.Fatalf("429 must set a positive retry_after_ms: %+v", oe)
	}
}

// End-to-end: a read whose /info call returns HTTP 429 surfaces as exit 41
// (rate_limit), not exit 40 (network) — the whole point of the fix.
func TestReadRateLimitedSurfacesAsRateLimit(t *testing.T) {
	resp := func(path, typ string, body map[string]any) (int, string) {
		return 429, `{"code":429,"msg":"per-IP weight exceeded"}`
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	_, err := c.Mids(ctx)
	assertErr(t, err, output.CatRateLimit, output.ExitRateLimit)
}

// A non-429 HTTP failure on a read stays a generic retryable network error (40).
func TestReadServerErrorStaysNetwork(t *testing.T) {
	resp := func(path, typ string, body map[string]any) (int, string) {
		return 500, `internal server error`
	}
	c, ctx := newTestClient(t, config.Default(), Options{}, resp)
	_, err := c.Mids(ctx)
	assertErr(t, err, output.CatNetwork, output.ExitNetwork)
}

// RequireQueryAddr (the exported guard the `info @` expansion uses) returns the
// auth error (exit 30) when no master address is configured, and nil otherwise.
func TestRequireQueryAddr(t *testing.T) {
	c := newCfgClient(t, config.Default())
	if err := c.RequireQueryAddr(); err != nil {
		t.Fatalf("a configured query addr must pass: %v", err)
	}
	c.queryAddr = ""
	assertErr(t, c.RequireQueryAddr(), output.CatAuth, output.ExitAuth)
}
