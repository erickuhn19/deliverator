package hl

// SECURITY-SENSITIVE — ANTI-TAMPER GUARD.
// The golden (r,s,v) signature vectors and frozen wire bytes below are the
// canary that proves Deliverator signs EXACTLY the action it shows the user.
// A change here that is not a deliberate, reviewed protocol update is a red flag:
// it can silently alter what gets signed on a real-money account. Any edit to a
// golden value REQUIRES maintainer review and an explanation of why the old bytes
// were wrong. Never "update" a vector just to make a failing test pass.
// The 0x… 64-hex literals here are deterministic THROWAWAY test keys, never real
// secrets (see .gitleaks.toml allowlist).

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/vmihailenco/msgpack/v5"
)

// Permanent, SDK-independent parity guards. These pin the wire format and the
// full signing pipeline to frozen golden values captured while the reference
// SDK differential tests (build tag `difftest`) were green, so parity holds
// after the SDK dependency is removed.

const goldenKeyHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

const goldenNonce int64 = 1700000000000

func rawMsgpack(t *testing.T, action any) string {
	t.Helper()
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	enc.UseCompactInts(true)
	if err := enc.Encode(action); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return hex.EncodeToString(convertStr16ToStr8(buf.Bytes()))
}

// TestGoldenOrderMsgpack pins raw order msgpack to the Python SDK golden hex.
func TestGoldenOrderMsgpack(t *testing.T) {
	const pythonHex = "83a474797065a56f72646572a66f72646572739186a16100a162c3a170a53430303030a173a5302e303031a172c2a17481a56c696d697481a3746966a3477463a867726f7570696e67a26e61"
	action := OrderAction{
		Type:     "order",
		Orders:   []OrderWire{{Asset: 0, IsBuy: true, LimitPx: "40000", Size: "0.001", OrderType: OrderWireType{Limit: &OrderWireTypeLimit{Tif: TifGtc}}}},
		Grouping: "na",
	}
	if got := rawMsgpack(t, action); got != pythonHex {
		t.Fatalf("order msgpack mismatch:\n got:    %s\n golden: %s", got, pythonHex)
	}
}

