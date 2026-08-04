// Package serve exposes deliverator's engine over a local Unix socket so a bot
// can drive it as a long-lived component instead of forking the binary for every
// call.
//
// # WHY THIS EXISTS
//
// The CLI is one front-end onto core.Client. Fork-per-call is fine for a human
// and wrong for a market maker: a downstream bot was spending one process per
// book read, per placement, per cancel, and a 1s quote loop tripped Hyperliquid's
// per-IP weight limit within seconds. Process spawn is the cheap part; what it
// costs is a fresh TLS handshake and a fresh connection on every single call, and
// it makes a persistent websocket for order entry impossible — there is nothing
// alive to hold one open.
//
// One server process means one client, one connection pool, one nonce holder,
// and somewhere for a websocket to live. That last point is the whole reason
// this lands before the WS order transport (#27) rather than after it.
//
// # WHAT THIS IS NOT
//
// It is not a remote API and must never become one. The socket can SIGN — it
// reaches the same keychain-backed agent as the CLI — so it is a Unix domain
// socket with owner-only permissions, never a TCP port. There is no auth beyond
// filesystem permissions, which is exactly why it must not be reachable off the
// box.
//
// It is not a full CLI surface either. The methods below are the ones an
// execution loop needs. Cobra's commands are package-level singletons carrying
// flag state between runs, so re-executing them in-process would leak flags from
// one request into the next; exposing the ENGINE avoids that entirely and gives
// a typed contract rather than shell strings.
//
// STREAMS DELIBERATELY STAY ON THE CLI. Market-data feeds are already one
// long-lived subprocess each — they are not the churn. The per-call cost is on
// the request/response path, and that is what this covers.
package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/output"
)

// Request is one call. Method names mirror the CLI verbs so the two surfaces
// stay legible against each other; Params is method-specific.
//
// ID is echoed back verbatim. It exists so a caller may pipeline several
// requests down one connection and still match answers to questions — replies
// are not guaranteed to arrive in request order once that is allowed.
type Request struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is the CLI's envelope plus the correlation id, so anything that can
// already parse deliverator's stdout can parse this.
type Response struct {
	ID string `json:"id"`
	output.Envelope
}

// Engine is the subset of core.Client this server exposes, matching that type's
// real signatures. An interface rather than the concrete type so the server is
// testable without a venue, a keychain or a network.
//
// Read methods that return a core.ReadMeta return it here too: partial coverage
// must stay machine-visible across this hop, exactly as it is on stdout. A
// server that flattened a degraded read into a clean one would reintroduce the
// failure #30 just removed.
type Engine interface {
	Book(ctx context.Context, coin string, levels int) (*core.BookView, error)
	Ctx(ctx context.Context, coin string) (*core.CtxView, error)
	Balance(ctx context.Context) (*core.BalanceView, error)
	Portfolio(ctx context.Context) (*core.PortfolioView, error)
	Orders(ctx context.Context, coin string) ([]hl.FrontendOpenOrder, core.ReadMeta, error)
	Fills(ctx context.Context, since *int64, limit int) ([]hl.Fill, core.ReadMeta, error)
	Place(ctx context.Context, req core.OrderReq) (*core.PlaceResult, []string, error)
	PlaceBatch(ctx context.Context, reqs []core.OrderReq) ([]*core.PlaceResult, []string, error)
	Cancel(ctx context.Context, req core.CancelReq) (*core.CancelResult, error)
	ScheduleCancel(ctx context.Context, deadlineMs *int64) error

	// RefreshOutcomes reloads the HIP-4 universe. A server outlives the DAILY
	// roll that retires a coin and lists its successor, so a universe cached at
	// startup goes stale every day. See reloadOutcomesFor.
	RefreshOutcomes(ctx context.Context) error

	// ReloadGuardsIfChanged re-reads the risk caps when the config file changed,
	// and returns any warnings to attach to this request's envelope. `config set`
	// writes the file and exits, so a server that captured risk config at startup
	// enforces it forever otherwise — the fork path and the socket path then
	// disagree about the caps, which is precisely what hid a two-day outage (#41).
	ReloadGuardsIfChanged(path string) []string

	// GuardGeneration names the config the gates are currently enforcing, so a
	// rejection can carry it and a stale cache is visible in the error itself.
	GuardGeneration() string

	// UniverseGeneration names the HIP-4 universe being enforced AND whether the
	// signer agrees with it. The same staleness family produced three multi-hour
	// incidents (#37, #41, #43), each invisible the same way: the process answered
	// confidently from a snapshot nothing in its output identified (#45).
	UniverseGeneration() string
}

