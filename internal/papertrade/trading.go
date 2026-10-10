package papertrade

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Known intake actions. A name in the venue's disabled list is reported false.
// Names we have not heard of still surface when the venue disables them.
var intakeActions = []string{
	"registerSessionKey",
	"openPosition",
	"closePositions",
	"paperStake",
	"paperUnstake",
	"paperClaim",
	"registerDeposit",
	"withdrawToCore",
}

// Markets is the agent-facing shape of GET /state/trading. Raw integers stay
// decimal strings. price_scale is the venue's scale factor, not a USD divisor.
type Markets struct {
	SchemaVersion          int          `json:"schema_version"`
	IndexedThroughBlock    int64        `json:"indexed_through_block"`
	TradingPaused          bool         `json:"trading_paused"`
	MinimumMarginRaw       string       `json:"minimum_margin_raw"`
	MinimumOpenNotionalRaw string       `json:"minimum_open_notional_raw"`
	AccRewardPerShareRaw   string       `json:"acc_reward_per_share_raw"`
	Instruments            []Instrument `json:"instruments"`
	Intake                 Intake       `json:"intake"`
}

// Instrument is one market. Openable is active and not paused.
type Instrument struct {
	ID                     int    `json:"id"`
	Symbol                 string `json:"symbol"`
	AssetIndex             int    `json:"asset_index"`
	PriceScale             string `json:"price_scale"`
	Active                 bool   `json:"active"`
	Paused                 bool   `json:"paused"`
	Openable               bool   `json:"openable"`
	Retired                bool   `json:"retired"`
	CloseOnly              bool   `json:"close_only"`
	MaxLeverage            int    `json:"max_leverage"`
	MaxPositionNotionalRaw string `json:"max_position_notional_raw"`
	BaseRateRaw            string `json:"base_rate_raw"`
	RateMultiplierRaw      string `json:"rate_multiplier_raw"`
	PositionMultiplierRaw  string `json:"position_multiplier_raw"`
	ReferenceNotionalRaw   string `json:"reference_notional_raw"`
	BustBufferRaw          string `json:"bust_buffer_raw"`
	LongOIRaw              string `json:"long_oi_raw"`
	ShortOIRaw             string `json:"short_oi_raw"`
	LongCapRaw             string `json:"long_cap_raw"`
	ShortCapRaw            string `json:"short_cap_raw"`
}

// Intake says which relayer actions are currently accepted.
type Intake struct {
	Revision                  string          `json:"revision"`
	MinimumPaperStakeRaw      string          `json:"minimum_paper_stake_raw"`
	MaximumNetOpenInterestRaw string          `json:"maximum_net_open_interest_raw"`
	Actions                   map[string]bool `json:"actions"`
}

// ActionEnabled reports whether the relayer is accepting action. A missing
// action map fails closed.
func (m Markets) ActionEnabled(action string) bool {
	if m.TradingPaused {
		return false
	}
	if m.Intake.Actions == nil {
		return false
	}
	on, ok := m.Intake.Actions[action]
	return ok && on
}

// Find returns the instrument with this symbol (case-insensitive).
func (m Markets) Find(symbol string) (Instrument, bool) {
	for _, ins := range m.Instruments {
		if strings.EqualFold(ins.Symbol, strings.TrimSpace(symbol)) {
			return ins, true
		}
	}
	return Instrument{}, false
}

// ParseMarkets decodes the venue's positional /state/trading payload.
func ParseMarkets(body []byte) (Markets, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw struct {
		SchemaVersion          json.Number       `json:"schemaVersion"`
		IndexedThroughBlock    json.Number       `json:"indexedThroughBlock"`
		TradingPaused          bool              `json:"tradingPaused"`
		MinimumTradeSizeRaw    string            `json:"minimumTradeSizeRaw"`
		MinimumOpenNotionalRaw string            `json:"minimumOpenNotionalRaw"`
		AccRewardPerShareRaw   string            `json:"accRewardPerShareRaw"`
		Instruments            []json.RawMessage `json:"instruments"`
		Intake                 json.RawMessage   `json:"intake"`
	}
	if err := dec.Decode(&raw); err != nil {
		return Markets{}, fmt.Errorf("trading frame: %w", err)
	}
	block, err := raw.IndexedThroughBlock.Int64()
	if err != nil {
		return Markets{}, fmt.Errorf("indexedThroughBlock: %w", err)
	}
	out := Markets{
		SchemaVersion:          int(numOr(raw.SchemaVersion, 0)),
		IndexedThroughBlock:    block,
		TradingPaused:          raw.TradingPaused,
		MinimumMarginRaw:       raw.MinimumTradeSizeRaw,
		MinimumOpenNotionalRaw: raw.MinimumOpenNotionalRaw,
		AccRewardPerShareRaw:   raw.AccRewardPerShareRaw,
	}
	for i, rawIns := range raw.Instruments {
		ins, err := parseInstrument(rawIns)
		if err != nil {
			return Markets{}, fmt.Errorf("instrument %d: %w", i, err)
		}
		out.Instruments = append(out.Instruments, ins)
	}
	intake, err := parseIntake(raw.Intake)
	if err != nil {
		return Markets{}, err
	}
	out.Intake = intake
	return out, nil
}

