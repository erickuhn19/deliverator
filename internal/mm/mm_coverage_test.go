package mm

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/erickuhn19/deliverator/internal/core"
)

// TestChunk exercises the generic batcher across empty, remainder, exact, and
// non-positive-n inputs, asserting the exact partition (order + boundaries),
// since Chunk backs the per-action order-batch limit.
func TestChunk(t *testing.T) {
	// remainder: last chunk is short.
	if got := Chunk([]int{1, 2, 3, 4, 5}, 2); !reflect.DeepEqual(got, [][]int{{1, 2}, {3, 4}, {5}}) {
		t.Fatalf("Chunk remainder = %v", got)
	}
	// exact multiple: no short trailing chunk.
	if got := Chunk([]int{1, 2, 3, 4}, 2); !reflect.DeepEqual(got, [][]int{{1, 2}, {3, 4}}) {
		t.Fatalf("Chunk exact = %v", got)
	}
	// n == 1: one element per chunk.
	if got := Chunk([]int{7, 8, 9}, 1); !reflect.DeepEqual(got, [][]int{{7}, {8}, {9}}) {
		t.Fatalf("Chunk n=1 = %v", got)
	}
	// n larger than len: a single chunk holding everything.
	if got := Chunk([]string{"a", "b"}, 10); !reflect.DeepEqual(got, [][]string{{"a", "b"}}) {
		t.Fatalf("Chunk n>len = %v", got)
	}
	// empty input: no chunks at all (nil result, len 0).
	if got := Chunk([]int{}, 3); len(got) != 0 {
		t.Fatalf("Chunk empty = %v, want no chunks", got)
	}
	// n <= 0: the whole slice is returned as one chunk (guard branch), NOT split.
	if got := Chunk([]int{1, 2, 3}, 0); !reflect.DeepEqual(got, [][]int{{1, 2, 3}}) {
		t.Fatalf("Chunk n=0 = %v, want single whole chunk", got)
	}
	if got := Chunk([]int{1, 2, 3}, -5); !reflect.DeepEqual(got, [][]int{{1, 2, 3}}) {
		t.Fatalf("Chunk n<0 = %v, want single whole chunk", got)
	}
}

// TestParseFloat covers the trim + parse + failure paths and the ok=false contract
// (empty / blank / non-numeric return 0,false rather than a sentinel).
func TestParseFloat(t *testing.T) {
	cases := []struct {
		in     string
		wantF  float64
		wantOk bool
	}{
		{"3.14", 3.14, true},
		{"  2.5  ", 2.5, true}, // surrounding whitespace is trimmed
		{"1e3", 1000, true},    // scientific notation is accepted by strconv
		{"-0.5", -0.5, true},
		{"0", 0, true},
		{"", 0, false},      // empty
		{"   ", 0, false},   // blank → empty after trim
		{"abc", 0, false},   // non-numeric
		{"1.2.3", 0, false}, // malformed
		{"0x10", 0, false},  // not a base-10 float
	}
	for _, c := range cases {
		gotF, gotOk := ParseFloat(c.in)
		if gotOk != c.wantOk || (gotOk && gotF != c.wantF) {
			t.Errorf("ParseFloat(%q) = (%v, %v), want (%v, %v)", c.in, gotF, gotOk, c.wantF, c.wantOk)
		}
	}
}

// TestQuestionKey verifies the bucketing key: a real question keys on the question
// id and takes precedence over Outcome; a standalone binary (Question==0) keys on
// its Outcome. The selector penalty and engine notional tracker must bucket alike.
func TestQuestionKey(t *testing.T) {
	if got := QuestionKey(core.Market{Question: 42, Outcome: 7}); got != "q:42" {
		t.Errorf("QuestionKey(question) = %q, want q:42", got)
	}
	// Question != 0 wins even when Outcome is also set.
	if got := QuestionKey(core.Market{Question: 3, Outcome: 999}); got != "q:3" {
		t.Errorf("QuestionKey precedence = %q, want q:3", got)
	}
	// Standalone binary: no question id, key on Outcome.
	if got := QuestionKey(core.Market{Question: 0, Outcome: 7}); got != "o:7" {
		t.Errorf("QuestionKey(standalone) = %q, want o:7", got)
	}
	// A Yes and its No sibling (same Outcome, Question==0) bucket together.
	yes := core.Market{Outcome: 5, Side: "Yes"}
	no := core.Market{Outcome: 5, Side: "No"}
	if QuestionKey(yes) != QuestionKey(no) {
		t.Errorf("Yes/No siblings must share a key: %q vs %q", QuestionKey(yes), QuestionKey(no))
	}
}

