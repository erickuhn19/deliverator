package hl

// Coverage for the #91/S8 response-body cap: a malicious/buggy endpoint that
// streams an oversized body must fail closed, not OOM the process.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPostRejectsOversizedBody(t *testing.T) {
	// Server streams maxHTTPBodyBytes+1 bytes — one past the cap.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		chunk := strings.Repeat("a", 1<<20)
		for written := 0; written <= maxHTTPBodyBytes; written += len(chunk) {
			_, _ = io.WriteString(w, chunk)
		}
	}))
	defer srv.Close()

	tr := newTransport(srv.URL, ClientOptHTTPClient(srv.Client()))
	if _, err := tr.post(context.Background(), "/info", map[string]any{}); err == nil {
		t.Fatal("post must reject a body that exceeds the cap (fail closed)")
	} else if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("error should name the size limit, got: %v", err)
	}
}

func TestPostAcceptsNormalBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	tr := newTransport(srv.URL, ClientOptHTTPClient(srv.Client()))
	b, err := tr.post(context.Background(), "/info", map[string]any{})
	if err != nil {
		t.Fatalf("a normal body must pass: %v", err)
	}
	if string(b) != `{"ok":true}` {
		t.Fatalf("body = %q", b)
	}
}
