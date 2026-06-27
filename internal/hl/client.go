// Package hl is deliverator's own Hyperliquid API client. It talks directly to
// the Hyperliquid HTTP API (POST /info and signed POST /exchange) with zero
// third-party SDK dependency, so the trust boundary that signs money-moving
// actions is code we own and test.
//
// The exchange protocol (msgpack action hashing + EIP-712 phantom-agent
// signing) is specified by the official Hyperliquid Python SDK. This package
// ports that scheme; the wire format and field ordering are cross-checked
// byte-for-byte against the reference Go implementation (sonirico/go-hyperliquid,
// MIT) via the differential tests under the `difftest` build tag.
//
// Scope: only the L1 (agent-key-signable) actions deliverator needs are
// implemented. User-signed / master-key actions (withdraw, usdSend, spotSend,
// approveAgent, …) are deliberately NOT implemented — the agent key cannot sign
// them, which is deliverator's non-custodial guarantee.
package hl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// API endpoints. The exact string identity of MainnetAPIURL matters: signing
// keys the phantom-agent source ("a" mainnet / "b" testnet) off whether the
// client's base URL equals MainnetAPIURL.
const (
	MainnetAPIURL = "https://api.hyperliquid.xyz"
	TestnetAPIURL = "https://api.hyperliquid-testnet.xyz"

	// DefaultSlippage is the fallback max slippage for market orders (5%).
	DefaultSlippage = 0.05

	httpErrorStatusCode = 400

	// maxHTTPBodyBytes caps a single response body. A malicious/buggy endpoint
	// (or MITM, since we don't pin) could otherwise stream an unbounded body and
	// OOM the process; we fail closed on overflow (audit #91 / S8). 16 MiB is far
	// above any real /info or /exchange response (the leaderboard uses its own
	// larger cap for the bulk feed).
	maxHTTPBodyBytes = 16 << 20
)

// APIError is a non-2xx error body from the exchange.
type APIError struct {
	Code    int    `json:"code"`
	Message string `json:"msg"`
	Data    any    `json:"data,omitempty"`
	// Status is the HTTP status code (e.g. 429). Set from the response, never the
	// body (json:"-"), so a caller can distinguish a per-IP rate-limit (429) from a
	// generic failure even when the body's own "code" is unrelated.
	Status int `json:"-"`
}

func (e APIError) Error() string { return fmt.Sprintf("API error %d: %s", e.Code, e.Message) }

// ---- options (mirror the SDK's functional-option surface so the consumer's
// constructor calls change only their import path) ----

type clientOpt func(*httpTransport)

// ClientOpt is applied to the underlying HTTP transport.
type ClientOpt = clientOpt

// InfoOpt is applied to an Info client.
type InfoOpt func(*Info)

// ExchangeOpt is applied to an Exchange client.
type ExchangeOpt func(*Exchange)

// ClientOptHTTPClient injects a custom *http.Client (timeouts, transport).
func ClientOptHTTPClient(httpClient *http.Client) ClientOpt {
	return func(t *httpTransport) { t.httpClient = httpClient }
}

// InfoOptClientOptions forwards ClientOpts to an Info client's transport.
func InfoOptClientOptions(opts ...ClientOpt) InfoOpt {
	return func(i *Info) { i.clientOpts = append(i.clientOpts, opts...) }
}

// ExchangeOptClientOptions forwards ClientOpts to an Exchange client's transport.
func ExchangeOptClientOptions(opts ...ClientOpt) ExchangeOpt {
	return func(e *Exchange) { e.clientOpts = append(e.clientOpts, opts...) }
}

// httpTransport is the thin POST layer shared by Info and Exchange.
type httpTransport struct {
	baseURL    string
	httpClient *http.Client
}

func newTransport(baseURL string, opts ...ClientOpt) *httpTransport {
	if baseURL == "" {
		baseURL = MainnetAPIURL
	}
	t := &httpTransport{baseURL: baseURL, httpClient: &http.Client{}}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// post sends a JSON body to path (e.g. "/info" or "/exchange") and returns the
// raw response body. A >=400 status is decoded into APIError when possible.
func (t *httpTransport) post(ctx context.Context, path string, payload any) ([]byte, error) {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+path, bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read one byte past the cap so we can DETECT (not silently truncate) an
	// oversized body and fail closed (audit #91 / S8).
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	if len(body) > maxHTTPBodyBytes {
		return nil, fmt.Errorf("response body exceeded %d-byte limit", maxHTTPBodyBytes)
	}

	if resp.StatusCode >= httpErrorStatusCode {
		if !json.Valid(body) {
			return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
		}
		var apiErr APIError
		if err := json.Unmarshal(body, &apiErr); err != nil {
			return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
		}
		apiErr.Status = resp.StatusCode
		return nil, apiErr
	}
	return body, nil
}

// ---- exchange response envelope ----

// APIResponse is the {status, response:{type, data}} envelope returned by
// /exchange. On status != "ok", response is a plain string error captured in Err.
type APIResponse[T any] struct {
	Status string
	Data   T
	Type   string
	Err    string
	Ok     bool
}

func (r *APIResponse[T]) UnmarshalJSON(data []byte) error {
	var raw struct {
		Status   string          `json:"status"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("failed to parse response envelope: %w", err)
	}
	r.Status = raw.Status
	r.Ok = raw.Status == "ok"
	if !r.Ok {
		// "response" is usually a plain string error message; ignore if not.
		_ = json.Unmarshal(raw.Response, &r.Err)
		return nil
	}
	var inner struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw.Response, &inner); err != nil {
		return fmt.Errorf("failed to parse response.data: %w", err)
	}
	r.Type = inner.Type
	if inner.Data == nil {
		return fmt.Errorf("missing response.data field in successful response")
	}
	if err := json.Unmarshal(inner.Data, &r.Data); err != nil {
		return fmt.Errorf("failed to unmarshal response data: %w", err)
	}
	return nil
}

// ---- loosely-typed JSON helpers (portfolio time series, cancel statuses) ----

// MixedValue is a deferred JSON value that can be decoded on demand.
type MixedValue json.RawMessage

func (mv *MixedValue) UnmarshalJSON(data []byte) error { *mv = data; return nil }
func (mv MixedValue) MarshalJSON() ([]byte, error)     { return mv, nil }

// String decodes the value as a JSON string.
func (mv *MixedValue) String() (string, bool) {
	var s string
	if err := json.Unmarshal(*mv, &s); err != nil {
		return "", false
	}
	return s, true
}

// Object decodes the value as a JSON object.
func (mv *MixedValue) Object() (map[string]any, bool) {
	var obj map[string]any
	if err := json.Unmarshal(*mv, &obj); err != nil {
		return nil, false
	}
	return obj, true
}

// Parse decodes the value into v.
func (mv *MixedValue) Parse(v any) error { return json.Unmarshal(*mv, v) }

// MixedArray is a heterogeneous JSON array (e.g. cancel statuses, [ts, value]).
type MixedArray []MixedValue

// FirstError returns the first non-"success" status as an error, or nil.
func (ma MixedArray) FirstError() error {
	for _, mv := range ma {
		if s, ok := mv.String(); ok {
			if s == "success" {
				continue
			}
			return errors.New(s)
		}
		if obj, ok := mv.Object(); ok {
			if v, ok := obj["error"]; ok {
				if msg, ok := v.(string); ok && msg != "" {
					return errors.New(msg)
				}
				b, _ := json.Marshal(v)
				return errors.New(string(b))
			}
		}
		return errors.New("cancel failed")
	}
	return nil
}
