package papertrade

import (
	"fmt"
	"math/big"

	"github.com/erickuhn19/deliverator/internal/output"
)

// OpenPlan is a venue-valid open that has not been signed. Risk caps that live
// in Deliverator config are applied by the caller on NotionalUSD.
type OpenPlan struct {
	Instrument      Instrument `json:"instrument"`
	MarginRaw       string     `json:"margin_raw"`
	Margin          string     `json:"margin"`
	Leverage        int        `json:"leverage"`
	IsLong          bool       `json:"is_long"`
	UseDebt         bool       `json:"use_debt"`
	NotionalUSD     float64    `json:"-"`
	Notional        string     `json:"notional"`
	CoinNotionalUSD float64    `json:"-"`
	margin          *big.Int
}

// PlanOpen checks the venue's own bounds: market open, margin, notional, and
// spendable balance. It does not look at Deliverator risk caps.
func PlanOpen(markets Markets, account Account, symbol string, isLong bool, margin *big.Int, leverage int, useDebt bool) (OpenPlan, error) {
	if !markets.ActionEnabled("openPosition") {
		return OpenPlan{}, output.Exchange("intake_action_paused", "Papertrade is not accepting opens right now").
			Retry().WithHint("retry after trading resumes; this is the venue, not a local risk cap")
	}
	ins, ok := markets.Find(symbol)
	if !ok {
		return OpenPlan{}, output.Validation("unknown_coin", "Papertrade has no market "+symbol).
			WithHint("run `deliverator papertrade markets` for the live list")
	}
	if !ins.Openable {
		return OpenPlan{}, output.Exchange("market_paused", ins.Symbol+" is not open for new positions").Retry()
	}
	if leverage < 1 || leverage > ins.MaxLeverage {
		return OpenPlan{}, output.Validation("leverage",
			fmt.Sprintf("leverage must be from 1 to %d on %s", ins.MaxLeverage, ins.Symbol))
	}
	if margin == nil || margin.Sign() <= 0 || margin.Cmp(oneRaw128) >= 0 {
		return OpenPlan{}, output.Validation("margin", "margin must be a positive amount below 2^128 raw units")
	}
	if cmp, ok := cmpRaw(margin, markets.MinimumMarginRaw); !ok {
		return OpenPlan{}, output.Network("trading_state", "minimum margin from the venue is unreadable").Retry()
	} else if cmp < 0 {
		return OpenPlan{}, output.Validation("below_minimum",
			fmt.Sprintf("margin %s is below the venue minimum %s", FormatRaw18(margin), FormatRaw18(rawInt(markets.MinimumMarginRaw)))).
			WithHint("Papertrade's minimum margin is 10 USDC")
	}
	notionalRaw := new(big.Int).Mul(margin, big.NewInt(int64(leverage)))
	if cmp, ok := cmpRaw(notionalRaw, markets.MinimumOpenNotionalRaw); !ok {
		return OpenPlan{}, output.Network("trading_state", "minimum notional from the venue is unreadable").Retry()
	} else if cmp < 0 {
		return OpenPlan{}, output.Validation("below_minimum_notional",
			fmt.Sprintf("notional %s (margin × leverage) is below the venue minimum %s", FormatRaw18(notionalRaw), FormatRaw18(rawInt(markets.MinimumOpenNotionalRaw)))).
			WithHint("Papertrade's minimum position notional is 10000 USD")
	}
	if cmp, ok := cmpRaw(notionalRaw, ins.MaxPositionNotionalRaw); !ok {
		return OpenPlan{}, output.Network("trading_state", "max position notional from the venue is unreadable").Retry()
	} else if cmp > 0 {
		return OpenPlan{}, output.Risk("max_position_notional",
			fmt.Sprintf("notional %s exceeds %s's venue cap %s", FormatRaw18(notionalRaw), ins.Symbol, FormatRaw18(rawInt(ins.MaxPositionNotionalRaw)))).
			WithHint("reduce margin or leverage")
	}
	if err := account.CanSpend(margin, useDebt); err != nil {
		return OpenPlan{}, output.Validation("insufficient_margin", err.Error()).
			WithHint("fund the deposit proxy, or pass --use-debt to spend queued winnings")
	}
	coin, err := account.CoinNotionalUSD(ins.ID)
	if err != nil {
		return OpenPlan{}, output.Network("account_state", err.Error()).Retry()
	}
	order := notionalUSD(margin, leverage)
	return OpenPlan{
		Instrument: ins, MarginRaw: margin.String(), Margin: FormatRaw18(margin),
		Leverage: leverage, IsLong: isLong, UseDebt: useDebt,
		NotionalUSD: order, Notional: FormatRaw18(notionalRaw),
		CoinNotionalUSD: coin + order, margin: margin,
	}, nil
}

func rawInt(decimal string) *big.Int {
	n, ok := new(big.Int).SetString(decimal, 10)
	if !ok {
		return big.NewInt(0)
	}
	return n
}
