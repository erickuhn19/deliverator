package core

import "testing"

// payload() must serialize exactly the fields a subscription sets — in
// particular activeAssetData needs BOTH coin and user in one subscription.
func TestStreamSubPayload(t *testing.T) {
	// coin + user together (activeAssetData)
	p := StreamSub{Type: ChanActiveAssetData, Coin: "BTC", User: "0xabc"}.payload()
	if p["type"] != ChanActiveAssetData || p["coin"] != "BTC" || p["user"] != "0xabc" {
		t.Fatalf("activeAssetData payload must carry type+coin+user: %v", p)
	}

	// user-only (webData2 / twap slices) — no coin/interval keys
	p = StreamSub{Type: ChanWebData2, User: "0xabc"}.payload()
	if p["user"] != "0xabc" {
		t.Fatalf("webData2 payload must carry user: %v", p)
	}
	if _, ok := p["coin"]; ok {
		t.Errorf("user-only payload must omit coin: %v", p)
	}

	// coin-only — no user key
	p = StreamSub{Type: ChanL2Book, Coin: "ETH"}.payload()
	if _, ok := p["user"]; ok {
		t.Errorf("coin-only payload must omit user: %v", p)
	}

	// nSigFigs only included when > 0
	noFigs := StreamSub{Type: ChanL2Book, Coin: "ETH"}.payload()
	if _, ok := noFigs["nSigFigs"]; ok {
		t.Error("nSigFigs 0 must be omitted")
	}
	withFigs := StreamSub{Type: ChanL2Book, Coin: "ETH", NSigFigs: 5}.payload()
	if withFigs["nSigFigs"] != 5 {
		t.Error("nSigFigs > 0 must be included")
	}
}

func TestNewStreamChannelConstants(t *testing.T) {
	cases := map[string]string{
		ChanWebData2:           "webData2",
		ChanActiveAssetData:    "activeAssetData",
		ChanUserTwapSliceFills: "userTwapSliceFills",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("channel constant = %q, want %q (must match the HL wire type exactly)", got, want)
		}
	}
}
