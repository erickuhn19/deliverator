package hl

import "testing"

const sampleLeaderboard = `{"leaderboardRows":[
  {"ethAddress":"0xAAA","accountValue":"125000.5","displayName":"whale","prize":3,
   "windowPerformances":[
     ["day",{"pnl":"1000","roi":"0.01","vlm":"500000"}],
     ["week",{"pnl":"5000","roi":"0.05","vlm":"2000000"}],
     ["month",{"pnl":"-2000","roi":"-0.02","vlm":"9000000"}],
     ["allTime",{"pnl":"50000","roi":"0.5","vlm":"80000000"}]]},
  {"ethAddress":"0xBBB","accountValue":"2000","displayName":null,"prize":0,
   "windowPerformances":[
     ["day",{"pnl":"-50","roi":"-0.025","vlm":"1000"}],
     ["allTime",{"pnl":"100","roi":"0.05","vlm":"10000"}]]}
]}`

func TestParseLeaderboard(t *testing.T) {
	lb, err := ParseLeaderboard([]byte(sampleLeaderboard))
	if err != nil {
		t.Fatal(err)
	}
	if len(lb.Rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(lb.Rows))
	}
	a := lb.Rows[0]
	if a.EthAddress != "0xAAA" || a.AccountValue != "125000.5" || a.DisplayName != "whale" {
		t.Fatalf("row[0] basics wrong: %+v", a)
	}
	// prize arrives as a JSON number; it should survive as a clean string.
	if a.Prize != "3" {
		t.Fatalf("prize should be %q, got %q", "3", a.Prize)
	}
	day, ok := a.Window(LeaderboardWindowDay)
	if !ok || day.Pnl != "1000" || day.Roi != "0.01" || day.Vlm != "500000" {
		t.Fatalf("day window wrong: %+v ok=%v", day, ok)
	}
	month, _ := a.Window(LeaderboardWindowMonth)
	if month.Pnl != "-2000" {
		t.Fatalf("month pnl wrong: %q", month.Pnl)
	}

	// null displayName -> "".
	if lb.Rows[1].DisplayName != "" {
		t.Fatalf("null displayName should be empty, got %q", lb.Rows[1].DisplayName)
	}
	// A window absent for a row is simply missing from the map.
	if _, ok := lb.Rows[1].Window(LeaderboardWindowWeek); ok {
		t.Fatal("0xBBB has no week window; lookup should report absent")
	}
}

func TestParseLeaderboardEmpty(t *testing.T) {
	lb, err := ParseLeaderboard([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if lb.Rows == nil || len(lb.Rows) != 0 {
		t.Fatalf("missing leaderboardRows should yield empty non-nil slice, got %#v", lb.Rows)
	}
	if _, err := ParseLeaderboard([]byte(`not json`)); err == nil {
		t.Fatal("invalid JSON should error")
	}
}
