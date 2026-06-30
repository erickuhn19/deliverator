package core

import (
	"context"

	hl "github.com/erickuhn19/deliverator/internal/hl"
)

// ClientAPI is the full surface the CLI (package cmd) invokes on *Client. It
// exists purely so command handlers can be unit-tested against a fake client —
// no network, no keychain — via cmd's swappable newClient seam. *Client is the
// only production implementation; the compile guard below keeps this interface
// in lockstep with Client (a signature drift fails the build, not a test).
type ClientAPI interface {
	// reads
	Balance(ctx context.Context) (*BalanceView, error)
	Bbo(ctx context.Context, coin string) (*BboView, error)
	Book(ctx context.Context, coin string, levels int) (*BookView, error)
	BuilderStatus(ctx context.Context) (*BuilderView, error)
	Candles(ctx context.Context, coin, interval string, since *int64) ([]hl.Candle, error)
	Ctx(ctx context.Context, coin string) (*CtxView, error)
	Fills(ctx context.Context, since *int64, limit int) ([]hl.Fill, error)
	Funding(ctx context.Context, since *int64) ([]hl.UserFundingHistory, error)
	HistoricalOrders(ctx context.Context, limit int) ([]hl.OrderQueryResponse, error)
	Leaderboard(ctx context.Context, p LeaderboardParams) (*LeaderboardView, error)
	Ledger(ctx context.Context, since *int64) ([]hl.UserNonFundingLedgerUpdates, error)
	Limits(ctx context.Context) (*LimitsView, error)
	MeasureSkew(ctx context.Context) (int64, error)
	Mids(ctx context.Context) (map[string]string, error)
	Orders(ctx context.Context, coin string) ([]hl.FrontendOpenOrder, error)
	OrderStatus(ctx context.Context, oid *int64, cloid string) (*hl.OrderQueryResult, error)
	Pnl(ctx context.Context) ([]PnlWindow, error)
	PnlAttribution(ctx context.Context, since *int64, coin string) (*PnlAttributionView, error)
	Portfolio(ctx context.Context) (*PortfolioView, error)
	Positions(ctx context.Context, coin string) ([]PositionView, error)
	PredictedFundings(ctx context.Context, coin string) ([]hl.PredictedFunding, error)
	Preview(ctx context.Context, coin string, side Side, size, limit string, leverage int) (*PreviewResult, error)
	RawInfo(ctx context.Context, body map[string]any) (any, error)
	Reconcile(ctx context.Context, opts ReconcileOpts) (*ReconcileView, error)
	ReferralStatus(ctx context.Context) (*hl.ReferralInfo, error)
	RiskStatus(ctx context.Context) (*RiskView, error)
	RiskStatusFromPortfolio(pf *PortfolioView) *RiskView
	Snapshot(ctx context.Context, coins []string) (*SnapshotView, []string, error)
	TwapStatus(ctx context.Context, coin string, id int64) (*TwapStatusView, error)

	// writes
	AdjustMargin(ctx context.Context, coin string, usd float64) (*MarginResult, error)
	BuildGrid(req GridReq) ([]OrderReq, error)
	Cancel(ctx context.Context, req CancelReq) (*CancelResult, error)
	Close(ctx context.Context, coin, size string, market bool, limit, cloidIn string) (*PlaceResult, []string, error)
	Modify(ctx context.Context, oid *int64, cloid, newSize, newLimit string) (*PlaceResult, []string, error)
	ModifyBatch(ctx context.Context, reqs []ModifyReq) ([]*PlaceResult, []string, error)
	Panic(ctx context.Context) (*PanicResult, error)
	Place(ctx context.Context, req OrderReq) (*PlaceResult, []string, error)
	PlaceBatch(ctx context.Context, reqs []OrderReq) ([]*PlaceResult, []string, error)
	PlaceBracket(ctx context.Context, req BracketReq) ([]*PlaceResult, []string, error)
	PlacePositionTpsl(ctx context.Context, req PositionTpslReq) ([]*PlaceResult, []string, error)
	ScheduleCancel(ctx context.Context, deadlineMs *int64) error
	SetLeverage(ctx context.Context, coin string, x int, cross bool) (*LeverageResult, error)
	SetReferrer(ctx context.Context, code string) error
	Twap(ctx context.Context, req TwapReq) (*TwapResult, []string, error)
	TwapCancel(ctx context.Context, coin string, twapID int64) (*TwapCancelResult, error)

	// copy / streaming / long-running (callback-driven)
	Copy(ctx context.Context, p CopyParams) (*CopyDiff, error)
	CopyExecute(ctx context.Context, diff *CopyDiff, p CopyParams) (*CopyExecuteResult, error)
	Chase(ctx context.Context, p ChaseParams, onEvent func(ChaseEvent)) error
	Stream(ctx context.Context, subs []StreamSub, onEvent func(StreamEvent)) error
	Watch(ctx context.Context, cfg WatchConfig, onEval func(WatchEval), onBreach func(WatchEval)) error

	// session / meta / misc
	EnsureOutcomes(ctx context.Context) error
	Halted() bool
	Meta() *MetaStore
	QueryAddr() string
	RequireQueryAddr() error
}

var _ ClientAPI = (*Client)(nil)
