package settle

import (
	"testing"
	"time"

	"github.com/erickuhn19/deliverator/internal/core"
)

// base is a fixed reference instant all time math is relative to. Chosen to align
// with the minute-resolution ExpiryLayout so parsed expiries are exact.
var base = time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

// expStr formats an instant as the Market.Expiry string core emits, so tests build
// markets the same way core does.
func expStr(t time.Time) string { return t.UTC().Format("2006-01-02 15:04Z") }

func mkt(expiry, status string) core.Market {
	return core.Market{
		IsOutcome:        true,
		Expiry:           expiry,
		ResolutionStatus: status,
	}
}

func TestStateString(t *testing.T) {
	cases := map[State]string{
		Quoting:   "quoting",
		Blackout:  "blackout",
		Settled:   "settled",
		Unknown:   "unknown",
		State(99): "State(99)",
		State(-1): "State(-1)",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", int(s), got, want)
		}
	}
}

func TestStatus(t *testing.T) {
	p := Policy{BlackoutMins: 15}

	tests := []struct {
		name   string
		expiry string
		status string
		now    time.Time
		want   State
	}{
		{
			name:   "settled dominates even with valid future expiry",
			expiry: expStr(base.Add(time.Hour)),
			status: "settled",
			now:    base,
			want:   Settled,
		},
		{
			name:   "settled case-insensitive/whitespace",
			expiry: expStr(base.Add(time.Hour)),
			status: "  SETTLED ",
			now:    base,
			want:   Settled,
		},
		{
			name:   "unparseable expiry -> unknown (fail-closed)",
			expiry: "not-a-date",
			status: "open",
			now:    base,
			want:   Unknown,
		},
		{
			name:   "empty expiry -> unknown",
			expiry: "",
			status: "open",
			now:    base,
			want:   Unknown,
		},
		{
			name:   "well outside window -> quoting",
			expiry: expStr(base.Add(time.Hour)),
			status: "open",
			now:    base,
			want:   Quoting,
		},
		{
			name:   "just outside window (16m) -> quoting",
			expiry: expStr(base.Add(16 * time.Minute)),
			status: "open",
			now:    base,
			want:   Quoting,
		},
		{
			name:   "exactly at window boundary (15m) -> blackout",
			expiry: expStr(base.Add(15 * time.Minute)),
			status: "open",
			now:    base,
			want:   Blackout,
		},
		{
			name:   "inside window (5m) -> blackout",
			expiry: expStr(base.Add(5 * time.Minute)),
			status: "open",
			now:    base,
			want:   Blackout,
		},
		{
			name:   "exactly at expiry (0 ttl) -> blackout",
			expiry: expStr(base),
			status: "open",
			now:    base,
			want:   Blackout,
		},
		{
			name:   "past expiry (negative ttl) but open -> blackout",
			expiry: expStr(base.Add(-30 * time.Minute)),
			status: "open",
			now:    base,
			want:   Blackout,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.Status(mkt(tt.expiry, tt.status), tt.now); got != tt.want {
				t.Errorf("Status = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestStatusZeroBlackout: with a zero window only expiry-or-past is blackout; anything
// strictly in the future keeps quoting.
func TestStatusZeroBlackout(t *testing.T) {
	p := Policy{BlackoutMins: 0}
	if got := p.Status(mkt(expStr(base.Add(time.Minute)), "open"), base); got != Quoting {
		t.Errorf("1m out, zero window: got %v want Quoting", got)
	}
	if got := p.Status(mkt(expStr(base), "open"), base); got != Blackout {
		t.Errorf("at expiry, zero window: got %v want Blackout", got)
	}
}

// TestInBlackout is the truth table: only Quoting is NOT blackout.
func TestInBlackout(t *testing.T) {
	p := Policy{BlackoutMins: 15}
	tests := []struct {
		name   string
		expiry string
		status string
		want   bool
	}{
		{"quoting -> false", expStr(base.Add(time.Hour)), "open", false},
		{"blackout -> true", expStr(base.Add(time.Minute)), "open", true},
		{"settled -> true", expStr(base.Add(time.Hour)), "settled", true},
		{"unknown -> true", "bad", "open", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.InBlackout(mkt(tt.expiry, tt.status), base); got != tt.want {
				t.Errorf("InBlackout = %v, want %v", got, tt.want)
			}
		})
	}
}

func priceMkt(target string, expiry time.Time) core.Market {
	return core.Market{
		IsOutcome:        true,
		TargetPrice:      target,
		Expiry:           expStr(expiry),
		ResolutionStatus: "settled",
	}
}

func TestEstimateWinner(t *testing.T) {
	exp := base

	tests := []struct {
		name    string
		target  string
		expiry  string // raw override; if "" use expStr(exp)
		samples []MarkSample
		wantWin bool
		wantOK  bool
	}{
		{
			name:    "no samples -> not ok",
			target:  "100",
			samples: nil,
			wantOK:  false,
		},
		{
			name:    "unparseable target -> not ok",
			target:  "abc",
			samples: []MarkSample{{T: exp, Px: 100}},
			wantOK:  false,
		},
		{
			name:   "flanking straddle target: interp >= target -> YES",
			target: "100",
			samples: []MarkSample{
				{T: exp.Add(-2 * time.Minute), Px: 90}, // before
				{T: exp.Add(2 * time.Minute), Px: 110}, // after; interp at expiry = 100
			},
			wantWin: true,
			wantOK:  true,
		},
		{
			name:   "flanking interp just under target -> NO",
			target: "100.001",
			samples: []MarkSample{
				{T: exp.Add(-2 * time.Minute), Px: 90},
				{T: exp.Add(2 * time.Minute), Px: 110}, // interp = 100 < 100.001
			},
			wantWin: false,
			wantOK:  true,
		},
		{
			name:   "asymmetric flanking: expiry near later sample",
			target: "100",
			// before at -6m px 80, after at +2m px 108. frac = 6/8 = 0.75.
			// interp = 80 + 0.75*(108-80) = 80 + 21 = 101 >= 100 -> YES
			samples: []MarkSample{
				{T: exp.Add(-6 * time.Minute), Px: 80},
				{T: exp.Add(2 * time.Minute), Px: 108},
			},
			wantWin: true,
			wantOK:  true,
		},
		{
			name:   "sample exactly at expiry decides",
			target: "100",
			samples: []MarkSample{
				{T: exp.Add(-time.Minute), Px: 10},
				{T: exp, Px: 100}, // exactly at expiry
				{T: exp.Add(time.Minute), Px: 999},
			},
			// before=exp(100 via later of {-1m,exp}? both <= exp, latest is exp px100)
			// after = earliest >= exp = exp px100. dt=0 -> mark=100 >= 100 YES
			wantWin: true,
			wantOK:  true,
		},
		{
			name:   "only-before fallback nearest",
			target: "100",
			samples: []MarkSample{
				{T: exp.Add(-10 * time.Minute), Px: 50},
				{T: exp.Add(-1 * time.Minute), Px: 120}, // nearest before
			},
			wantWin: true, // 120 >= 100
			wantOK:  true,
		},
		{
			name:   "only-after fallback nearest",
			target: "100",
			samples: []MarkSample{
				{T: exp.Add(1 * time.Minute), Px: 40}, // nearest after
				{T: exp.Add(10 * time.Minute), Px: 200},
			},
			wantWin: false, // 40 < 100
			wantOK:  true,
		},
		{
			name:    "single before sample",
			target:  "100",
			samples: []MarkSample{{T: exp.Add(-time.Minute), Px: 100}},
			wantWin: true, // 100 >= 100 (boundary inclusive)
			wantOK:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := priceMkt(tt.target, exp)
			if tt.expiry != "" {
				m.Expiry = tt.expiry
			}
			win, ok := EstimateWinner(m, tt.samples)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && win != tt.wantWin {
				t.Errorf("winnerYes = %v, want %v", win, tt.wantWin)
			}
		})
	}
}

// TestEstimateWinnerUnparseableExpiry: a bad expiry string fails closed.
func TestEstimateWinnerUnparseableExpiry(t *testing.T) {
	m := core.Market{IsOutcome: true, TargetPrice: "100", Expiry: "nope"}
	if _, ok := EstimateWinner(m, []MarkSample{{T: base, Px: 100}}); ok {
		t.Errorf("expected ok=false for unparseable expiry")
	}
}

// TestEstimateWinnerUnorderedSamples: flanking detection must not assume samples are
// sorted — it scans for the latest-before and earliest-after regardless of order.
func TestEstimateWinnerUnorderedSamples(t *testing.T) {
	exp := base
	m := priceMkt("100", exp)
	samples := []MarkSample{
		{T: exp.Add(2 * time.Minute), Px: 110},
		{T: exp.Add(-10 * time.Minute), Px: 0},
		{T: exp.Add(-2 * time.Minute), Px: 90}, // this is the real "before"
		{T: exp.Add(30 * time.Minute), Px: 999},
	}
	// before=-2m/90, after=+2m/110 -> interp at expiry = 100 -> YES
	win, ok := EstimateWinner(m, samples)
	if !ok || !win {
		t.Errorf("got win=%v ok=%v, want win=true ok=true", win, ok)
	}
}