// TestCanonicalSignatureVector pins the full L1 pipeline (msgpack -> action hash
// -> EIP-712 -> ECDSA) for a canonical order with a fixed key/nonce.
func TestCanonicalSignatureVector(t *testing.T) {
	key, err := crypto.HexToECDSA(goldenKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	action := OrderAction{
		Type:     "order",
		Orders:   []OrderWire{{Asset: 1, IsBuy: true, LimitPx: "65000.5", Size: "0.25", OrderType: OrderWireType{Limit: &OrderWireTypeLimit{Tif: TifGtc}}}},
		Grouping: "na",
	}
	sig, err := SignL1Action(key, action, "", goldenNonce, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	const (
		wantR = "0x5129961a53e6c460d772abce1fd07527e47c5d9ce43fad40236882464873fcc8"
		wantS = "0x6285c9828a09a5673ca8fe97713acd84a3f9eb5aad72bf47108416625635952d"
		wantV = 28
	)
	if sig.R != wantR || sig.S != wantS || sig.V != wantV {
		t.Fatalf("signature vector drift:\n got  r=%s s=%s v=%d", sig.R, sig.S, sig.V)
	}
}

// vectorActions returns one representative action per family, including the
// str16->str8 (cloid) paths and TWAP, signed at a fixed key/nonce/mainnet.
func vectorActions() []struct {
	name   string
	action any
} {
	cl := "0x0000000000000000000000000000abcd"
	schedTime := goldenNonce + 60000
	return []struct {
		name   string
		action any
	}{
		{"order_gtc", OrderAction{Type: "order", Orders: []OrderWire{{Asset: 1, IsBuy: true, LimitPx: "65000.5", Size: "0.25", OrderType: OrderWireType{Limit: &OrderWireTypeLimit{Tif: TifGtc}}}}, Grouping: "na"}},
		{"order_cloid", OrderAction{Type: "order", Orders: []OrderWire{{Asset: 2, IsBuy: false, LimitPx: "100", Size: "3", ReduceOnly: true, OrderType: OrderWireType{Limit: &OrderWireTypeLimit{Tif: TifGtc}}, Cloid: &cl}}, Grouping: "na"}},
		{"order_trigger_sl_market", OrderAction{Type: "order", Orders: []OrderWire{{Asset: 0, IsBuy: false, LimitPx: "60000", Size: "0.1", OrderType: OrderWireType{Trigger: &OrderWireTypeTrigger{IsMarket: true, TriggerPx: "61000", Tpsl: StopLoss}}}}, Grouping: "na"}},
		// Revenue-critical: an order carrying a builder {b,f}. Pins the builder
		// submap into the signed msgpack (field order, tags, compact-int fee).
		{"order_with_builder", OrderAction{Type: "order", Orders: []OrderWire{{Asset: 1, IsBuy: true, LimitPx: "65000.5", Size: "0.25", OrderType: OrderWireType{Limit: &OrderWireTypeLimit{Tif: TifGtc}}}}, Grouping: "na", Builder: &BuilderInfo{Builder: "0xcafef00dcafef00dcafef00dcafef00dcafef00d", Fee: 10}}},
		{"cancel", CancelAction{Type: "cancel", Cancels: []CancelOrderWire{{Asset: 1, OrderID: 123456789}}}},
		{"cancel_by_cloid", CancelByCloidAction{Type: "cancelByCloid", Cancels: []CancelByCloidWire{{Asset: 3, ClientID: cl}}}},
		{"modify_by_cloid", ModifyAction{Type: "modify", Oid: cl, Order: OrderWire{Asset: 1, IsBuy: true, LimitPx: "64000", Size: "0.5", OrderType: OrderWireType{Limit: &OrderWireTypeLimit{Tif: TifGtc}}}}},
		{"update_leverage_iso", UpdateLeverageAction{Type: "updateLeverage", Asset: 4, IsCross: false, Leverage: 5}},
		{"update_isolated_margin_remove", UpdateIsolatedMarginAction{Type: "updateIsolatedMargin", Asset: 4, IsBuy: true, Ntli: -50000000}},
		{"schedule_cancel_set", ScheduleCancelAction{Type: "scheduleCancel", Time: &schedTime}},
		{"twap_order", TwapOrderAction{Type: "twapOrder", Twap: TwapWire{Asset: 1, IsBuy: true, Size: "2", Minutes: 30}}},
		{"set_referrer", SetReferrerAction{Type: "setReferrer", Code: "DELIVERATOR"}},
		{"twap_cancel", TwapCancelAction{Type: "twapCancel", Asset: 1, TwapID: 555}},
		// HIP-4 outcome order: asset id >= 100_000_000 crosses the msgpack compact-int
		// boundary into uint32 (0xce) encoding that no perp/spot vector (asset 0-4)
		// exercises in the signed bytes. Price is a probability in (0,1), size integer.
		{"order_outcome", OrderAction{Type: "order", Orders: []OrderWire{{Asset: OutcomeAsset(641, 1), IsBuy: true, LimitPx: "0.97", Size: "10", OrderType: OrderWireType{Limit: &OrderWireTypeLimit{Tif: TifGtc}}}}, Grouping: "na"}},
		// batchModify with both an oid target and a cloid target (live-verified on
		// mainnet — pins the new signing surface against future drift).
		{"batch_modify", BatchModifyAction{Type: "batchModify", Modifies: []ModifyWire{
			{Oid: int64(123456789), Order: OrderWire{Asset: 1, IsBuy: true, LimitPx: "64000", Size: "0.5", OrderType: OrderWireType{Limit: &OrderWireTypeLimit{Tif: TifGtc}}}},
			{Oid: cl, Order: OrderWire{Asset: 2, IsBuy: false, LimitPx: "100", Size: "3", ReduceOnly: true, OrderType: OrderWireType{Limit: &OrderWireTypeLimit{Tif: TifAlo}}, Cloid: &cl}},
		}}},
		// Order PRIORITY fee: identical to order_gtc EXCEPT grouping is the {"p":rate}
		// object instead of the "na" string — so its (r,s,v) MUST differ, proving the
		// priority grouping entered the signed bytes (the unpinned priority branch).
		{"order_priority", OrderAction{Type: "order", Orders: []OrderWire{{Asset: 1, IsBuy: true, LimitPx: "65000.5", Size: "0.25", OrderType: OrderWireType{Limit: &OrderWireTypeLimit{Tif: TifGtc}}}}, Grouping: PriorityGrouping(20000)}},
	}
}

// TestActionSignatureVectors pins the full signing pipeline for every action
// family to frozen golden (r,s,v), captured while the SDK differential tests
// were green. This is the permanent parity guard after the SDK is removed.
func TestActionSignatureVectors(t *testing.T) {
	key, err := crypto.HexToECDSA(goldenKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	golden := map[string][3]any{
		"order_gtc":                     {"0x5129961a53e6c460d772abce1fd07527e47c5d9ce43fad40236882464873fcc8", "0x6285c9828a09a5673ca8fe97713acd84a3f9eb5aad72bf47108416625635952d", 28},
		"order_cloid":                   {"0xcef2baae79bcd03314d8b5cb3fa88bf6200c965866dc2080ebbf5a8e07ffdd89", "0x6aab504bd0f7a3ab19ea3993026aa64548963596aef27aa1798871b4be23a679", 28},
		"order_trigger_sl_market":       {"0x1f6da2e8b3b487fbb0d395c2b3785f6188c928d8c645874005ea3ffae6247007", "0x27819c8ef48fcfb881a194d32dcc3739eebe88bca360f6493b4ab56bc5957aba", 28},
		"cancel":                        {"0x3393d685004131a0ad65a65eef078cb2a0f4257003d86f88981b726afe72220c", "0x299f388f48470ce78b5b3cfbff60b5c5b0a6da2332b2ae1ff9056e4b1786cc90", 28},
		"cancel_by_cloid":               {"0x84b84c4f03ac677d6105f2a1706373a96a1dd0f36174df1e729421a801f1330", "0x9d7a716c6a9711d8912f519af8056bd81860bf6427c6579ec78040b5bf473d3", 27},
		"modify_by_cloid":               {"0xc72fa9ccdfa49a31ad686496bb4219207200165cf3fd146f20ac6d1f9c3bf842", "0x25694294ef33fc06ae49bb337ef7a19910cd75f2a811fb3d3fc5c97d0a2a2478", 27},
		"update_leverage_iso":           {"0x6815e4aa5a9b435c83656d25313e4b53f4909fff4ada1476db5c3852d6f37118", "0x351bb70a6801325523749845ace570d01aeb53ab8cc8563dcd99587033ed8dda", 27},
		"update_isolated_margin_remove": {"0x43440644a2c5f989e4a01656176773d4bec40c92827fd8bcae36868cb5a15682", "0x7ecf718b1a90f6fd56bfcae1cadbdb22555dbc4138333f33b3f34ce3e7b630f5", 28},
		"schedule_cancel_set":           {"0xdbf96b34c26f8c70e3115d2ab23d50994854ecf546104551b99519a282029331", "0x14e27f7822a0e476269ddc8249776e30f43d6ef440229f0bf7a32351e6c122ec", 28},
		"twap_order":                    {"0x3ec803da00071f6ce1e3bd5389e210e046ad4f0457569d0a206b0c08cb615aa5", "0x3aaef1c3bada5b46cd39346bfd26f25ce1cebb6a6fdc9e301bfa09a77a5b06da", 27},
		"set_referrer":                  {"0x74e2df6628f193dc28542a9582d10e9f3a116bb9501b815a93d306689f17376", "0x5517060cfbe3ece957d6f3db063a83d420189f153485426a62e8fc187d7427f1", 28},
		"order_with_builder":            {"0x4a18eab5247ae7886f7cb78744b010fa779bd894d145d8d3a0dd39238dcdbcd", "0x7f7785c77c23472e5affb81ea4470962a299af1195409e0ab8c859cab638fca0", 27},
		"batch_modify":                  {"0x5f355597c68fd3a1bf0f77eb94a42468ef879f8cf5a70b9397cf8a01d1272bb9", "0x5a8445d4ea9c3517a4fbfa83da3bcbfa247084325668f4069ffbd4997246cf2c", 28},
		"twap_cancel":                   {"0x668aaa9f4c6d41d10861ef6dc8668010a44222807e7c4cc563caa14b2cadac26", "0x5d4931d542ba96ca68f1c444815c8564e9021fcae3db3dfbc4477967af7da1f0", 27},
		"order_outcome":                 {"0x3bf526e930012d5e20e5a79766588167e05e5a6a822c64dbc90aca30a745733b", "0x527fe07cfbdc44f1d1214085333bbf4093548634f56223e9f8fd79edd9bfae71", 27},
		"order_priority":                {"0xa5f44914d657c559a7f7c0d4074fa93624b9dce8d3457f5f0eda242286901325", "0x3068eb1e8d0799efeeaaa14d8c6b1bf33160ad298693491b577f82807de34492", 27},
	}
	for _, c := range vectorActions() {
		sig, err := SignL1Action(key, c.action, "", goldenNonce, nil, true)
		if err != nil {
			t.Fatalf("%s: sign: %v", c.name, err)
		}
		want := golden[c.name]
		if sig.R != want[0] || sig.S != want[1] || sig.V != want[2] {
			t.Errorf("%s vector drift: got r=%s s=%s v=%d", c.name, sig.R, sig.S, sig.V)
		}
	}
}

func TestFloatToWire(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{40000, "40000"},
		{0.001, "0.001"},
		{65000.5, "65000.5"},
		{0, "0"},
		{-0, "0"},
		{1.23456789, "1.23456789"},
		{100, "100"},
	}
	for _, c := range cases {
		got, err := floatToWire(c.in)
		if err != nil {
			t.Errorf("floatToWire(%v): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("floatToWire(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	// More than 8 significant decimals must error (caller must pre-round).
	if _, err := floatToWire(1.123456789); err == nil {
		t.Errorf("floatToWire(1.123456789): expected rounding error, got nil")
	}
}

func TestRoundToSignificantFigures(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{65000.5, 65000}, // integer part already has 5 digits -> returned whole (floored)
		{123456, 123456}, // >5 integer digits -> returned whole
		{0.0012345, 0.0012345},
		{1.23456, 1.2346},
		{0, 0},
	}
	for _, c := range cases {
		if got := roundToSignificantFigures(c.in, 5); got != c.want {
			t.Errorf("roundToSignificantFigures(%v,5) = %v, want %v", c.in, got, c.want)
		}
	}
}

// actionHash must return an error — never panic — on a negative nonce (a
// pre-1970 host clock) or negative expiresAfter, so a crash mid-sign can't escape
// the JSON-envelope / exit-code contract with a raw stack trace (audit #91 / S6).
// The well-formed inputs the golden vectors use must still hash cleanly.
func TestActionHashRejectsBadInputs(t *testing.T) {
	if _, err := actionHash(canonicalOrder(), "", -1, nil); err == nil {
		t.Error("negative nonce must return an error, not panic")
	}
	neg := int64(-5)
	if _, err := actionHash(canonicalOrder(), "", goldenNonce, &neg); err == nil {
		t.Error("negative expiresAfter must return an error, not panic")
	}
	if _, err := actionHash(canonicalOrder(), "", goldenNonce, nil); err != nil {
		t.Errorf("a well-formed action must hash without error: %v", err)
	}
}
