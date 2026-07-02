package hl

// Tests for the typed transport-failure classification (review P0-B): post
// wraps every transport-level failure in TransportError, recording whether the
// request provably never left this host (Sent=false — dial/DNS/TLS handshake)
// or may have reached the exchange (Sent=true — everything else). The engine's
// write mapper branches on Sent, so misclassifying "possibly sent" as "never
// sent" would sanction a blind resubmit → double-fill.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A connection refused (dial failure) is provably never sent.
func TestPostConnectionRefusedNeverSent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := srv.URL
	srv.Close()
	tr := newTransport(dead)
	_, err := tr.post(context.Background(), "/exchange", map[string]any{})
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("want *TransportError, got %T %v", err, err)
	}
	if te.Sent {
		t.Fatalf("a dial failure must be Sent=false: %v", err)
	}
}

// A connection reset AFTER the request was fully read may have delivered it:
// Sent must be true (outcome unknown for a write), never false.
func TestPostResetAfterSendPossiblySent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetLinger(0) // RST, not FIN
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	tr := newTransport(srv.URL)
	_, err := tr.post(context.Background(), "/exchange", map[string]any{})
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("want *TransportError, got %T %v", err, err)
	}
	if !te.Sent {
		t.Fatalf("a post-send reset must be Sent=true: %v", err)
	}
}

// A 200 whose body doesn't parse (truncated mid-stream) means the action
// reached the exchange: executeAction must surface TransportError{Sent:true}.
func TestExecuteActionTruncated200PossiblySent(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","respo` // truncated envelope
	})
	_, err := ex.Order(ctx, limitOrder(), nil, 0)
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("want *TransportError, got %T %v", err, err)
	}
	if !te.Sent {
		t.Fatalf("an unparseable 200 must be Sent=true: %v", err)
	}
}

// A 200 modify response whose envelope doesn't parse (truncated mid-stream)
// reached the exchange: ModifyOrder parses its own envelope (it bypasses
// executeAction), so it must surface TransportError{Sent:true} itself — the
// cancel+replace may have landed (and even filled), never a definitive reject.
func TestModifyTruncated200PossiblySent(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"ok","respo` // truncated envelope
	})
	oid := int64(42)
	_, err := ex.ModifyOrder(ctx, ModifyOrderRequest{Oid: &oid, Order: limitOrder()})
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("want *TransportError, got %T %v", err, err)
	}
	if !te.Sent {
		t.Fatalf("an unparseable 200 modify response must be Sent=true: %v", err)
	}
}

// A non-ok modify envelope is the exchange definitively speaking — that must
// STAY a plain rejection, not be reclassified as a transport failure.
func TestModifyEnvelopeErrorStaysRejection(t *testing.T) {
	ex, ctx := testExchange(t, noInfo, func(string, map[string]any) (int, string) {
		return 200, `{"status":"err","response":"Order was never placed"}`
	})
	oid := int64(42)
	_, err := ex.ModifyOrder(ctx, ModifyOrderRequest{Oid: &oid, Order: limitOrder()})
	if err == nil {
		t.Fatal("expected envelope error")
	}
	var te *TransportError
	if errors.As(err, &te) {
		t.Fatalf("a parsed err envelope is a definitive rejection, not a transport failure: %v", err)
	}
}

// A >=400 response with a non-JSON body is a gateway/edge page (Cloudflare/
// nginx HTML), not the exchange API speaking: the request reached that edge and
// may have been forwarded upstream, so Sent must be true — a write's outcome is
// UNKNOWN, never a definitive rejection.
func TestPostNonJSONErrorBodyPossiblySent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`<html><body>502 Bad Gateway — cloudflare</body></html>`))
	}))
	t.Cleanup(srv.Close)
	tr := newTransport(srv.URL)
	_, err := tr.post(context.Background(), "/exchange", map[string]any{})
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("want *TransportError, got %T %v", err, err)
	}
	if !te.Sent {
		t.Fatalf("a gateway error page must be Sent=true (possibly forwarded): %v", err)
	}
}

// A non-2xx with a JSON body stays a typed APIError (the exchange/edge spoke
// definitively) — it must NOT be reclassified as a transport failure.
func TestPost429StaysAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"code":429,"msg":"rate limited"}`))
	}))
	t.Cleanup(srv.Close)
	tr := newTransport(srv.URL)
	_, err := tr.post(context.Background(), "/exchange", map[string]any{})
	var apiErr APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 429 {
		t.Fatalf("want APIError{Status:429}, got %T %v", err, err)
	}
	var te *TransportError
	if errors.As(err, &te) {
		t.Fatalf("an HTTP response is not a transport failure: %v", err)
	}
}

// requestNeverSent: only dial/DNS(/TLS-handshake) failures qualify; read-phase
// and timeout errors must stay "possibly sent".
func TestRequestNeverSentClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"dns", &net.DNSError{Err: "no such host", Name: "api.example"}, true},
		{"dial op", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, true},
		{"read op", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}, false},
		{"deadline", context.DeadlineExceeded, false},
		{"plain", errors.New("unexpected EOF"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestNeverSent(tc.err); got != tc.want {
				t.Fatalf("requestNeverSent(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
