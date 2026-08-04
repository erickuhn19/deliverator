package serve

// The method table.
//
// Every handler is a thin adapter onto core.Client. There is deliberately no
// business logic here: the risk gates, the nonce discipline and the audit trail
// all live below this layer, so a bot driving the socket is subject to exactly
// the same envelope as a human driving the CLI. A server that could place an
// order the CLI would refuse would be a way to launder around the risk config,
// and that is the one thing this must never become.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/output"
)

// Methods lists what the socket answers, for the `ping` handshake and for the
// error message when a caller asks for something else.
var Methods = []string{
	"ping", "book", "ctx", "balance", "portfolio", "orders", "fills",
	"place", "batch", "cancel", "dms",
}

func (s *Server) dispatch(ctx context.Context, req Request) Response {
	if req.Method == "" {
		return s.fail(req.ID, "serve", output.Validation("no_method",
			"request has no method").WithHint("one of: "+joinMethods()))
	}
	h, ok := handlers[req.Method]
	if !ok {
		return s.fail(req.ID, req.Method, output.Validation("unknown_method",
			fmt.Sprintf("method %q is not served", req.Method)).
			WithHint("serve mode exposes the execution path only; streams and admin stay on the CLI. Available: "+joinMethods()))
	}
	resp := h(s, ctx, req)

	// A "#<enc>" coin the universe does not know is almost always a stale cache,
	// not a bad request: these markets roll DAILY and this process outlives the
	// roll. Reload once and retry.
	//
	// Retrying is safe HERE and only here. unknown_coin is a VALIDATION error
	// raised while resolving the market, long before anything is signed — so
	// nothing reached the venue and there is no double-fill to create. No other
	// failure gets this treatment.
	if !resp.OK && resp.Error != nil && resp.Error.Code == "unknown_coin" &&
		strings.Contains(resp.Error.Message, "#") && s.reloadOutcomes(ctx) {
		resp = h(s, ctx, req)
	}
	return resp
}

// reloadOutcomes refreshes the HIP-4 universe, at most once every 30s. Reports
// whether a refresh actually ran, so the caller only retries when the world may
// have changed underneath it.
func (s *Server) reloadOutcomes(ctx context.Context) bool {
	s.refreshMu.Lock()
	if time.Since(s.outcomeRefreshAt) < 30*time.Second {
		s.refreshMu.Unlock()
		return false
	}
	s.outcomeRefreshAt = time.Now()
	s.refreshMu.Unlock()
	return s.eng.RefreshOutcomes(ctx) == nil
}

func joinMethods() string {
	out := ""
	for i, m := range Methods {
		if i > 0 {
			out += ", "
		}
		out += m
	}
	return out
}

type handler func(*Server, context.Context, Request) Response

// decodeParams unmarshals into v, treating absent params as an empty object so
// a no-argument method does not need `"params":{}` written out.
func decodeParams(req Request, v any) error {
	if len(req.Params) == 0 || string(req.Params) == "null" {
		return nil
	}
	return json.Unmarshal(req.Params, v)
}

func badParams(s *Server, req Request, err error) Response {
	return s.fail(req.ID, req.Method, output.Validation("bad_params",
		"could not decode params: "+err.Error()))
}

// engineErr maps an engine error onto the envelope. A *output.Error from the
// gates is passed through UNCHANGED — its code, category, retryability and any
// cloids are the contract an agent acts on, and flattening them to a string here
// would break the §5.4 status-check protocol on an ambiguous write.
func engineErr(s *Server, req Request, err error) Response {
	if e, ok := err.(*output.Error); ok {
		return s.fail(req.ID, req.Method, e)
	}
	return s.fail(req.ID, req.Method, output.Unknown("engine", err.Error()))
}

