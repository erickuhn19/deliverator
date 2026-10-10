package papertrade

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// Account is one wallet frame from GET /state/user/live. Amounts are 18-decimal
// raw strings. Session key expiry is unix seconds.
type Account struct {
	Address                 string       `json:"address"`
	IndexedThroughBlock     int64        `json:"indexed_through_block"`
	IndexedThroughTimestamp int64        `json:"indexed_through_timestamp"`
	SessionKeys             []SessionKey `json:"session_keys"`
	Balance                 string       `json:"balance"`
	Queued                  string       `json:"queued"`
	Locked                  string       `json:"locked"`
	AvailableBalance        string       `json:"available_balance"`
	AvailableQueued         string       `json:"available_queued"`
	ConsumableDebt          string       `json:"consumable_debt"`
	OpenPositions           []Position   `json:"open_positions"`
	PaperBalance            string       `json:"paper_balance"`
	PaperStaked             string       `json:"paper_staked"`
}

// SessionKey is one registered trading key.
type SessionKey struct {
	Address    string `json:"address"`
	ExpiryUnix int64  `json:"expiry_unix"`
}

// Position is one isolated slug. MarginRaw is the collateral locked in it.
type Position struct {
	PositionID    string `json:"position_id"`
	InstrumentID  int    `json:"instrument_id"`
	IsLong        bool   `json:"is_long"`
	UseDebt       bool   `json:"use_debt"`
	EntryPriceRaw string `json:"entry_price_raw"`
	MarginRaw     string `json:"margin_raw"`
	Leverage      int    `json:"leverage"`
	BustPriceRaw  string `json:"bust_price_raw"`
	OpenedAtMs    int64  `json:"opened_at_ms"`
	IntentID      string `json:"intent_id"`
}

// AccountView adds the loaded agent key's readiness on top of Account.
type AccountView struct {
	Account
	SessionKey   string `json:"session_key,omitempty"`
	SessionReady *bool  `json:"session_ready,omitempty"`
}

// ParseAccount decodes a wallet frame. The live stream sends named fields;
// open positions and session keys may be objects or the venue's tuples.
func ParseAccount(body []byte) (Account, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return Account{}, fmt.Errorf("account frame: %w", err)
	}
	addr, _ := asString(raw["address"])
	block, _ := asInt64(raw["indexedThroughBlock"])
	ts, _ := asInt64(raw["indexedThroughTimestamp"])
	keys, err := parseSessionKeys(raw["sessionKeys"])
	if err != nil {
		return Account{}, err
	}
	positions, err := parsePositions(raw["openPositions"])
	if err != nil {
		return Account{}, err
	}
	funds, _ := raw["funds"].(map[string]any)
	paper, _ := raw["paper"].(map[string]any)
	avail, _ := asString(funds["availableBalance"])
	availQ, _ := asString(funds["availableQueued"])
	bal, _ := asString(raw["balance"])
	queued, _ := asString(raw["queued"])
	locked, _ := asString(raw["locked"])
	debt, _ := asString(raw["consumableDebt"])
	pBal, _ := asString(paper["balance"])
	pStaked, _ := asString(paper["staked"])
	return Account{
		Address: strings.ToLower(addr), IndexedThroughBlock: block, IndexedThroughTimestamp: ts,
		SessionKeys: keys, Balance: bal, Queued: queued, Locked: locked,
		AvailableBalance: avail, AvailableQueued: availQ, ConsumableDebt: debt,
		OpenPositions: positions, PaperBalance: pBal, PaperStaked: pStaked,
	}, nil
}

// SessionReady reports whether agent may trade against this frame. Expiry must
// sit more than 61 minutes past the later of now and the indexed timestamp.
func SessionReady(a Account, agent common.Address, nowUnix int64) bool {
	if agent == (common.Address{}) {
		return false
	}
	want := strings.ToLower(agent.Hex())
	floor := a.IndexedThroughTimestamp
	if nowUnix > floor {
		floor = nowUnix
	}
	need := floor + SessionSkewSeconds
	for _, k := range a.SessionKeys {
		if strings.EqualFold(k.Address, want) && k.ExpiryUnix > need {
			return true
		}
	}
	return false
}

// HasIntent reports whether the intent id appears on an open position. The
// account frame does not carry a full intent history, so a close or a queued
// intent can be absent here and still have landed.
func (a Account) HasIntent(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return false
	}
	for _, p := range a.OpenPositions {
		if strings.EqualFold(p.IntentID, id) {
			return true
		}
	}
	return false
}

// CoinNotionalUSD sums margin × leverage across open slugs in one instrument.
// A slug whose margin cannot be parsed fails closed.
func (a Account) CoinNotionalUSD(instrumentID int) (float64, error) {
	var sum float64
	for _, p := range a.OpenPositions {
		if p.InstrumentID != instrumentID {
			continue
		}
		raw, ok := new(big.Int).SetString(strings.TrimSpace(p.MarginRaw), 10)
		if !ok || p.Leverage <= 0 {
			return 0, fmt.Errorf("position %s has an unreadable margin", p.PositionID)
		}
		sum += notionalUSD(raw, p.Leverage)
	}
	return sum, nil
}

