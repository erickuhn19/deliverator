package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/output"
)

const ptHardhatAddr = "0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266"

// ptExtra layers the deposit, info, and positioned-account routes over the
// shared ptCapture relayer, so every papertrade command runs offline.
type ptExtra struct {
	cap         *ptCapture
	spotUSDC    string
	withPos     bool
	checkStatus int
	checks      int
}

func (x *ptExtra) handler() http.Handler {
	base := x.cap.handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/info":
			var q map[string]string
			_ = json.NewDecoder(r.Body).Decode(&q)
			if q["type"] == "spotClearinghouseState" {
				_, _ = io.WriteString(w, `{"balances":[{"coin":"USDC","token":0,"total":"`+x.spotUSDC+`","hold":"0.0"}]}`)
			} else {
				_, _ = io.WriteString(w, `{"withdrawable":"0.0"}`)
			}
		case r.URL.Path == "/funding/deposits/check":
			x.checks++
			if x.checkStatus != 0 {
				w.WriteHeader(x.checkStatus)
				_, _ = io.WriteString(w, `{"error":"noDeposit"}`)
				return
			}
			_, _ = io.WriteString(w, `{"proxy":"0xCE424134A6C414C3C2C8D853CAEBF8B3079ADB84"}`)
		case r.URL.Path == "/state/user/live" && x.withPos:
			body := strings.Replace(ptAccountBody(), `"openPositions": []`,
				`"openPositions": [["7", 0, true, false, "830165", "20000000000000000000", 500, "828899", 1700000000000, "0x`+strings.Repeat("ab", 32)+`"]]`, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: wallet\ndata: "+ptCompact(body)+"\n\n")
		default:
			base.ServeHTTP(w, r)
		}
	})
}

func ptUseExtra(t *testing.T, x *ptExtra) string {
	t.Helper()
	home := ptUse(t, x.cap)
	srv := httptest.NewServer(x.handler())
	t.Cleanup(srv.Close)
	t.Setenv("DELIVERATOR_PAPERTRADE_URL", srv.URL)
	t.Setenv("DELIVERATOR_PAPERTRADE_RPC", srv.URL)
	t.Setenv("DELIVERATOR_PAPERTRADE_INFO", srv.URL+"/info")
	return home
}

func ptOKReply(n int) (int, string) { return http.StatusOK, `{"ok":true,"state":"queued"}` }

func TestPapertradeReadsOffline(t *testing.T) {
	x := &ptExtra{cap: &ptCapture{}, withPos: true}
	ptUseExtra(t, x)
	env, err := ptRun(t, ptMarketsCmd)
	if rdExitCode(t, err) != output.ExitOK || !env.OK {
		t.Fatalf("markets %v %+v", err, env.Err)
	}
	env, err = ptRun(t, ptAccountCmd)
	if rdExitCode(t, err) != output.ExitOK || !strings.Contains(string(env.Data), `"session_ready":true`) {
		t.Fatalf("account %v %s", err, env.Data)
	}
	env, err = ptRun(t, ptSessionCmd)
	if rdExitCode(t, err) != output.ExitOK || !strings.Contains(string(env.Data), ptHardhatAddr) {
		t.Fatalf("session %v %s", err, env.Data)
	}
	// summary is not served: a network failure, not a crash.
	if _, err := ptRun(t, ptSummaryCmd); rdExitCode(t, err) == output.ExitOK {
		t.Fatal("summary should fail without a venue route")
	}
}

func TestPapertradeReadsNetworkFailure(t *testing.T) {
	ptUse(t, &ptCapture{})
	t.Setenv("DELIVERATOR_PAPERTRADE_URL", "http://127.0.0.1:1")
	t.Setenv("DELIVERATOR_PAPERTRADE_RPC", "http://127.0.0.1:1")
	for _, c := range []*cobra.Command{ptMarketsCmd, ptAccountCmd, ptSessionCmd, ptDepositCmd, ptDepositStatusCmd} {
		if _, err := ptRun(t, c); rdExitCode(t, err) == output.ExitOK {
			t.Fatalf("%s should fail offline", c.Name())
		}
	}
}

