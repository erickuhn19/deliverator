package papertrade

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

func TestDepositCheckBody(t *testing.T) {
	got := string(DepositCheckBody(common.HexToAddress("0x9ccAcA47f0318FaeF9C8175767a15AEe1586177e")))
	if got != `{"user":"0x9ccaca47f0318faef9c8175767a15aee1586177e"}` {
		t.Fatal(got)
	}
}

func TestCoreBalanceAndCheck(t *testing.T) {
	var checkBody []byte
	var build string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/info":
			var q map[string]string
			_ = json.NewDecoder(r.Body).Decode(&q)
			if q["type"] == "spotClearinghouseState" {
				_, _ = io.WriteString(w, `{"balances":[{"coin":"USDC","token":0,"total":"22.5","hold":"0.0"},{"coin":"HYPE","total":"1","hold":"0"}]}`)
			} else {
				_, _ = io.WriteString(w, `{"withdrawable":"0.0"}`)
			}
		case DepositCheckPath:
			checkBody, _ = io.ReadAll(r.Body)
			build = r.Header.Get("X-Papertrade-Build")
			_, _ = io.WriteString(w, `{"proxy":"0x90DF7D81A9BC4DED5435ED926271F67FE77BE4B3"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(2 * time.Second)
	c.Exchange, c.Info = srv.URL, srv.URL+"/info"
	ctx := context.Background()
	bal, err := c.CoreBalance(ctx, common.HexToAddress("0x90df7d81a9bc4ded5435ed926271f67fe77be4b3"))
	if err != nil || bal.TotalRaw != 22_500_000 || !bal.MeetsMinimum || bal.SpotUSDC != "22.5" {
		t.Fatalf("%+v %v", bal, err)
	}
	user := common.HexToAddress("0x9ccAcA47f0318FaeF9C8175767a15AEe1586177e")
	p, err := c.CheckDeposit(ctx, user)
	if err != nil || p != common.HexToAddress("0x90df7d81a9bc4ded5435ed926271f67fe77be4b3") {
		t.Fatal(p, err)
	}
	if string(checkBody) != string(DepositCheckBody(user)) || build != DepositBuild {
		t.Fatalf("body %s build %s", checkBody, build)
	}
}

func TestCheckDepositRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":"bad"}`)
	}))
	defer srv.Close()
	c := NewClient(2 * time.Second)
	c.Exchange = srv.URL
	_, err := c.CheckDeposit(context.Background(), common.HexToAddress("0x01"))
	if de, ok := err.(*DepositCheckError); !ok || de.Status != 400 {
		t.Fatalf("%T %v", err, err)
	}
}

func TestUSDC6(t *testing.T) {
	for in, want := range map[string]int64{"10": 10_000_000, "0.0000019": 1, "21.0": 21_000_000} {
		if got, err := usdc6(in); err != nil || got != want {
			t.Fatalf("%s: %d %v", in, got, err)
		}
	}
}
