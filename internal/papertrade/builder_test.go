package papertrade

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestBuilderSelectorsMatchDispatchers(t *testing.T) {
	cases := []struct{ sig, want string }{
		{"builderRegistry()", selBuilderRegistry},
		{"feeRate()", selFeeRate},
		{"isApproved(address)", selIsApproved},
		{"builderFees(address)", selBuilderFees},
		{"claimBuilderFees()", selClaimBuilderFees},
		{"sessionKeys(address,address)", selSessionKeys},
	}
	for _, c := range cases {
		got := hex.EncodeToString(crypto.Keccak256([]byte(c.sig))[:4])
		if got != c.want {
			t.Fatalf("%s selector %s, dispatcher constant %s", c.sig, got, c.want)
		}
	}
	if hex.EncodeToString(ClaimBuilderFeesData()) != selClaimBuilderFees {
		t.Fatal("claim calldata drifted from claimBuilderFees()")
	}
}

func TestFormatFeeRate(t *testing.T) {
	if FormatFeeRate(big.NewInt(5_000_000_000_000_000)) != "0.5%" {
		t.Fatalf("live rate %s", FormatFeeRate(big.NewInt(5_000_000_000_000_000)))
	}
	cap, _ := new(big.Int).SetString(BuilderFeeRateCapRaw, 10)
	if FormatFeeRate(cap) != "1%" {
		t.Fatalf("cap %s", FormatFeeRate(cap))
	}
	if FormatFeeRate(big.NewInt(0)) != "0%" || FormatFeeRate(nil) != "" {
		t.Fatal("zero and nil")
	}
}

func TestResolveBuilder(t *testing.T) {
	configured := "0x0000000000000000000000000000000000000001"
	zero, err := ResolveBuilder("", "")
	if err != nil || zero != (common.Address{}) {
		t.Fatalf("default must be no builder: %v %s", err, zero.Hex())
	}
	// The Hyperliquid builder config must not leak in. An empty Papertrade
	// field stays empty even if some other address exists.
	got, err := ResolveBuilder("", configured)
	if err != nil || got != common.HexToAddress(configured) {
		t.Fatalf("config %v %s", err, got.Hex())
	}
	got, err = ResolveBuilder("none", configured)
	if err != nil || got != (common.Address{}) {
		t.Fatalf("explicit none must opt out: %v %s", err, got.Hex())
	}
	got, err = ResolveBuilder("0x0000000000000000000000000000000000000000", configured)
	if err != nil || got != (common.Address{}) {
		t.Fatalf("explicit zero must opt out: %v %s", err, got.Hex())
	}
	if _, err := ResolveBuilder("0x123", ""); err == nil {
		t.Fatal("short address accepted")
	}
	if msg := FeeWarning(common.Address{}, "0.5%"); msg != "" {
		t.Fatal("zero builder must not warn")
	}
	if !strings.Contains(FeeWarning(gotOr(configured), "0.5%"), "0.5%") {
		t.Fatal("warning dropped the live rate")
	}
}

func gotOr(s string) common.Address { return common.HexToAddress(s) }

func TestNormalizeApplication(t *testing.T) {
	app, err := NormalizeApplication("  Deliverator  ", "https://deliverator.net/trade", "email", "Ops@Example.com", "Routes session keys.\r\n\r\n\r\nSecond paragraph.")
	if err != nil {
		t.Fatal(err)
	}
	if app.AppName != "Deliverator" || app.Contact != "ops@example.com" || app.ContactKind != "email" {
		t.Fatalf("%+v", app)
	}
	if !strings.Contains(app.Integration, "Routes session keys.\n\nSecond paragraph.") {
		t.Fatalf("integration %q", app.Integration)
	}
	app, err = NormalizeApplication("Bot", "https://example.com", "telegram", "@paper_bot", "Signs feeAddress.")
	if err != nil || app.Contact != "paper_bot" {
		t.Fatalf("telegram %+v %v", app, err)
	}
	// ﬁ (U+FB01) is NFKC "fi". The venue client normalizes before signing.
	app, err = NormalizeApplication("ﬁ", "https://example.com", "x", "@ab", "ok")
	if err != nil || app.AppName != "fi" {
		t.Fatalf("nfkc %+v %v", app, err)
	}
	for _, bad := range []struct{ name, site, kind, contact, text, field string }{
		{"", "https://example.com", "email", "a@b.co", "x", "appName"},
		{"n", "http://example.com", "email", "a@b.co", "x", "websiteUrl"},
		{"n", "https://example.com", "discord", "a@b.co", "x", "contactKind"},
		{"n", "https://example.com", "email", "not-an-email", "x", "contact"},
		{"n", "https://example.com", "email", "a@b.co", "", "integration"},
		{"n", "https://example.com", "email", "a@b.co", "has\u0000nul", "integration"},
	} {
		_, err := NormalizeApplication(bad.name, bad.site, bad.kind, bad.contact, bad.text)
		var fe *FieldError
		if err == nil || !errorAs(err, &fe) || fe.Field != bad.field {
			t.Fatalf("%s: %v", bad.field, err)
		}
	}
}

func errorAs(err error, target **FieldError) bool {
	fe, ok := err.(*FieldError)
	if ok {
		*target = fe
	}
	return ok
}

