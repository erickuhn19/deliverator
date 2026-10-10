package papertrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

func TestClientReadsAndReceipts(t *testing.T) {
	var posts [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-Fetch-Mode") != "cors" && r.URL.Path != "/" {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/state/trading":
			_, _ = io.WriteString(w, sampleTrading)
		case r.Method == http.MethodGet && r.URL.Path == "/state/user/live":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: liveness\ndata: {}\n\nevent: wallet\ndata: "+compactJSON(sampleAccount)+"\n\n")
		case r.Method == http.MethodPost && r.URL.Path == "/":
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":"0x000000000000000000000000ce424134a6c414c3c2c8d853caebf8b3079adb84"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/intents":
			b, _ := io.ReadAll(r.Body)
			posts = append(posts, b)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "boom")
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"code":"rate_limited","message":"slow down"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(2 * time.Second)
	c.Exchange = srv.URL
	c.RPC = srv.URL
	ctx := context.Background()

	m, err := c.Markets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Find("BTC"); !ok {
		t.Fatal("missing BTC")
	}
	acct, err := c.Account(ctx, common.HexToAddress("0x0000000000000000000000000000000000000001"))
	if err != nil || len(acct.OpenPositions) != 2 {
		t.Fatalf("account %+v %v", acct, err)
	}
	proxy, err := c.DepositProxy(ctx, common.HexToAddress("0x0000000000000000000000000000000000000001"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(proxy.Hex(), "0xce424134a6c414c3c2c8d853caebf8b3079adb84") {
		t.Fatalf("proxy %s", proxy.Hex())
	}
	_, err = c.PostIntent(ctx, "/intents", []byte(`{"action":"openPosition"}`))
	var te *TransportError
	if !errors.As(err, &te) || !te.Sent || te.Status != 500 {
		t.Fatalf("500: %v", err)
	}
	if len(posts) != 1 || string(posts[0]) != `{"action":"openPosition"}` {
		t.Fatalf("posted %q", posts)
	}
	rec, err := c.PostIntent(ctx, "/intents/0xabc/cancel", []byte(`{}`))
	if err != nil || rec.Status != 429 || rec.Code != "rate_limited" {
		t.Fatalf("429 rec=%+v err=%v", rec, err)
	}
}

func compactJSON(s string) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		return s
	}
	return buf.String()
}

func TestProxyCalldataSelector(t *testing.T) {
	got := proxyCalldata(common.HexToAddress("0x0000000000000000000000000000000000000001"))
	want := "0xd600539a0000000000000000000000000000000000000000000000000000000000000001"
	if got != want {
		t.Fatalf("calldata %s", got)
	}
}

func TestPostIntentNotSent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	c := NewClient(time.Second)
	c.Exchange = url
	_, err := c.PostIntent(context.Background(), "/intents", []byte(`{}`))
	var te *TransportError
	if !errors.As(err, &te) || te.Sent {
		t.Fatalf("closed server must be a pre-send failure, got %v", err)
	}
}