// TestParseBookTop projects core.BboView → BookTop over nil, one-sided, two-sided,
// and unparseable-field inputs, asserting the exact numeric values AND the
// Has-flags (a real 0-side vs "no level" must be distinguishable).
func TestParseBookTop(t *testing.T) {
	// nil view → zero BookTop: no levels present.
	got := ParseBookTop(nil)
	if got != (BookTop{}) {
		t.Fatalf("ParseBookTop(nil) = %+v, want zero value", got)
	}

	// bid-only (one-sided): HasBid set, HasAsk unset, and no mid.
	got = ParseBookTop(&core.BboView{Bid: "0.40", BidSz: "12"})
	if !got.HasBid || got.HasAsk {
		t.Fatalf("bid-only flags wrong: %+v", got)
	}
	if got.Bid != 0.40 || got.BidSz != 12 {
		t.Fatalf("bid-only values wrong: %+v", got)
	}
	if got.Ask != 0 || got.AskSz != 0 {
		t.Fatalf("bid-only ask fields should be zero: %+v", got)
	}
	if _, ok := got.Mid(); ok {
		t.Fatal("one-sided book must have no mid")
	}

	// two-sided: both flags, all four fields parsed, mid = (bid+ask)/2.
	got = ParseBookTop(&core.BboView{Bid: "0.40", Ask: "0.44", BidSz: "12", AskSz: "8"})
	want := BookTop{Bid: 0.40, Ask: 0.44, BidSz: 12, AskSz: 8, HasBid: true, HasAsk: true}
	if got != want {
		t.Fatalf("two-sided = %+v, want %+v", got, want)
	}
	if mid, ok := got.Mid(); !ok || math.Abs(mid-0.42) > 1e-9 {
		t.Fatalf("two-sided mid = %v ok=%v, want 0.42", mid, ok)
	}

	// unparseable price strings leave the side absent (Has=false), sizes default 0.
	got = ParseBookTop(&core.BboView{Bid: "", Ask: "n/a", BidSz: "bad", AskSz: ""})
	if got.HasBid || got.HasAsk {
		t.Fatalf("unparseable prices must not set Has flags: %+v", got)
	}
	if got != (BookTop{}) {
		t.Fatalf("all-unparseable view should yield zero BookTop, got %+v", got)
	}
}

// TestTTL covers the ok=false branch (missing / unparseable expiry) plus the exact
// signed duration on both sides of expiry (a negative TTL still reports ok=true so
// callers can treat it as "past expiry").
func TestTTL(t *testing.T) {
	m := core.Market{Expiry: "2026-07-02 14:36Z"}

	// before expiry: positive, exact duration.
	now := time.Date(2026, 7, 2, 14, 6, 0, 0, time.UTC)
	if d, ok := TTL(m, now); !ok || d != 30*time.Minute {
		t.Fatalf("TTL before = %v ok=%v, want 30m,true", d, ok)
	}
	// after expiry: negative duration, still ok=true.
	past := time.Date(2026, 7, 2, 15, 36, 0, 0, time.UTC)
	if d, ok := TTL(m, past); !ok || d != -time.Hour {
		t.Fatalf("TTL after = %v ok=%v, want -1h,true", d, ok)
	}
	// missing expiry → ok=false, zero duration.
	if d, ok := TTL(core.Market{Expiry: ""}, now); ok || d != 0 {
		t.Fatalf("TTL empty = %v ok=%v, want 0,false", d, ok)
	}
	// unparseable expiry → ok=false.
	if d, ok := TTL(core.Market{Expiry: "not-a-date"}, now); ok || d != 0 {
		t.Fatalf("TTL garbage = %v ok=%v, want 0,false", d, ok)
	}
}

