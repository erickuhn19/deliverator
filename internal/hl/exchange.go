package hl

// Exchange is the signed write client (POST /exchange). It owns nonce
// coordination and the L1 signer. Only agent-key-signable L1 actions are
// implemented (no user-signed/master-key actions) — deliverator's non-custodial
// guarantee.

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"
)

// poster is the pipe a signed action travels down. httpTransport and
// wsTransport both satisfy it, and NOTHING above this line knows which is in
// use: the signing, nonce and envelope logic is transport-agnostic, which is
// what makes swapping the pipe safe.
type poster interface {
	post(ctx context.Context, path string, payload any) ([]byte, error)
	// base is the REST endpoint this transport ultimately talks to. Needed
	// because mainnet-vs-testnet decisions (builder-fee attachment) key off it,
	// and those must not change when the pipe does.
	base() string
}

type Exchange struct {
	transport    poster
	privateKey   *ecdsa.PrivateKey
	vault        string
	accountAddr  string
	info         *Info
	expiresAfter *int64
	lastNonce    atomic.Int64
	useWS        bool

	clientOpts []ClientOpt
}

// NewExchange builds a signing client. Signature mirrors the prior SDK so the
// consumer's constructor call changes only its import path. perpDexs is unused.
func NewExchange(
	ctx context.Context,
	privateKey *ecdsa.PrivateKey,
	baseURL string,
	meta *Meta,
	vaultAddr, accountAddr string,
	spotMeta *SpotMeta,
	perpDexs *MixedArray,
	opts ...ExchangeOpt,
) *Exchange {
	ex := &Exchange{privateKey: privateKey, vault: vaultAddr, accountAddr: accountAddr}
	for _, opt := range opts {
		opt(ex)
	}
	ht := newTransport(baseURL, ex.clientOpts...)
	ex.transport = ht
	if ex.useWS {
		// Signed actions go over the socket; everything else keeps the HTTP path.
		// Opt-in only — the default must stay HTTP so a socket problem can never
		// be something an operator gets by upgrading.
		ex.transport = newWSTransport(baseURL, ht)
	}
	// Forward the same client options (custom *http.Client / timeout) to the
	// inner read client — the Exchange reads through it (SlippagePrice, MarketClose).
	ex.info = NewInfo(ctx, baseURL, true, meta, spotMeta, perpDexs, InfoOptClientOptions(ex.clientOpts...))
	return ex
}

// Info returns the read client backing this exchange.
func (e *Exchange) Info() *Info { return e.info }

func (e *Exchange) isMainnet() bool { return e.transport.base() == MainnetAPIURL }

// nextNonce returns a strictly-increasing millisecond nonce (CAS-guarded so
// concurrent callers never collide).
func (e *Exchange) nextNonce() int64 {
	for {
		last := e.lastNonce.Load()
		candidate := time.Now().UnixMilli()
		if candidate <= last {
			candidate = last + 1
		}
		if e.lastNonce.CompareAndSwap(last, candidate) {
			return candidate
		}
	}
}

// SetLastNonce floors the nonce generator at n (e.g. a persisted high-water mark).
func (e *Exchange) SetLastNonce(n int64) { e.lastNonce.Store(n) }

// SetExpiresAfter sets an optional auto-reject deadline for subsequent actions.
func (e *Exchange) SetExpiresAfter(expiresAfter *int64) { e.expiresAfter = expiresAfter }

func (e *Exchange) signL1Action(action any, nonce int64) (SignatureResult, error) {
	return SignL1Action(e.privateKey, action, e.vault, nonce, e.expiresAfter, e.isMainnet())
}

// executeAction signs and posts an action, decoding the response into result.
func (e *Exchange) executeAction(ctx context.Context, action, result any) error {
	nonce := e.nextNonce()
	sig, err := e.signL1Action(action, nonce)
	if err != nil {
		return err
	}
	resp, err := e.postAction(ctx, action, sig, nonce)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(resp, result); err != nil {
		// A 200 whose body we can't parse (truncated by a dying connection, or
		// wire drift): the action reached the exchange — outcome UNKNOWN.
		return &TransportError{Sent: true, Err: err}
	}
	return nil
}

// executeChecked signs and posts an action whose SUCCESS response carries no
// data ({"status":"ok","response":{"type":"default"}}), and returns an error
// when the exchange rejected it ({"status":"err","response":"<message>"}).
// Used for leverage/margin/scheduleCancel, where a silently-dropped rejection
// would falsely report success on a protective control.
func (e *Exchange) executeChecked(ctx context.Context, action any) error {
	nonce := e.nextNonce()
	sig, err := e.signL1Action(action, nonce)
	if err != nil {
		return err
	}
	resp, err := e.postAction(ctx, action, sig, nonce)
	if err != nil {
		return err
	}
	var env struct {
		Status   string          `json:"status"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(resp, &env); err != nil {
		// Unparseable 200 body: the action reached the exchange — outcome UNKNOWN.
		return &TransportError{Sent: true, Err: err}
	}
	if env.Status != "ok" {
		var msg string
		_ = json.Unmarshal(env.Response, &msg) // failure response is a plain string
		if msg == "" {
			msg = env.Status
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

func (e *Exchange) postAction(ctx context.Context, action any, signature SignatureResult, nonce int64) ([]byte, error) {
	payload := map[string]any{"action": action, "nonce": nonce, "signature": signature}
	if e.vault != "" {
		payload["vaultAddress"] = e.vault
	}
	if e.expiresAfter != nil {
		payload["expiresAfter"] = *e.expiresAfter
	}
	return e.transport.post(ctx, "/exchange", payload)
}

// ExchangeOptWebsocket routes SIGNED ACTIONS over the websocket instead of REST.
// Reads are unaffected — they are idempotent and are not what the per-IP weight
// budget needs protecting from.
//
// OFF BY DEFAULT, and it must stay that way. An operator should never acquire a
// new failure mode by upgrading; turning this on is a decision, made once, in
// config. See ws_transport.go for why an unknown outcome on this path must be
// resolved by reading rather than resending.
func ExchangeOptWebsocket(on bool) ExchangeOpt {
	return func(e *Exchange) { e.useWS = on }
}

// Close releases a websocket transport if one is in use, waking any in-flight
// caller as unknown-outcome. A no-op on the HTTP path.
func (e *Exchange) Close() {
	if w, ok := e.transport.(*wsTransport); ok {
		w.Close()
	}
}