func TestApplicationMessageAndSignature(t *testing.T) {
	key, err := crypto.HexToECDSA(goldenKey)
	if err != nil {
		t.Fatal(err)
	}
	app := BuilderApplication{
		AppName: "Deliverator", WebsiteURL: "https://deliverator.net",
		ContactKind: "email", Contact: "ops@example.com", Integration: "Session keys.",
	}
	addr := "0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266"
	msg := ApplicationMessage(addr, BuilderApplicationEnvironment, app, 1700000000000)
	want := strings.Join([]string{
		"Papertrade builder application",
		"Address: " + addr,
		"Environment: production",
		"App: Deliverator",
		"Website: https://deliverator.net",
		"Contact: email ops@example.com",
		"Timestamp: 1700000000000",
		"",
		"Integration:",
		"Session keys.",
		"",
		"This signature only submits a builder application. It moves no funds and approves nothing.",
	}, "\n")
	if msg != want {
		t.Fatalf("message\n%s", msg)
	}
	sig, err := SignPersonal(key, msg)
	if err != nil {
		t.Fatal(err)
	}
	raw := common.FromHex(sig)
	if len(raw) != 65 || raw[64] < 27 {
		t.Fatalf("sig %s", sig)
	}
	raw[64] -= 27
	pub, err := crypto.SigToPub(accounts.TextHash([]byte(msg)), raw)
	if err != nil {
		t.Fatal(err)
	}
	if crypto.PubkeyToAddress(*pub) != crypto.PubkeyToAddress(key.PublicKey) {
		t.Fatal("personal sign did not recover the builder")
	}
}

func TestParseBuilderDocuments(t *testing.T) {
	board, err := ParseLeaderboard([]byte(`{
		"ok": true,
		"registry": "0xFdc6550436Dd75Ce8a0B145f935B7D3A64F53248",
		"feeRateRaw": "5000000000000000",
		"asOfMs": 1792781672494,
		"totals": {"builders": 1, "users": 2, "closes": 3, "notionalRaw": "0", "notionalDayRaw": "0", "accruedRaw": "9"},
		"builders": [{"address": "0x0000000000000000000000000000000000000001", "name": "App", "users": 2, "closes": 3, "notionalRaw": "0", "notionalDayRaw": "0", "accruedRaw": "9"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if board.Registry != "0xfdc6550436dd75ce8a0b145f935b7d3a64f53248" || board.FeeRateRaw != "5000000000000000" || board.Builders != 1 || board.Rows[0].Name != "App" {
		t.Fatalf("%+v", board)
	}
	empty, err := ParseLeaderboard([]byte(`{"ok":true,"registry":null,"feeRateRaw":null,"asOfMs":1,"totals":{"builders":0,"users":0,"closes":0,"notionalRaw":"0","notionalDayRaw":"0","accruedRaw":"0"},"builders":[]}`))
	if err != nil || empty.Registry != "" || empty.FeeRateRaw != "" {
		t.Fatalf("null registry %+v %v", empty, err)
	}
	stats, err := ParseBuilderStats([]byte(`{
		"ok": true, "address": "0x0000000000000000000000000000000000000001", "approved": false, "unclaimedRaw": "0",
		"routing": {"users": 0, "sessionKeys": 0, "closes": 0, "notionalRaw": "0", "asOfMs": 5, "daily": []},
		"earnings": {"accruedRaw": "1", "claimedRaw": "2", "paidRaw": "3", "daily": []}
	}`))
	if err != nil || !stats.HasEarnings || stats.PaidRaw != "3" || stats.Approved {
		t.Fatalf("%+v %v", stats, err)
	}
	app, err := ParseBuilderApplication([]byte(`{"ok":true,"address":"0x0000000000000000000000000000000000000001","status":null,"submittedAtMs":null}`))
	if err != nil || !app.StatusKnown || app.Status != "" || app.HasSubmitted {
		t.Fatalf("null application %+v %v", app, err)
	}
	app, err = ParseBuilderApplication([]byte(`{"ok":true,"address":"0x0000000000000000000000000000000000000001","status":"pending","submittedAtMs":9}`))
	if err != nil || app.Status != "pending" || app.SubmittedAtMs != 9 {
		t.Fatalf("%+v %v", app, err)
	}
}

func TestSubmitApplication(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/query/builders/applications" || r.Header.Get("Sec-Fetch-Mode") != "cors" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"ok":true,"address":"0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266","status":"pending","submittedAtMs":9,"nextSubmissionAtMs":10}`))
	}))
	defer srv.Close()
	c := NewClient(time.Second)
	c.Exchange = srv.URL
	app := BuilderApplication{AppName: "A", WebsiteURL: "https://example.com", ContactKind: "email", Contact: "a@b.co", Integration: "x"}
	rec, err := c.SubmitApplication(context.Background(), common.HexToAddress("0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266"), app, 9, "0x"+strings.Repeat("ab", 65))
	if err != nil || rec.Status != "pending" || rec.NextSubmissionAtMs != 10 {
		t.Fatalf("%v %+v", err, rec)
	}
	if got["environment"] != nil {
		t.Fatal("environment is inside the signature, not a JSON field")
	}
	if got["appName"] != "A" || got["signedAtMs"] != json.Number("9") && got["signedAtMs"] != float64(9) {
		t.Fatalf("body %+v", got)
	}

	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"wait","code":"application_cooldown","nextSubmissionAtMs":50}`))
	})
	_, err = c.SubmitApplication(context.Background(), common.HexToAddress("0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266"), app, 9, "0x"+strings.Repeat("ab", 65))
	ae, ok := err.(*ApplicationError)
	if !ok || ae.Code != "application_cooldown" || !ae.HasNext || ae.NextAtMs != 50 || ae.Unknown {
		t.Fatalf("%v", err)
	}
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("bad gateway"))
	})
	_, err = c.SubmitApplication(context.Background(), common.HexToAddress("0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266"), app, 9, "0x"+strings.Repeat("ab", 65))
	ae, ok = err.(*ApplicationError)
	if !ok || !ae.Unknown {
		t.Fatalf("5xx must be unknown, got %v", err)
	}
}
