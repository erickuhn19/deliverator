package papertrade

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestParseMargin(t *testing.T) {
	n, err := ParseMargin("1000")
	if err != nil {
		t.Fatal(err)
	}
	if n.String() != "1000000000000000000000" {
		t.Fatalf("1000 → %s", n)
	}
	half, err := ParseMargin("1000.5")
	if err != nil {
		t.Fatal(err)
	}
	if FormatRaw18(half) != "1000.5" {
		t.Fatalf("format %s", FormatRaw18(half))
	}
	if FormatRaw18(n) != "1000" {
		t.Fatalf("format whole %s", FormatRaw18(n))
	}
	tiny := big.NewInt(1)
	if FormatRaw18(tiny) != "0.000000000000000001" {
		t.Fatalf("format tiny %s", FormatRaw18(tiny))
	}
	for _, bad := range []string{"", "01", "1e3", ".5", "1.", "1.1234567890123456789", "-1", "1.0.0"} {
		if _, err := ParseMargin(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	padded, err := ParseMargin(" 10 ")
	if err != nil || FormatRaw18(padded) != "10" {
		t.Fatalf("trim: %v %v", padded, err)
	}
}

func TestParseMarkets(t *testing.T) {
	m, err := ParseMarkets([]byte(sampleTrading))
	if err != nil {
		t.Fatal(err)
	}
	if m.TradingPaused || m.MinimumMarginRaw != "10000000000000000000" {
		t.Fatalf("header %+v", m)
	}
	btc, ok := m.Find("btc")
	if !ok || !btc.Openable || btc.MaxLeverage != 1000 || btc.PriceScale != "10" {
		t.Fatalf("btc %+v ok=%v", btc, ok)
	}
	if !m.ActionEnabled("openPosition") || m.ActionEnabled("nope") {
		t.Fatalf("actions %+v", m.Intake.Actions)
	}
	paused := strings.Replace(sampleTrading, `"tradingPaused": false`, `"tradingPaused": true`, 1)
	pm, err := ParseMarkets([]byte(paused))
	if err != nil {
		t.Fatal(err)
	}
	if pm.ActionEnabled("openPosition") {
		t.Fatal("a paused book must fail closed")
	}
	bad := strings.Replace(sampleTrading, `true,false,false,false,false,null,null,null`, `true,true,false,false,false,null,null,null`, 1)
	if _, err := ParseMarkets([]byte(bad)); err == nil {
		t.Fatal("inconsistent lifecycle must fail the read")
	}
}

func TestParseAccountAndSpend(t *testing.T) {
	a, err := ParseAccount([]byte(sampleAccount))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.SessionKeys) != 2 {
		t.Fatalf("keys %+v", a.SessionKeys)
	}
	if a.SessionKeys[0].Address != "0x00000000000000000000000000000000000000aa" ||
		a.SessionKeys[1].Address != "0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266" {
		t.Fatalf("session map must be sorted lowercase: %+v", a.SessionKeys)
	}
	if len(a.OpenPositions) != 2 || a.OpenPositions[0].PositionID != "7" || a.OpenPositions[1].MarginRaw == "" {
		t.Fatalf("positions %+v", a.OpenPositions)
	}
	agent := common.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266")
	if !SessionReady(a, agent, 1_700_000_000) {
		t.Fatal("session should be ready")
	}
	if SessionReady(a, agent, 1_900_000_000) {
		t.Fatal("session inside the 61-minute skew must not be ready")
	}
	margin, _ := ParseMargin("1000")
	if err := a.CanSpend(margin, false); err != nil {
		t.Fatal(err)
	}
	tooMuch, _ := ParseMargin("1001")
	if err := a.CanSpend(tooMuch, false); err == nil {
		t.Fatal("over-balance must fail")
	}
	if err := a.CanSpend(margin, true); err == nil {
		t.Fatal("empty queued balance must fail")
	}
	usd, err := a.CoinNotionalUSD(0)
	if err != nil || usd != 10000 {
		t.Fatalf("coin notional %v %v", usd, err)
	}
	if !a.HasPosition("7") || !a.HasIntent("0xABC") || a.OpenPositions[1].IntentID != "0xdef" {
		t.Fatalf("lookup %+v", a.OpenPositions)
	}
}

func TestPlanOpen(t *testing.T) {
	m, err := ParseMarkets([]byte(sampleTrading))
	if err != nil {
		t.Fatal(err)
	}
	a, err := ParseAccount([]byte(sampleAccount))
	if err != nil {
		t.Fatal(err)
	}
	margin, _ := ParseMargin("1000")
	plan, err := PlanOpen(m, a, "BTC", true, margin, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Notional != "10000" || plan.NotionalUSD != 10000 || plan.CoinNotionalUSD != 20000 {
		t.Fatalf("plan %+v", plan)
	}
	if _, err := PlanOpen(m, a, "BTC", true, margin, 11, false); err != nil {
		t.Fatalf("venue max is 1000; leverage 11 is a Deliverator cap, not a venue reject: %v", err)
	}
	small, _ := ParseMargin("9")
	if _, err := PlanOpen(m, a, "BTC", true, small, 10, false); err == nil {
		t.Fatal("below minimum margin")
	}
	thin, _ := ParseMargin("100")
	if _, err := PlanOpen(m, a, "BTC", true, thin, 10, false); err == nil {
		t.Fatal("below minimum notional")
	}
	if _, err := PlanOpen(m, a, "DOGE", true, margin, 1, false); err == nil {
		t.Fatal("unknown market")
	}
}

const sampleTrading = `{
  "schemaVersion": 1,
  "indexedThroughBlock": 10,
  "tradingPaused": false,
  "minimumTradeSizeRaw": "10000000000000000000",
  "minimumOpenNotionalRaw": "10000000000000000000000",
  "accRewardPerShareRaw": "0",
  "instruments": [
    [0,"BTC",0,"10",true,false,false,false,false,null,null,null,"15000000000000000000000","814598000000000000000",1000,"10000000000000000000000000","476041580087574","100000000000000000","100000000000000000000000","1","2","3","4"]
  ],
  "intake": ["9","0","0",[]]
}`

const sampleAccount = `{
  "address": "0x0000000000000000000000000000000000000001",
  "indexedThroughBlock": 10,
  "indexedThroughTimestamp": 1700000000,
  "sessionKeys": {
    "0xF39Fd6e51aad88F6F4ce6aB8827279cffFb92266": 1800000000,
    "0x00000000000000000000000000000000000000aa": 1800000000
  },
  "balance": "1000000000000000000000",
  "queued": "0",
  "locked": "0",
  "funds": {"availableBalance": "1000000000000000000000", "availableQueued": "0"},
  "consumableDebt": "0",
  "paper": {"balance": "0", "staked": "0"},
  "openPositions": [
    ["7", 0, true, false, "1", "1000000000000000000000", 10, "2", 1700000000000, "0xabc"],
    {"positionId": "8", "instrumentId": 1, "isLong": false, "marginRaw": "1000000000000000000000", "leverage": 2, "intentId": "0xDEF"}
  ]
}`