func parseInstrument(raw json.RawMessage) (Instrument, error) {
	arr, err := decodeArray(raw)
	if err != nil {
		return Instrument{}, err
	}
	// Tuple: id, symbol, assetIndex, priceScale, active, paused, terminal,
	// retired, closeOnly, frozenBid, frozenAsk, pausedAt, rateMultiplier,
	// positionMultiplier, maxLeverage, maxPosition, bustBuffer, baseRate,
	// referenceNotional, longOI, shortOI, longCap, shortCap.
	if len(arr) != 23 {
		return Instrument{}, fmt.Errorf("want 23 fields, got %d", len(arr))
	}
	id, ok := asInt(arr[0])
	sym, okS := asString(arr[1])
	asset, okA := asInt(arr[2])
	scale, okSc := asString(arr[3])
	active, okAc := asBool(arr[4])
	paused, okP := asBool(arr[5])
	terminal, okT := asBool(arr[6])
	retired, okR := asBool(arr[7])
	closeOnly, okC := asBool(arr[8])
	if !ok || !okS || !okA || !okSc || !okAc || !okP || !okT || !okR || !okC {
		return Instrument{}, fmt.Errorf("header fields have the wrong types")
	}
	if sym == "" || sym != strings.TrimSpace(sym) {
		return Instrument{}, fmt.Errorf("symbol %q", sym)
	}
	// The venue client rejects a lifecycle combination that is not one of the
	// two consistent shapes. Fail closed rather than trade a market we cannot
	// classify.
	if terminal != paused || closeOnly != paused || retired != (paused || !active) {
		return Instrument{}, fmt.Errorf("lifecycle flags for %s are inconsistent", sym)
	}
	if terminal {
		if arr[9] == nil || arr[10] == nil || arr[11] == nil {
			return Instrument{}, fmt.Errorf("%s is terminal without a frozen book", sym)
		}
	} else if arr[9] != nil || arr[10] != nil || arr[11] != nil {
		return Instrument{}, fmt.Errorf("%s has a frozen book but is not terminal", sym)
	}
	rate, ok1 := asString(arr[12])
	posMul, ok2 := asString(arr[13])
	maxLev, ok3 := asInt(arr[14])
	maxPos, ok4 := asString(arr[15])
	bust, ok5 := asString(arr[16])
	base, ok6 := asString(arr[17])
	ref, ok7 := asString(arr[18])
	longOI, ok8 := asString(arr[19])
	shortOI, ok9 := asString(arr[20])
	longCap, ok10 := asString(arr[21])
	shortCap, ok11 := asString(arr[22])
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || !ok7 || !ok8 || !ok9 || !ok10 || !ok11 {
		return Instrument{}, fmt.Errorf("numeric fields for %s have the wrong types", sym)
	}
	return Instrument{
		ID: id, Symbol: sym, AssetIndex: asset, PriceScale: scale,
		Active: active, Paused: paused, Openable: active && !paused,
		Retired: retired, CloseOnly: closeOnly, MaxLeverage: maxLev,
		MaxPositionNotionalRaw: maxPos, BaseRateRaw: base, RateMultiplierRaw: rate,
		PositionMultiplierRaw: posMul, ReferenceNotionalRaw: ref, BustBufferRaw: bust,
		LongOIRaw: longOI, ShortOIRaw: shortOI, LongCapRaw: longCap, ShortCapRaw: shortCap,
	}, nil
}

func parseIntake(raw json.RawMessage) (Intake, error) {
	arr, err := decodeArray(raw)
	if err != nil {
		return Intake{}, fmt.Errorf("intake: %w", err)
	}
	if len(arr) != 4 {
		return Intake{}, fmt.Errorf("intake: want 4 fields, got %d", len(arr))
	}
	rev, ok1 := asString(arr[0])
	minStake, ok2 := asString(arr[1])
	maxOI, ok3 := asString(arr[2])
	if !ok1 || !ok2 || !ok3 {
		return Intake{}, fmt.Errorf("intake header has the wrong types")
	}
	disabled, err := decodeArray(mustRaw(arr[3]))
	if err != nil {
		return Intake{}, fmt.Errorf("intake actions: %w", err)
	}
	off := map[string]bool{}
	for _, d := range disabled {
		name, ok := asString(d)
		if !ok {
			return Intake{}, fmt.Errorf("intake action name has the wrong type")
		}
		off[name] = true
	}
	actions := map[string]bool{}
	for _, name := range intakeActions {
		actions[name] = !off[name]
	}
	for name := range off {
		if _, known := actions[name]; !known {
			actions[name] = false
		}
	}
	return Intake{
		Revision: rev, MinimumPaperStakeRaw: minStake, MaximumNetOpenInterestRaw: maxOI,
		Actions: actions,
	}, nil
}

func decodeArray(raw json.RawMessage) ([]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("expected an array")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var arr []any
	if err := dec.Decode(&arr); err != nil {
		return nil, err
	}
	return arr, nil
}

func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}

func asString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	default:
		return "", false
	}
}

func asBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case json.Number:
		i, err := t.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	case string:
		var n json.Number = json.Number(t)
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return int(i), true
	default:
		return 0, false
	}
}

func numOr(n json.Number, fallback int64) int64 {
	i, err := n.Int64()
	if err != nil {
		return fallback
	}
	return i
}