// CanSpend reports whether the frame can lock margin. useDebt spends the queued
// balance, and only up to consumableDebt. A missing figure fails closed.
func (a Account) CanSpend(margin *big.Int, useDebt bool) error {
	if margin == nil || margin.Sign() <= 0 {
		return errString("margin must be positive")
	}
	if !useDebt {
		avail, ok := new(big.Int).SetString(strings.TrimSpace(a.AvailableBalance), 10)
		if !ok {
			return errString("available balance is unreadable")
		}
		if avail.Cmp(margin) < 0 {
			return errString(fmt.Sprintf("available balance %s raw is below margin %s raw", avail.String(), margin.String()))
		}
		return nil
	}
	queued, okQ := new(big.Int).SetString(strings.TrimSpace(a.AvailableQueued), 10)
	debt, okD := new(big.Int).SetString(strings.TrimSpace(a.ConsumableDebt), 10)
	if !okQ || !okD {
		return errString("queued balance is unreadable")
	}
	if queued.Cmp(margin) < 0 || debt.Cmp(margin) < 0 {
		return errString(fmt.Sprintf("queued balance %s raw (consumable debt %s) is below margin %s raw", queued.String(), debt.String(), margin.String()))
	}
	return nil
}

// HasPosition reports whether an open slug has this id.
func (a Account) HasPosition(id string) bool {
	for _, p := range a.OpenPositions {
		if p.PositionID == id {
			return true
		}
	}
	return false
}

func parseSessionKeys(v any) ([]SessionKey, error) {
	if v == nil {
		return nil, nil
	}
	switch t := v.(type) {
	case map[string]any:
		var out []SessionKey
		for addr, exp := range t {
			unix, ok := asInt64(exp)
			if !ok {
				return nil, fmt.Errorf("session key %s expiry", addr)
			}
			out = append(out, SessionKey{Address: strings.ToLower(addr), ExpiryUnix: unix})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
		return out, nil
	case []any:
		var out []SessionKey
		for _, item := range t {
			switch row := item.(type) {
			case []any:
				if len(row) < 2 {
					return nil, fmt.Errorf("session key tuple")
				}
				addr, okA := asString(row[0])
				unix, okU := asInt64(row[1])
				if !okA || !okU {
					return nil, fmt.Errorf("session key tuple")
				}
				out = append(out, SessionKey{Address: strings.ToLower(addr), ExpiryUnix: unix})
			case map[string]any:
				addr, _ := asString(first(row, "address", "key"))
				unix, ok := asInt64(first(row, "expiry", "expiryUnix", "expiresAt"))
				if addr == "" || !ok {
					return nil, fmt.Errorf("session key object")
				}
				out = append(out, SessionKey{Address: strings.ToLower(addr), ExpiryUnix: unix})
			default:
				return nil, fmt.Errorf("session key entry")
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("sessionKeys")
	}
}

func parsePositions(v any) ([]Position, error) {
	if v == nil {
		return nil, nil
	}
	rows, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("openPositions")
	}
	out := make([]Position, 0, len(rows))
	for _, item := range rows {
		p, err := parsePosition(item)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func parsePosition(v any) (Position, error) {
	switch t := v.(type) {
	case []any:
		if len(t) != 10 {
			return Position{}, fmt.Errorf("position tuple wants 10 fields, got %d", len(t))
		}
		id, ok0 := asString(t[0])
		inst, ok1 := asInt(t[1])
		long, ok2 := asBool(t[2])
		debt, ok3 := asBool(t[3])
		entry, ok4 := asString(t[4])
		margin, ok5 := asString(t[5])
		lev, ok6 := asInt(t[6])
		bust, ok7 := asString(t[7])
		opened, ok8 := asInt64(t[8])
		intent, ok9 := asString(t[9])
		if !ok0 || !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || !ok7 || !ok8 || !ok9 {
			return Position{}, fmt.Errorf("position tuple types")
		}
		return Position{
			PositionID: id, InstrumentID: inst, IsLong: long, UseDebt: debt,
			EntryPriceRaw: entry, MarginRaw: margin, Leverage: lev, BustPriceRaw: bust,
			OpenedAtMs: opened, IntentID: strings.ToLower(intent),
		}, nil
	case map[string]any:
		id, _ := asString(first(t, "positionId", "position_id"))
		inst, okI := asInt(first(t, "instrumentId", "instrument_id"))
		long, okL := asBool(first(t, "isLong", "is_long"))
		debt, _ := asBool(first(t, "useDebt", "use_debt"))
		entry, _ := asString(first(t, "entryPriceRaw", "entry_price_raw"))
		margin, _ := asString(first(t, "marginRaw", "margin_raw"))
		lev, okLev := asInt(first(t, "leverage"))
		bust, _ := asString(first(t, "bustPriceRaw", "bust_price_raw"))
		opened, _ := asInt64(first(t, "openedAtMs", "opened_at_ms"))
		intent, _ := asString(first(t, "intentId", "intent_id"))
		if id == "" || !okI || !okL || !okLev || margin == "" {
			return Position{}, fmt.Errorf("position object")
		}
		return Position{
			PositionID: id, InstrumentID: inst, IsLong: long, UseDebt: debt,
			EntryPriceRaw: entry, MarginRaw: margin, Leverage: lev, BustPriceRaw: bust,
			OpenedAtMs: opened, IntentID: strings.ToLower(intent),
		}, nil
	default:
		return Position{}, fmt.Errorf("position entry")
	}
}

func first(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return nil
}

func asInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case json.Number:
		i, err := t.Int64()
		return i, err == nil
	case string:
		n := json.Number(strings.TrimSpace(t))
		i, err := n.Int64()
		return i, err == nil
	case float64:
		return int64(t), float64(int64(t)) == t
	default:
		return 0, false
	}
}
