package hl

// Leaderboard reads the public Hyperliquid trader leaderboard. Unlike the rest of
// this package (which talks to the signed /exchange and the /info endpoints on
// api.hyperliquid.xyz), the leaderboard is served as a single static-ish JSON blob
// from Hyperliquid's own stats host — the exact source the official web app uses:
//
//	https://stats-data.hyperliquid.xyz/Mainnet/leaderboard
//	https://stats-data.hyperliquid.xyz/Testnet/leaderboard
//
// It is GET-only, takes no parameters, and returns every ranked address in one
// response, so it lives here as plain types + a parser rather than as a transport
// method. No third-party data source is involved — this is first-party HL data.

import (
	"encoding/json"
	"fmt"
)

// Public trader-leaderboard sources. These are Hyperliquid's own stats host — the
// exact endpoints the official web app reads — not a third-party aggregator.
const (
	MainnetLeaderboardURL = "https://stats-data.hyperliquid.xyz/Mainnet/leaderboard"
	TestnetLeaderboardURL = "https://stats-data.hyperliquid.xyz/Testnet/leaderboard"
)

// Leaderboard window names as they appear in the wire payload's tuple keys.
const (
	LeaderboardWindowDay     = "day"
	LeaderboardWindowWeek    = "week"
	LeaderboardWindowMonth   = "month"
	LeaderboardWindowAllTime = "allTime"
)

// LeaderboardWindows lists the windows the endpoint reports, in chronological order.
var LeaderboardWindows = []string{
	LeaderboardWindowDay,
	LeaderboardWindowWeek,
	LeaderboardWindowMonth,
	LeaderboardWindowAllTime,
}

// WindowPerformance is one trading window's performance for a leaderboard row.
// pnl/roi/vlm are kept as the exchange's own strings (roi is a fraction, e.g.
// "0.1234" = 12.34%; pnl/vlm are USD).
type WindowPerformance struct {
	Pnl string `json:"pnl"`
	Roi string `json:"roi"`
	Vlm string `json:"vlm"`
}

// LeaderboardRow is one ranked address. windowPerformances arrives on the wire as
// an array of [name, {pnl,roi,vlm}] tuples; it is decoded into Windows keyed by
// window name so a caller can look up any window directly.
type LeaderboardRow struct {
	EthAddress   string
	AccountValue string
	DisplayName  string // "" when the trader has no public display name
	Prize        string // contest prize; usually "0"
	Windows      map[string]WindowPerformance
}

// Window returns the performance for a named window (day|week|month|allTime),
// reporting whether it was present.
func (r LeaderboardRow) Window(name string) (WindowPerformance, bool) {
	wp, ok := r.Windows[name]
	return wp, ok
}

// UnmarshalJSON decodes the per-row wire shape, flattening the windowPerformances
// tuple array into the Windows map. prize and displayName tolerate either a
// string, a number, or null.
func (r *LeaderboardRow) UnmarshalJSON(data []byte) error {
	var raw struct {
		EthAddress         string              `json:"ethAddress"`
		AccountValue       string              `json:"accountValue"`
		DisplayName        json.RawMessage     `json:"displayName"`
		Prize              json.RawMessage     `json:"prize"`
		WindowPerformances [][]json.RawMessage `json:"windowPerformances"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.EthAddress = raw.EthAddress
	r.AccountValue = raw.AccountValue
	r.DisplayName = looseString(raw.DisplayName)
	r.Prize = looseString(raw.Prize)
	r.Windows = make(map[string]WindowPerformance, len(raw.WindowPerformances))
	for _, tuple := range raw.WindowPerformances {
		if len(tuple) < 2 {
			continue
		}
		var name string
		if err := json.Unmarshal(tuple[0], &name); err != nil || name == "" {
			continue
		}
		var wp WindowPerformance
		if err := json.Unmarshal(tuple[1], &wp); err != nil {
			continue
		}
		r.Windows[name] = wp
	}
	return nil
}

// looseString renders a JSON value that may be a string, a number, or null as a
// string ("" for null/absent). The leaderboard sends displayName as a string-or-null
// and prize as a number; this keeps both as clean strings without losing them.
func looseString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return ""
}

// Leaderboard is the parsed leaderboard payload.
type Leaderboard struct {
	Rows []LeaderboardRow `json:"leaderboardRows"`
}

// ParseLeaderboard decodes the raw stats-data leaderboard JSON body.
func ParseLeaderboard(body []byte) (*Leaderboard, error) {
	var lb Leaderboard
	if err := json.Unmarshal(body, &lb); err != nil {
		return nil, fmt.Errorf("failed to unmarshal leaderboard: %w", err)
	}
	if lb.Rows == nil {
		lb.Rows = []LeaderboardRow{}
	}
	return &lb, nil
}