func TestPapertradeDepositStatusAndRegister(t *testing.T) {
	x := &ptExtra{cap: &ptCapture{}, spotUSDC: "5.0"}
	ptUseExtra(t, x)
	env, err := ptRun(t, ptDepositStatusCmd)
	if rdExitCode(t, err) != output.ExitOK || !strings.Contains(string(env.Data), "deposit_address") {
		t.Fatalf("status %v %s", err, env.Data)
	}
	env, err = ptRun(t, ptDepositRegisterCmd)
	if rdExitCode(t, err) == output.ExitOK || env.Err == nil || env.Err.Code != "deposit" || x.checks != 0 {
		t.Fatalf("under-minimum register must not post: %v %+v checks=%d", err, env.Err, x.checks)
	}
	x.spotUSDC = "22.0"
	flagDryRun = true
	env, err = ptRun(t, ptDepositRegisterCmd)
	if rdExitCode(t, err) != output.ExitOK || !strings.Contains(string(env.Data), "dry_run") || x.checks != 0 {
		t.Fatalf("dry-run register %v %s", err, env.Data)
	}
	flagDryRun = false
	env, err = ptRun(t, ptDepositRegisterCmd)
	if rdExitCode(t, err) != output.ExitOK || !strings.Contains(string(env.Data), "venue_proxy") || x.checks != 1 {
		t.Fatalf("register %v %s", err, env.Data)
	}
	x.checkStatus = http.StatusBadRequest
	if _, err := ptRun(t, ptDepositRegisterCmd); rdExitCode(t, err) == output.ExitOK {
		t.Fatal("a rejected check must fail")
	}
}

func TestPapertradeCloseFlow(t *testing.T) {
	x := &ptExtra{cap: &ptCapture{next: ptOKReply}, withPos: true}
	ptUseExtra(t, x)
	ptPositions = []string{"9"}
	env, err := ptRun(t, ptCloseCmd)
	if rdExitCode(t, err) == output.ExitOK || env.Err == nil || env.Err.Code != "position" {
		t.Fatalf("unknown position %v %+v", err, env.Err)
	}
	ptPositions = []string{"7"}
	flagDryRun = true
	env, err = ptRun(t, ptCloseCmd)
	if rdExitCode(t, err) != output.ExitOK || x.cap.postCount() != 0 {
		t.Fatalf("dry close %v %+v", err, env.Err)
	}
	flagDryRun = false
	env, err = ptRun(t, ptCloseCmd)
	if rdExitCode(t, err) != output.ExitOK || !strings.Contains(string(env.Data), "closePositions") || x.cap.postCount() != 1 {
		t.Fatalf("close %v %+v %s", err, env.Err, env.Data)
	}
	// intent status sees the stored close.
	var data struct {
		IntentID string `json:"intent_id"`
	}
	_ = json.Unmarshal(env.Data, &data)
	ptIntent = data.IntentID
	env, err = ptRun(t, ptIntentStatusCmd)
	if rdExitCode(t, err) != output.ExitOK || !strings.Contains(string(env.Data), `"stored":true`) {
		t.Fatalf("intent status %v %s", err, env.Data)
	}
}

func TestPapertradeCancelFlow(t *testing.T) {
	x := &ptExtra{cap: &ptCapture{next: ptOKReply}}
	ptUseExtra(t, x)
	ptIntent = "0x" + strings.Repeat("cd", 32)
	flagDryRun = true
	if env, err := ptRun(t, ptCancelCmd); rdExitCode(t, err) != output.ExitOK || !env.OK {
		t.Fatalf("dry cancel %v", err)
	}
	flagDryRun = false
	env, err := ptRun(t, ptCancelCmd)
	if rdExitCode(t, err) != output.ExitOK || !strings.Contains(string(env.Data), "cancelIntent") {
		t.Fatalf("cancel %v %+v", err, env.Err)
	}
	ptIntent = "zz"
	if _, err := ptRun(t, ptCancelCmd); rdExitCode(t, err) == output.ExitOK {
		t.Fatal("bad intent must fail")
	}
	// unknown intent: status reports stored=false and not seen.
	ptIntent = strings.Repeat("ef", 32)
	env, err = ptRun(t, ptIntentStatusCmd)
	if rdExitCode(t, err) != output.ExitOK || !strings.Contains(string(env.Data), `"stored":false`) {
		t.Fatalf("status %v %s", err, env.Data)
	}
	if _, err := ptRun(t, ptResendCmd); rdExitCode(t, err) == output.ExitOK {
		t.Fatal("resend of an unknown intent must fail")
	}
}

