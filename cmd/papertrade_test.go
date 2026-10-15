package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
)

// Public Hardhat test key. Tests set DELIVERATOR_AGENT_KEY so Load never touches
// the operator keychain, and they never POST to the live relayer.
const (
	ptTestKey  = "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
	ptTestUser = "0x0000000000000000000000000000000000000001"
)

const ptTradingBody = `{
  "schemaVersion": 1,
  "indexedThroughBlock": 10,
  "tradingPaused": false,
  "minimumTradeSizeRaw": "10000000000000000000",
  "minimumOpenNotionalRaw": "10000000000000000000000",
  "accRewardPerShareRaw": "0",
  "instruments": [
    [0,"BTC",0,"10",true,false,false,false,false,null,null,null,"15000000000000000000000","814598000000000000000",1000,"10000000000000000000000000","476041580087574","100000000000000000","100000000000000000000000","0","0","0","0"]
  ],
  "intakeControls": [1,"0",[]]
}`

// ptPaperJSON is the wallet frame's paper object. Staking tests override it.
var ptPaperJSON = `{"balance": "0", "staked": "0"}`

func ptAccountBody() string {
	exp := time.Now().Unix() + 40*86400
	return `{
	  "address": "` + ptTestUser + `",
	  "indexedThroughBlock": 10,
	  "indexedThroughTimestamp": 1,
	  "sessionKeys": [["0xf39fd6e51aad88f6f4ce6ab8827279cfffb92266", ` + itoa(exp) + `]],
	  "balance": "1000000000000000000000",
	  "queued": "0",
	  "locked": "0",
	  "funds": {"availableBalance": "1000000000000000000000", "availableQueued": "0"},
	  "consumableDebt": "0",
	  "paper": ` + ptPaperJSON + `,
	  "openPositions": []
	}`
}

func ptCompact(s string) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		return s
	}
	return buf.String()
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	u := n
	if n < 0 {
		u = -n
	}
	for u > 0 {
		i--
		b[i] = byte('0' + u%10)
		u /= 10
	}
	if n < 0 {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

type ptCapture struct {
	mu    sync.Mutex
	posts [][]byte
	hits  int
	next  func(n int) (int, string)
}

func (p *ptCapture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.hits++
		p.mu.Unlock()
		if r.Header.Get("Sec-Fetch-Mode") != "cors" && r.URL.Path != "/" {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/state/trading":
			_, _ = io.WriteString(w, ptTradingBody)
		case r.Method == http.MethodGet && r.URL.Path == "/state/user/live":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: wallet\ndata: "+ptCompact(ptAccountBody())+"\n\n")
		case r.Method == http.MethodPost && r.URL.Path == "/":
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":"0x000000000000000000000000ce424134a6c414c3c2c8d853caebf8b3079adb84"}`)
		case r.Method == http.MethodPost && (r.URL.Path == "/intents" || strings.Contains(r.URL.Path, "/cancel")):
			b, _ := io.ReadAll(r.Body)
			p.mu.Lock()
			p.posts = append(p.posts, append([]byte(nil), b...))
			n := len(p.posts)
			next := p.next
			p.mu.Unlock()
			code, body := http.StatusInternalServerError, "boom"
			if next != nil {
				code, body = next(n)
			}
			w.WriteHeader(code)
			_, _ = io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	})
}

func (p *ptCapture) postCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.posts)
}

func (p *ptCapture) post(i int) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.posts[i]
}

type ptEnvelope struct {
	OK   bool            `json:"ok"`
	Cmd  string          `json:"cmd"`
	Data json.RawMessage `json:"data"`
	Err  *struct {
		Code      string   `json:"code"`
		IntentIDs []string `json:"intent_ids"`
		Hint      string   `json:"hint"`
	} `json:"error"`
}

func ptUse(t *testing.T, cap *ptCapture) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DELIVERATOR_HOME", home)
	t.Setenv("DELIVERATOR_AGENT_KEY", ptTestKey)
	t.Setenv("DELIVERATOR_MASTER_KEY", "")
	srv := httptest.NewServer(cap.handler())
	t.Cleanup(srv.Close)
	t.Setenv("DELIVERATOR_PAPERTRADE_URL", srv.URL)
	t.Setenv("DELIVERATOR_PAPERTRADE_RPC", srv.URL)

	cfg := config.Default()
	cfg.Network = config.NetworkMainnet
	cfg.Wallet.MasterAddress = ptTestUser
	cfg.State.Audit = false

	save := ptSavedFlags{
		cfg: Cfg, dry: flagDryRun, timeout: flagTimeout, noAudit: flagNoAudit,
		address: ptAddress, coin: ptCoin, side: ptSide, margin: ptMargin,
		lev: ptLeverage, debt: ptUseDebt, pos: append([]string(nil), ptPositions...),
		intent: ptIntent, days: ptDays,
	}
	Cfg = cfg
	flagDryRun = false
	flagTimeout = 2 * time.Second
	flagNoAudit = true
	ptAddress, ptCoin, ptSide, ptMargin, ptIntent = "", "", "", "", ""
	ptLeverage, ptDays = 0, 30
	ptUseDebt = false
	ptPositions = nil
	t.Cleanup(func() {
		Cfg = save.cfg
		flagDryRun, flagTimeout, flagNoAudit = save.dry, save.timeout, save.noAudit
		ptAddress, ptCoin, ptSide, ptMargin = save.address, save.coin, save.side, save.margin
		ptLeverage, ptUseDebt, ptPositions, ptIntent, ptDays = save.lev, save.debt, save.pos, save.intent, save.days
		output.Configure(true, nil)
	})
	return home
}

