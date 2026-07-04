package arb

import (
	"math"
	"testing"

	"github.com/erickuhn19/deliverator/internal/core"
	"github.com/erickuhn19/deliverator/internal/mm"
)

// ask builds a BookTop that has only an ask side (the only side the buy-both scan
// reads), so tests stay focused on the arb inputs that matter.
func ask(px, sz float64) mm.BookTop {
	return mm.BookTop{Ask: px, AskSz: sz, HasAsk: true}
}

func TestScanCrossBook(t *testing.T) {
	m := core.Market{Outcome: 3} // YesCoin="#30", NoCoin="#31"
	wantYes, wantNo := mm.YesCoin(3), mm.NoCoin(3)

	tests := []struct {
		name       string
		yes, no    mm.BookTop
		p          Params
		wantOK     bool
		wantShares int64
		wantEdge   float64
	}{
		{
			// 0.40 + 0.50 = 0.90, no fee ⇒ edge 0.10 >> MinEdge; sized by thinner leg.
			name:       "arb found, sized by thinner leg",
			yes:        ask(0.40, 100),
			no:         ask(0.50, 60),
			p:          Params{MinEdge: 0.005, MaxSizeShares: 1000},
			wantOK:     true,
			wantShares: 60,
			wantEdge:   0.10,
		},
		{
			// Sum exactly 1.0 ⇒ edge 0, below MinEdge ⇒ no arb.
			name:   "sum equals one, no edge",
			yes:    ask(0.50, 100),
			no:     ask(0.50, 100),
			p:      Params{MinEdge: 0.005, MaxSizeShares: 1000},
			wantOK: false,
		},
		{
			// edge == MinEdge exactly must be REJECTED (strict: edge < MinEdge fails,
			// but the spec requires clearing the bar; edge==MinEdge is the boundary).
			// 1 - 0.40 - 0.595 = 0.005 == MinEdge ⇒ accepted (not < MinEdge).
			name:       "edge exactly equals MinEdge is accepted",
			yes:        ask(0.40, 100),
			no:         ask(0.595, 100),
			p:          Params{MinEdge: 0.005, MaxSizeShares: 1000},
			wantOK:     true,
			wantShares: 100,
			wantEdge:   0.005,
		},
		{
			// Just under the bar: edge 0.004 < MinEdge 0.005 ⇒ rejected.
			name:   "edge just below MinEdge",
			yes:    ask(0.40, 100),
			no:     ask(0.596, 100),
			p:      Params{MinEdge: 0.005, MaxSizeShares: 1000},
			wantOK: false,
		},
		{
			// Settle fee eats the edge: 1 - 0.45 - 0.50 - 0.04 = 0.01, still > MinEdge.
			name:       "settle fee reduces but keeps edge",
			yes:        ask(0.45, 200),
			no:         ask(0.50, 200),
			p:          Params{MinEdge: 0.005, MaxSizeShares: 1000, SettleFeeFrac: 0.04},
			wantOK:     true,
			wantShares: 200,
			wantEdge:   0.01,
		},
		{
			// Settle fee pushes edge below MinEdge: 1-0.45-0.50-0.06 = -0.01 ⇒ reject.
			name:   "settle fee kills edge",
			yes:    ask(0.45, 200),
			no:     ask(0.50, 200),
			p:      Params{MinEdge: 0.005, MaxSizeShares: 1000, SettleFeeFrac: 0.06},
			wantOK: false,
		},
		{
			// MaxSizeShares caps below the available min(100,80)=80.
			name:       "capped by MaxSizeShares",
			yes:        ask(0.40, 100),
			no:         ask(0.50, 80),
			p:          Params{MinEdge: 0.005, MaxSizeShares: 25},
			wantOK:     true,
			wantShares: 25,
			wantEdge:   0.10,
		},
		{
			// Fractional book sizes floor to integers before the min: floor(60.9)=60,
			// floor(59.2)=59 ⇒ 59.
			name:       "fractional sizes floored",
			yes:        ask(0.40, 60.9),
			no:         ask(0.50, 59.2),
			p:          Params{MinEdge: 0.005, MaxSizeShares: 1000},
			wantOK:     true,
			wantShares: 59,
			wantEdge:   0.10,
		},
		{
			// Missing yes ask ⇒ false.
			name:   "no yes ask",
			yes:    mm.BookTop{HasAsk: false},
			no:     ask(0.50, 100),
			p:      Params{MinEdge: 0.005, MaxSizeShares: 1000},
			wantOK: false,
		},
		{
			// Missing no ask ⇒ false.
			name:   "no no ask",
			yes:    ask(0.40, 100),
			no:     mm.BookTop{HasAsk: false},
			p:      Params{MinEdge: 0.005, MaxSizeShares: 1000},
			wantOK: false,
		},
		{
			// MinNotional gate: cheap legs, only 5 shares available. yes notional
			// 0.10*5=0.50 < default $10 ⇒ reject even though edge is huge.
			name:   "fails default MinNotional floor",
			yes:    ask(0.10, 5),
			no:     ask(0.10, 5),
			p:      Params{MinEdge: 0.005, MaxSizeShares: 1000},
			wantOK: false,
		},
		{
			// Same cheap legs but enough shares: 0.10*200=20 >= $10 on both ⇒ pass.
			name:       "clears default MinNotional with size",
			yes:        ask(0.10, 200),
			no:         ask(0.10, 200),
			p:          Params{MinEdge: 0.005, MaxSizeShares: 1000},
			wantOK:     true,
			wantShares: 200,
			wantEdge:   0.80,
		},
		{
			// Explicit MinNotionalUSD gate: yes leg 0.40*50=20 >= 25? no ⇒ reject.
			name:   "fails explicit MinNotionalUSD on thin leg",
			yes:    ask(0.40, 50),
			no:     ask(0.50, 50),
			p:      Params{MinEdge: 0.005, MaxSizeShares: 1000, MinNotionalUSD: 25},
			wantOK: false,
		},
		{
			// MaxSizeShares cap drives shares below MinNotional: cap=10, yes
			// 0.40*10=4 < $10 ⇒ reject. Confirms the notional gate runs AFTER capping.
			name:   "cap pushes below MinNotional",
			yes:    ask(0.40, 1000),
			no:     ask(0.50, 1000),
			p:      Params{MinEdge: 0.005, MaxSizeShares: 10},
			wantOK: false,
		},
		{
			// Zero available size on a leg (HasAsk true but AskSz 0) ⇒ shares 0 ⇒ false.
			name:   "zero ask size floors to zero shares",
			yes:    ask(0.40, 0),
			no:     ask(0.50, 100),
			p:      Params{MinEdge: 0.005, MaxSizeShares: 1000},
			wantOK: false,
		},
		{
			// MaxSizeShares unset (0) must NOT cap to zero; it means "no cap".
			name:       "zero MaxSizeShares means no cap",
			yes:        ask(0.40, 100),
			no:         ask(0.50, 100),
			p:          Params{MinEdge: 0.005},
			wantOK:     true,
			wantShares: 100,
			wantEdge:   0.10,
		},
	}

	const eps = 1e-9
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ScanCrossBook(m, tt.yes, tt.no, tt.p)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tt.wantOK, got)
			}
			if !tt.wantOK {
				if got != (Opportunity{}) {
					t.Fatalf("rejected scan must return zero Opportunity, got %+v", got)
				}
				return
			}
			if got.Shares != tt.wantShares {
				t.Errorf("Shares = %d, want %d", got.Shares, tt.wantShares)
			}
			if math.Abs(got.EdgePerShare-tt.wantEdge) > eps {
				t.Errorf("EdgePerShare = %v, want %v", got.EdgePerShare, tt.wantEdge)
			}
			if got.YesCoin != wantYes || got.NoCoin != wantNo {
				t.Errorf("coins = %s/%s, want %s/%s", got.YesCoin, got.NoCoin, wantYes, wantNo)
			}
			if got.YesPx != tt.yes.Ask || got.NoPx != tt.no.Ask {
				t.Errorf("px = %v/%v, want %v/%v", got.YesPx, got.NoPx, tt.yes.Ask, tt.no.Ask)
			}
		})
	}
}