func TestPapertradeSessionRegisterLive(t *testing.T) {
	x := &ptExtra{cap: &ptCapture{next: ptOKReply}}
	ptUseExtra(t, x)
	if _, err := ptRun(t, ptRegisterCmd); rdExitCode(t, err) == output.ExitOK {
		t.Fatal("live register without a master key must fail")
	}
	t.Setenv("DELIVERATOR_MASTER_KEY", "nothex")
	if _, err := ptRun(t, ptRegisterCmd); rdExitCode(t, err) == output.ExitOK {
		t.Fatal("bad master key must fail")
	}
	t.Setenv("DELIVERATOR_MASTER_KEY", "0x"+ptTestKey)
	// wallet.master_address (0x…01) does not match the key.
	env, err := ptRun(t, ptRegisterCmd)
	if rdExitCode(t, err) == output.ExitOK || env.Err == nil || env.Err.Code != "master_address" {
		t.Fatalf("mismatch %v %+v", err, env.Err)
	}
	Cfg.Wallet.MasterAddress = ptHardhatAddr
	ptAddress = "0x0000000000000000000000000000000000000002"
	if _, err := ptRun(t, ptRegisterCmd); rdExitCode(t, err) == output.ExitOK {
		t.Fatal("--address mismatch must fail")
	}
	ptAddress = ""
	ptDays = 99
	if _, err := ptRun(t, ptRegisterCmd); rdExitCode(t, err) == output.ExitOK {
		t.Fatal("days out of range must fail")
	}
	ptDays = 30
	env, err = ptRun(t, ptRegisterCmd)
	if rdExitCode(t, err) != output.ExitOK || !strings.Contains(string(env.Data), "registerSessionKey") || x.cap.postCount() != 1 {
		t.Fatalf("register %v %+v", err, env.Err)
	}
}

func TestPapertradeClassifyBranches(t *testing.T) {
	ptUse(t, &ptCapture{})
	cases := []struct {
		status int
		body   string
	}{
		{http.StatusTooManyRequests, `{"error":"rate_limited"}`},
		{http.StatusOK, `{"ok":false,"code":"bad_nonce","message":"nonce used"}`},
		{http.StatusOK, `{"ok":false,"reason":"why"}`},
		{http.StatusOK, `{"ok":true,"state":"failed"}`},
		{http.StatusOK, `{"ok":true,"state":"weird"}`},
		{http.StatusOK, `{"ok":true,"state":"queued","intentId":"0x01"}`},
	}
	for _, c := range cases {
		c := c
		x := &ptExtra{cap: &ptCapture{next: func(int) (int, string) { return c.status, c.body }}, withPos: true}
		ptUseExtra(t, x)
		ptPositions = []string{"7"}
		if _, err := ptRun(t, ptCloseCmd); rdExitCode(t, err) == output.ExitOK {
			t.Fatalf("reply %d %s should not be a confirmation", c.status, c.body)
		}
	}
}

func TestPapertradeHelperValidation(t *testing.T) {
	if _, err := paperSide("up"); err == nil {
		t.Fatal("side")
	}
	if v, err := paperSide("sell"); err != nil || v {
		t.Fatal("sell")
	}
	for _, bad := range [][]string{nil, {"1", "1"}, {"x"}, {"-1"}} {
		if _, err := paperPositionIDs(bad); err == nil {
			t.Fatalf("ids %v", bad)
		}
	}
	if ids, err := paperPositionIDs([]string{" 1", "2"}); err != nil || len(ids) != 2 {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "0x12", "0x" + strings.Repeat("g", 64)} {
		if _, err := paperIntentHash(bad); err == nil {
			t.Fatalf("hash %q", bad)
		}
	}
	if _, err := paperMargin("abc"); err == nil {
		t.Fatal("margin")
	}
	if wireDeadline(make(chan int)) != "" {
		t.Fatal("unmarshalable wire")
	}
}