type ptSavedFlags struct {
	cfg     *config.Config
	dry     bool
	timeout time.Duration
	noAudit bool
	address string
	coin    string
	side    string
	margin  string
	lev     int
	debt    bool
	pos     []string
	intent  string
	days    int
}

func ptRun(t *testing.T, c *cobra.Command) (ptEnvelope, error) {
	t.Helper()
	var buf strings.Builder
	output.Configure(true, &buf)
	err := c.RunE(c, nil)
	var env ptEnvelope
	if s := buf.String(); s != "" {
		if e := json.Unmarshal([]byte(s), &env); e != nil {
			t.Fatalf("%s: expected one JSON envelope, got %q: %v", c.Name(), s, e)
		}
	}
	return env, err
}

func TestPapertradeDryRunOpen(t *testing.T) {
	cap := &ptCapture{}
	home := ptUse(t, cap)
	flagDryRun = true
	ptCoin, ptSide, ptMargin, ptLeverage = "BTC", "long", "1000", 10
	env, err := ptRun(t, ptOpenCmd)
	if rdExitCode(t, err) != output.ExitOK || !env.OK {
		t.Fatalf("dry-run exit %v env %+v", err, env)
	}
	var data struct {
		DryRun   bool   `json:"dry_run"`
		Margin   string `json:"margin"`
		Leverage int    `json:"leverage"`
		Notional string `json:"notional"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	if !data.DryRun || data.Margin != "1000" || data.Leverage != 10 || data.Notional != "10000" {
		t.Fatalf("data %+v", data)
	}
	if cap.postCount() != 0 {
		t.Fatal("dry-run posted an intent")
	}
	if _, err := os.Stat(filepath.Join(home, "papertrade")); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote venue state: %v", err)
	}
}

func TestPapertradeOpenLeverageCap(t *testing.T) {
	cap := &ptCapture{}
	ptUse(t, cap)
	flagDryRun = true
	ptCoin, ptSide, ptMargin, ptLeverage = "BTC", "long", "1000", 11
	env, err := ptRun(t, ptOpenCmd)
	if rdExitCode(t, err) != output.ExitRisk || env.Err == nil || env.Err.Code != "max_leverage" {
		t.Fatalf("exit %v env %+v", err, env.Err)
	}
	if cap.postCount() != 0 {
		t.Fatal("a rejected open must not be signed")
	}
}

func TestPapertradeOpenHalt(t *testing.T) {
	cap := &ptCapture{}
	home := ptUse(t, cap)
	if err := os.WriteFile(filepath.Join(home, "halt"), []byte("on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ptCoin, ptSide, ptMargin, ptLeverage = "BTC", "long", "1000", 10
	env, err := ptRun(t, ptOpenCmd)
	if rdExitCode(t, err) != output.ExitHalt || env.Err == nil || env.Err.Code != "halted" {
		t.Fatalf("exit %v env %+v", err, env.Err)
	}
	if cap.hitsOrZero() != 0 {
		t.Fatal("halt must reject before any venue call")
	}
}

func (p *ptCapture) hitsOrZero() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits
}

func TestPapertradeOpenUnconfirmedResend(t *testing.T) {
	cap := &ptCapture{next: func(n int) (int, string) {
		if n == 1 {
			return http.StatusInternalServerError, "boom"
		}
		return http.StatusOK, `{"ok":true,"state":"queued"}`
	}}
	ptUse(t, cap)
	ptCoin, ptSide, ptMargin, ptLeverage = "BTC", "long", "1000", 10
	env, err := ptRun(t, ptOpenCmd)
	if rdExitCode(t, err) != output.ExitTimeout || env.Err == nil || len(env.Err.IntentIDs) != 1 {
		t.Fatalf("exit %v env %+v", err, env.Err)
	}
	if !strings.Contains(env.Err.Hint, "resend") {
		t.Fatalf("hint %q", env.Err.Hint)
	}
	id := env.Err.IntentIDs[0]
	ptIntent = id
	env, err = ptRun(t, ptResendCmd)
	if rdExitCode(t, err) != output.ExitOK || !env.OK {
		t.Fatalf("resend exit %v env %+v", err, env.Err)
	}
	if cap.postCount() != 2 || string(cap.post(0)) != string(cap.post(1)) {
		t.Fatalf("resend body mismatch\nfirst %s\nsecond %s", cap.post(0), cap.post(1))
	}
	if !strings.Contains(string(cap.post(0)), `"nonce"`) || !strings.Contains(string(cap.post(0)), `"instrumentId":"0"`) {
		t.Fatalf("posted body %s", cap.post(0))
	}
}

func TestPapertradeDepositAddress(t *testing.T) {
	ptUse(t, &ptCapture{})
	env, err := ptRun(t, ptDepositCmd)
	if rdExitCode(t, err) != output.ExitOK {
		t.Fatal(err)
	}
	var data struct {
		Deposit string `json:"deposit_address"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Deposit != "0xce424134a6c414c3c2c8d853caebf8b3079adb84" {
		t.Fatalf("deposit %s", data.Deposit)
	}
}

func TestPapertradeRegisterDryRunDoesNotReadMasterKey(t *testing.T) {
	cap := &ptCapture{}
	ptUse(t, cap)
	flagDryRun = true
	env, err := ptRun(t, ptRegisterCmd)
	if rdExitCode(t, err) != output.ExitOK || !env.OK {
		t.Fatalf("exit %v env %+v", err, env.Err)
	}
	if cap.postCount() != 0 {
		t.Fatal("dry-run register posted")
	}
	if !strings.Contains(string(env.Data), "dry_run") {
		t.Fatalf("data %s", env.Data)
	}
}