var handlers = map[string]handler{
	// ping is the handshake: it proves the socket is a deliverator server of a
	// known schema before a bot sends anything that signs.
	"ping": func(s *Server, _ context.Context, req Request) Response {
		return s.ok(req.ID, "ping", map[string]any{
			"schema": output.SchemaVersion, "methods": Methods,
		}, nil)
	},

	"book": func(s *Server, ctx context.Context, req Request) Response {
		var p struct {
			Coin   string `json:"coin"`
			Levels int    `json:"levels"`
		}
		if err := decodeParams(req, &p); err != nil {
			return badParams(s, req, err)
		}
		v, err := s.eng.Book(ctx, p.Coin, p.Levels)
		if err != nil {
			return engineErr(s, req, err)
		}
		return s.ok(req.ID, "book", v, nil)
	},

	"ctx": func(s *Server, ctx context.Context, req Request) Response {
		var p struct {
			Coin string `json:"coin"`
		}
		if err := decodeParams(req, &p); err != nil {
			return badParams(s, req, err)
		}
		v, err := s.eng.Ctx(ctx, p.Coin)
		if err != nil {
			return engineErr(s, req, err)
		}
		return s.ok(req.ID, "ctx", v, nil)
	},

	"balance": func(s *Server, ctx context.Context, req Request) Response {
		v, err := s.eng.Balance(ctx)
		if err != nil {
			return engineErr(s, req, err)
		}
		return s.ok(req.ID, "balance", v, nil)
	},

	"portfolio": func(s *Server, ctx context.Context, req Request) Response {
		v, err := s.eng.Portfolio(ctx)
		if err != nil {
			return engineErr(s, req, err)
		}
		return s.ok(req.ID, "portfolio", v, nil)
	},

	"orders": func(s *Server, ctx context.Context, req Request) Response {
		var p struct {
			Coin string `json:"coin"`
		}
		if err := decodeParams(req, &p); err != nil {
			return badParams(s, req, err)
		}
		v, rm, err := s.eng.Orders(ctx, p.Coin)
		if err != nil {
			return engineErr(s, req, err)
		}
		return s.okRead(req.ID, "orders", v, rm)
	},

	"fills": func(s *Server, ctx context.Context, req Request) Response {
		var p struct {
			Since *int64 `json:"since"`
			Limit int    `json:"limit"`
		}
		if err := decodeParams(req, &p); err != nil {
			return badParams(s, req, err)
		}
		v, rm, err := s.eng.Fills(ctx, p.Since, p.Limit)
		if err != nil {
			return engineErr(s, req, err)
		}
		return s.okRead(req.ID, "fills", v, rm)
	},

	"place": func(s *Server, ctx context.Context, req Request) Response {
		var p OrderParams
		if err := decodeParams(req, &p); err != nil {
			return badParams(s, req, err)
		}
		o, err := p.toCore()
		if err != nil {
			return badParams(s, req, err)
		}
		res, warn, err := s.eng.Place(ctx, o)
		if err != nil {
			return engineErr(s, req, err)
		}
		return s.ok(req.ID, "place", res, warn)
	},

	"batch": func(s *Server, ctx context.Context, req Request) Response {
		var p struct {
			Orders []OrderParams `json:"orders"`
		}
		if err := decodeParams(req, &p); err != nil {
			return badParams(s, req, err)
		}
		if len(p.Orders) == 0 {
			return s.fail(req.ID, "batch", output.Validation("no_orders",
				"batch needs at least one order"))
		}
		orders := make([]core.OrderReq, len(p.Orders))
		for i, po := range p.Orders {
			o, err := po.toCore()
			if err != nil {
				return badParams(s, req, fmt.Errorf("order %d: %w", i, err))
			}
			orders[i] = o
		}
		res, warn, err := s.eng.PlaceBatch(ctx, orders)
		if err != nil {
			return engineErr(s, req, err)
		}
		return s.ok(req.ID, "batch", res, warn)
	},

	"cancel": func(s *Server, ctx context.Context, req Request) Response {
		var p CancelParams
		if err := decodeParams(req, &p); err != nil {
			return badParams(s, req, err)
		}
		res, err := s.eng.Cancel(ctx, p.toCore())
		if err != nil {
			return engineErr(s, req, err)
		}
		return s.ok(req.ID, "cancel", res, nil)
	},

	// dms carries the dead-man switch. Deadline nil CLEARS it; a value arms it.
	// Exposed here because a bot holding a persistent connection is exactly the
	// caller that should be heartbeating one.
	"dms": func(s *Server, ctx context.Context, req Request) Response {
		var p struct {
			DeadlineMs *int64 `json:"deadline_ms"`
		}
		if err := decodeParams(req, &p); err != nil {
			return badParams(s, req, err)
		}
		if err := s.eng.ScheduleCancel(ctx, p.DeadlineMs); err != nil {
			return engineErr(s, req, err)
		}
		return s.ok(req.ID, "dms", map[string]any{"deadline_ms": p.DeadlineMs}, nil)
	},
}

// okRead carries a read's health across the socket. A degraded read stays
// degraded here: DegradedDexs means those sub-dexs' rows are MISSING from data
// rather than absent from the account, and a caller that cannot see that would
// reconcile against a partial book believing it complete.
func (s *Server) okRead(id, cmd string, data any, rm core.ReadMeta) Response {
	r := s.ok(id, cmd, data, warningsFor(rm))
	r.Envelope.DegradedDexs = rm.Degraded
	r.Envelope.Truncated = rm.Truncated
	return r
}

func warningsFor(rm core.ReadMeta) []string {
	var w []string
	for _, d := range rm.Degraded {
		w = append(w, "degraded read: "+d+" state is unavailable; its rows are missing from this response")
	}
	if rm.Truncated {
		w = append(w, "truncated: this history read stopped at its safety cap and more rows are available")
	}
	return w
}