// Server accepts requests on a Unix socket until its context ends.
type Server struct {
	eng Engine
	// configPath is the risk config watched for changes on each request. Empty
	// means the default location.
	configPath string
	path       string
	meta       func() output.Meta

	ln net.Listener

	// outcomeRefreshAt rate-limits the universe reload so a caller hammering a
	// genuinely bad coin cannot turn every request into an API fetch.
	refreshMu        sync.Mutex
	outcomeRefreshAt time.Time

	// One in-flight request per CONNECTION, enforced by handling a connection's
	// lines sequentially. Concurrency across connections is allowed and the
	// engine's own locking covers it; serialising within a connection keeps a
	// single caller's ordering intuitive, which for an order path is worth more
	// than the parallelism.
	wg sync.WaitGroup
}

// New builds a server. The socket path is created on Start.
func New(eng Engine, socketPath string, meta func() output.Meta) *Server {
	return &Server{eng: eng, path: socketPath, meta: meta}
}

// WatchConfig points the per-request risk-config freshness check at a specific
// file. Empty (the default) uses the standard config location.
func (s *Server) WatchConfig(path string) *Server {
	s.configPath = path
	return s
}

// Listen creates the socket. Separate from Serve so a caller can report a bind
// failure before daemonising, and so tests can find the address.
func (s *Server) Listen() error {
	if s.path == "" {
		return errors.New("serve: socket path is empty")
	}
	// Unix sockets have a hard path limit in the kernel struct (sun_path: 104
	// bytes on darwin, 108 on linux) and exceeding it fails with a bare
	// "bind: invalid argument" that says nothing about length. Caught here with a
	// message that names the actual problem — this bit the tests first, and a
	// deep XDG or project path would do the same to an operator.
	if len(s.path) >= sunPathMax {
		return fmt.Errorf("serve: socket path is %d bytes, over the %d-byte kernel limit: %s — "+
			"use a shorter path via --socket or $DELIVERATOR_SOCKET",
			len(s.path), sunPathMax, s.path)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("serve: create socket directory: %w", err)
	}
	// A stale socket from a killed process would make Listen fail with "address
	// already in use" forever. Removing it is safe: a LIVE server holds the file
	// open but a second bind would have failed anyway, and we check that below by
	// dialling first.
	if c, err := net.DialTimeout("unix", s.path, 200*time.Millisecond); err == nil {
		c.Close()
		return fmt.Errorf("serve: another server is already listening on %s", s.path)
	}
	_ = os.Remove(s.path)

	ln, err := net.Listen("unix", s.path)
	if err != nil {
		return fmt.Errorf("serve: listen on %s: %w", s.path, err)
	}
	// OWNER ONLY. This socket can sign orders. Anything wider would hand the
	// account to every user on the box, and there is no second layer of auth
	// behind it.
	if err := os.Chmod(s.path, 0o600); err != nil {
		ln.Close()
		return fmt.Errorf("serve: restrict socket permissions: %w", err)
	}
	s.ln = ln
	return nil
}

// Addr is the socket path actually bound.
func (s *Server) Addr() string { return s.path }

// Serve runs until ctx ends, then stops accepting and waits for in-flight
// requests. A signing call that is halfway to the venue must not be abandoned:
// the order would land with nobody left to record it.
func (s *Server) Serve(ctx context.Context) error {
	if s.ln == nil {
		if err := s.Listen(); err != nil {
			return err
		}
	}
	defer func() {
		s.ln.Close()
		_ = os.Remove(s.path)
	}()

	go func() {
		<-ctx.Done()
		s.ln.Close() // unblocks Accept
	}()

	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.wg.Wait()
				return nil
			}
			return fmt.Errorf("serve: accept: %w", err)
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(ctx, conn)
		}()
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	// Requests are small; batch legs are the largest realistic payload. The cap
	// stops a malformed peer from growing the buffer without bound.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	enc := json.NewEncoder(conn)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(s.fail("", "request", output.Validation("bad_request",
				"could not decode request: "+err.Error())))
			continue
		}
		resp := s.dispatch(ctx, req)
		if err := enc.Encode(resp); err != nil {
			return // peer went away mid-write; nothing useful left to say
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (s *Server) meta_() output.Meta {
	if s.meta == nil {
		return output.Meta{}
	}
	return s.meta()
}

func (s *Server) ok(id, cmd string, data any, warnings []string) Response {
	if warnings == nil {
		warnings = []string{}
	}
	return Response{ID: id, Envelope: output.Envelope{
		Schema: output.SchemaVersion, OK: true, Ts: output.Now(), Cmd: cmd,
		Data: data, Warnings: warnings, Meta: s.meta_(),
	}}
}

func (s *Server) fail(id, cmd string, e *output.Error) Response {
	return Response{ID: id, Envelope: output.Envelope{
		Schema: output.SchemaVersion, OK: false, Ts: output.Now(), Cmd: cmd,
		Error: e, Warnings: []string{}, Meta: s.meta_(),
	}}
}