// TestClampProbEdges pins the band edges: values exactly on a boundary pass through
// unchanged, values just outside snap to the boundary, interior values are untouched.
func TestClampProbEdges(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{OutcomeMinPrice, OutcomeMinPrice}, // exactly on floor: unchanged
		{OutcomeMaxPrice, OutcomeMaxPrice}, // exactly on ceil: unchanged
		{0.0000001, OutcomeMinPrice},       // below floor: snap up
		{0.9999999, OutcomeMaxPrice},       // above ceil: snap down
		{-3, OutcomeMinPrice},              // far below
		{7, OutcomeMaxPrice},               // far above
		{0.5, 0.5},                         // interior: untouched
	}
	for _, c := range cases {
		if got := ClampProb(c.in); got != c.want {
			t.Errorf("ClampProb(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestIsYesClassification checks the Yes-default behavior around the side label:
// only an explicit (case-insensitive, trimmed) "No" is a No leg; empty / unknown
// labels default to Yes.
func TestIsYesClassification(t *testing.T) {
	yes := []string{"Yes", "yes", "YES", "", "  ", "maybe"}
	for _, s := range yes {
		if !IsYes(core.Market{Side: s}) {
			t.Errorf("IsYes(side=%q) = false, want true (Yes default)", s)
		}
	}
	no := []string{"No", "no", "NO", "  No  "}
	for _, s := range no {
		if IsYes(core.Market{Side: s}) {
			t.Errorf("IsYes(side=%q) = true, want false", s)
		}
	}
}

// TestCoinEncodingRoundTrip pins the "#<10*outcome+side>" encoding and the sibling
// flip for both a Yes and a No market across a couple of outcome ids.
func TestCoinEncodingRoundTrip(t *testing.T) {
	for _, o := range []int{0, 5, 641} {
		wantYes := "#" + itoa(10*o)
		wantNo := "#" + itoa(10*o+1)
		if YesCoin(o) != wantYes || NoCoin(o) != wantNo {
			t.Fatalf("outcome %d: yes=%s no=%s, want %s/%s", o, YesCoin(o), NoCoin(o), wantYes, wantNo)
		}
		// Sibling of a Yes market is the No coin; sibling of a No market is the Yes coin.
		if got := SiblingCoin(core.Market{Outcome: o, Side: "Yes"}); got != wantNo {
			t.Fatalf("SiblingCoin(yes,%d) = %s, want %s", o, got, wantNo)
		}
		if got := SiblingCoin(core.Market{Outcome: o, Side: "No"}); got != wantYes {
			t.Fatalf("SiblingCoin(no,%d) = %s, want %s", o, got, wantYes)
		}
	}
}

// TestInventoryNetSigns covers the signed exposure: long-Yes positive, long-No
// negative, balanced zero, and the empty inventory.
func TestInventoryNetSigns(t *testing.T) {
	if n := (Inventory{Yes: 30, No: 12}).Net(); n != 18 {
		t.Errorf("Net(30,12) = %d, want 18", n)
	}
	if n := (Inventory{Yes: 5, No: 20}).Net(); n != -15 {
		t.Errorf("Net(5,20) = %d, want -15", n)
	}
	if n := (Inventory{Yes: 9, No: 9}).Net(); n != 0 {
		t.Errorf("Net(9,9) = %d, want 0", n)
	}
	if n := (Inventory{}).Net(); n != 0 {
		t.Errorf("Net(zero) = %d, want 0", n)
	}
}

// TestFairZeroValue asserts the zero-value Fair is unusable (P==0 fails the in-band
// check) regardless of the clock.
func TestFairZeroValue(t *testing.T) {
	if (Fair{}).Valid(time.Now()) {
		t.Fatal("zero-value Fair must be invalid (P=0)")
	}
	// A zero ValidUntil means "no expiry set" — an otherwise good estimate stays
	// valid arbitrarily far in the future.
	f := Fair{P: 0.6, Conf: 0.9}
	if !f.Valid(time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("zero ValidUntil should not expire the estimate")
	}
}

// itoa is a tiny local base-10 int formatter for the encoding round-trip test,
// kept test-local to avoid depending on the source's strconv usage.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
