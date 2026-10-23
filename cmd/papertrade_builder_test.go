package cmd

import (
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
	"github.com/erickuhn19/deliverator/internal/papertrade"
)

func TestPapertradeRegisterDefaultsToNoBuilder(t *testing.T) {
	cap := &ptCapture{}
	ptUse(t, cap)
	// The shipped Hyperliquid builder must not become a Papertrade fee.
	if Cfg.Builder.Address == "" {
		t.Fatal("test config lost the Hyperliquid builder")
	}
	flagDryRun = true
	env, err := ptRun(t, ptRegisterCmd)
	if rdExitCode(t, err) != output.ExitOK || !strings.Contains(string(env.Data), `"fee_address":"0x0000000000000000000000000000000000000000"`) {
		t.Fatalf("default fee %v %s", err, env.Data)
	}
	if cap.postCount() != 0 {
		t.Fatal("dry-run posted")
	}
}

func TestPapertradeBuilderRegisterAndClaim(t *testing.T) {
	posts := 0
	var intent []byte
	var application map[string]any
	approved := false
	unclaimed := big.NewInt(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/state/trading":
			_, _ = w.Write([]byte(ptTradingBody))
		case r.Method == http.MethodGet && r.URL.Path == "/state/user/live":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: wallet\ndata: " + ptCompact(ptAccountBody()) + "\n\n"))
		case r.Method == http.MethodGet && r.URL.Path == "/query/builders/leaderboard":
			_, _ = w.Write([]byte(`{"ok":true,"registry":"0xfdc6550436dd75ce8a0b145f935b7d3a64f53248","feeRateRaw":"5000000000000000","asOfMs":10,"totals":{"builders":0,"users":0,"closes":0,"notionalRaw":"0","notionalDayRaw":"0","accruedRaw":"0"},"builders":[]}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/query/builders/") && strings.HasSuffix(r.URL.Path, "/stats"):
			_, _ = w.Write([]byte(`{"ok":true,"address":"` + r.URL.Path[len("/query/builders/"):len(r.URL.Path)-len("/stats")] + `","approved":false,"unclaimedRaw":"0","routing":{"users":1,"sessionKeys":2,"closes":3,"notionalRaw":"4","asOfMs":10},"earnings":null}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/query/builders/") && strings.HasSuffix(r.URL.Path, "/application"):
			_, _ = w.Write([]byte(`{"ok":true,"address":"` + r.URL.Path[len("/query/builders/"):len(r.URL.Path)-len("/application")] + `","status":null,"submittedAtMs":null}`))
		case r.Method == http.MethodPost && r.URL.Path == "/query/builders/applications":
			_ = json.NewDecoder(r.Body).Decode(&application)
			addr, _ := application["address"].(string)
			_, _ = w.Write([]byte(`{"ok":true,"address":"` + addr + `","status":"pending","submittedAtMs":9,"nextSubmissionAtMs":10}`))
		case r.Method == http.MethodPost && r.URL.Path == "/intents":
			intent, _ = readAll(r)
			posts++
			_, _ = w.Write([]byte(`{"ok":true,"state":"queued"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/":
			var req struct {
				Method string `json:"method"`
				Params []any  `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			var result any = "0x0"
			switch req.Method {
			case "eth_chainId":
				result = "0x3e7"
			case "eth_getTransactionCount":
				result = "0x0"
			case "eth_getBalance":
				result = "0x3635c9adc5dea00000"
			case "eth_gasPrice":
				result = "0x5f5e100"
			case "eth_getBlockByNumber":
				result = map[string]string{"baseFeePerGas": "0x5f5e100"}
			case "eth_estimateGas":
				result = "0x5208"
			case "eth_call":
				call, _ := req.Params[0].(map[string]any)
				data, _ := call["data"].(string)
				switch {
				case strings.Contains(data, "e737de13"):
					result = "0x" + padAddr(papertrade.BuilderRegistryFallback)
				case strings.Contains(data, "978bbdb9"):
					result = "0x" + padInt(big.NewInt(5_000_000_000_000_000))
				case strings.Contains(data, "673448dd"):
					if approved {
						result = "0x" + padInt(big.NewInt(1))
					} else {
						result = "0x" + padInt(big.NewInt(0))
					}
				case strings.Contains(data, "9d346583"):
					result = "0x" + padInt(unclaimed)
				default:
					result = "0x" + strings.Repeat("00", 32)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ptUse(t, &ptCapture{})
	t.Setenv("DELIVERATOR_PAPERTRADE_URL", srv.URL)
	t.Setenv("DELIVERATOR_PAPERTRADE_RPC", srv.URL)

	builder := "0x00000000000000000000000000000000000000b1"
	ptBuilder = builder
	flagDryRun = true
	env, err := ptRun(t, ptRegisterCmd)
	if rdExitCode(t, err) == output.ExitOK || env.Err == nil || env.Err.Code != "builder_not_approved" {
		t.Fatalf("unapproved dry-run %v %+v", err, env.Err)
	}
	approved = true
	env, err = ptRun(t, ptRegisterCmd)
	if rdExitCode(t, err) != output.ExitOK || !strings.Contains(string(env.Data), `"fee_address":"`+builder+`"`) || !strings.Contains(strings.Join(env.Warnings, " "), "0.5%") {
		t.Fatalf("approved dry-run %v %s warnings %v", err, env.Data, env.Warnings)
	}
	flagDryRun = false
	t.Setenv("DELIVERATOR_MASTER_KEY", "0x"+ptTestKey)
	Cfg.Wallet.MasterAddress = ptHardhatAddr
	env, err = ptRun(t, ptRegisterCmd)
	if rdExitCode(t, err) == output.ExitOK || env.Err == nil || env.Err.Code != "confirm" || posts != 0 {
		t.Fatalf("missing yes %v %+v posts %d", err, env.Err, posts)
	}
	flagYes = true
	t.Cleanup(func() { flagYes = false })
	env, err = ptRun(t, ptRegisterCmd)
	if rdExitCode(t, err) != output.ExitOK || posts != 1 || !strings.Contains(string(intent), `"feeAddress":"`+builder+`"`) {
		t.Fatalf("register %v %s body %s", err, env.Data, intent)
	}

	// Opt out of a configured builder for one registration.
	Cfg.Papertrade.BuilderAddress = builder
	ptBuilder = "none"
	flagDryRun = true
	flagYes = false
	env, err = ptRun(t, ptRegisterCmd)
	if rdExitCode(t, err) != output.ExitOK || !strings.Contains(string(env.Data), `"fee_address":"0x0000000000000000000000000000000000000000"`) {
		t.Fatalf("opt out %v %s", err, env.Data)
	}

	ptBuilder = ""
	ptAddress = builder
	env, err = ptRun(t, ptBuilderCmd)
	if rdExitCode(t, err) != output.ExitOK || !strings.Contains(string(env.Data), `"approved":true`) || !strings.Contains(string(env.Data), `"fee_rate":"0.5%"`) {
		t.Fatalf("status %v %s", err, env.Data)
	}

	ptAddress = ""
	unclaimed = big.NewInt(0)
	env, err = ptRun(t, ptBuilderClaimCmd)
	if rdExitCode(t, err) == output.ExitOK || env.Err == nil || env.Err.Code != "papertrade_tx" {
		t.Fatalf("nothing to claim %v %+v", err, env.Err)
	}
	unclaimed = big.NewInt(10)
	// Dry-run claim targets wallet.master_address, which is the hardhat key, not builder.
	Cfg.Wallet.MasterAddress = ptHardhatAddr
	env, err = ptRun(t, ptBuilderClaimCmd)
	if rdExitCode(t, err) != output.ExitOK || !strings.Contains(string(env.Data), `"dry_run":true`) || !strings.Contains(string(env.Data), "69f9a5f0") {
		t.Fatalf("claim dry-run %v %s", err, env.Data)
	}

	ptAppName, ptWebsite, ptContactKind, ptContact, ptIntegration = "Deliverator", "https://deliverator.net", "email", "Ops@Example.com", "Session keys."
	env, err = ptRun(t, ptBuilderApplyCmd)
	if rdExitCode(t, err) != output.ExitOK || !strings.Contains(string(env.Data), "This signature only submits a builder application") {
		t.Fatalf("apply dry-run %v %s", err, env.Data)
	}
	if application != nil {
		t.Fatal("dry-run apply posted")
	}
	flagDryRun = false
	flagYes = true
	t.Setenv("DELIVERATOR_MASTER_KEY", "0x"+ptTestKey)
	env, err = ptRun(t, ptBuilderApplyCmd)
	if rdExitCode(t, err) != output.ExitOK || application == nil {
		t.Fatalf("apply %v %+v", err, env.Err)
	}
	msg, _ := application["message"].(string)
	if msg != "" {
		t.Fatal("the signed text must not be a separate JSON field; it is what the signature covers")
	}
	sig, _ := application["signature"].(string)
	raw := common.FromHex(sig)
	if len(raw) != 65 {
		t.Fatalf("signature %s", sig)
	}
	raw[64] -= 27
	signedAt, _ := application["signedAtMs"].(float64)
	text := papertrade.ApplicationMessage(
		"0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266",
		papertrade.BuilderApplicationEnvironment,
		papertrade.BuilderApplication{
			AppName: "Deliverator", WebsiteURL: "https://deliverator.net",
			ContactKind: "email", Contact: "ops@example.com", Integration: "Session keys.",
		},
		int64(signedAt),
	)
	pub, err := crypto.SigToPub(accounts.TextHash([]byte(text)), raw)
	if err != nil || crypto.PubkeyToAddress(*pub) != common.HexToAddress(ptHardhatAddr) {
		t.Fatalf("recovered %v %v", err, pub)
	}
}

func TestConfigSetPapertradeBuilderWarns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DELIVERATOR_HOME", home)
	if err := config.Default().Save(config.Path()); err != nil {
		t.Fatal(err)
	}
	save := flagConfig
	flagConfig = ""
	t.Cleanup(func() { flagConfig = save })
	env, err := runCmd(t, configSetCmd, []string{"papertrade.builder_address", "0x0000000000000000000000000000000000000001"})
	if err != nil || !env.OK {
		t.Fatalf("set %v %+v", err, env)
	}
	warned := false
	for _, w := range env.Warnings {
		if strings.Contains(w, "papertrade.builder_address changed") && strings.Contains(w, "1% of adjusted PnL") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("warnings %v", env.Warnings)
	}
	got, err := config.Load(config.Path())
	if err != nil || got.Papertrade.BuilderAddress != "0x0000000000000000000000000000000000000001" {
		t.Fatalf("saved %+v %v", got, err)
	}
	env, err = runCmd(t, configSetCmd, []string{"papertrade.builder_address", "none"})
	if err != nil || !env.OK {
		t.Fatal(err)
	}
	got, err = config.Load(config.Path())
	if err != nil || got.Papertrade.BuilderAddress != "" {
		t.Fatalf("cleared %+v %v", got, err)
	}
}

func readAll(r *http.Request) ([]byte, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}

func padInt(n *big.Int) string {
	b := make([]byte, 32)
	if n != nil {
		raw := n.Bytes()
		copy(b[32-len(raw):], raw)
	}
	return common.Bytes2Hex(b)
}

func padAddr(s string) string { return padInt(new(big.Int).SetBytes(common.HexToAddress(s).Bytes())) }
