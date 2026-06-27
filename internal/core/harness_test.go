package core

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/erickuhn19/deliverator/internal/config"
	hl "github.com/erickuhn19/deliverator/internal/hl"
	"github.com/erickuhn19/deliverator/internal/state"
)

const (
	testAgentKeyHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testMaster      = "0x1234567890abcdef1234567890abcdef12345678"
)

// testMeta builds a MetaStore covering a perp (BTC), an isolated-only perp (ETH),
// and a non-canonical spot pair (PURR/USDC -> asset 10000). No network.
func testMeta() *MetaStore {
	meta := &hl.Meta{Universe: []hl.AssetInfo{
		{Name: "BTC", SzDecimals: 5, MaxLeverage: 40},
		{Name: "ETH", SzDecimals: 4, MaxLeverage: 25, OnlyIsolated: true},
	}}
	spot := &hl.SpotMeta{
		Universe: []hl.SpotAssetInfo{{Name: "PURR/USDC", Index: 0, Tokens: []int{1}}},
		Tokens:   []hl.SpotTokenInfo{{Index: 1, SzDecimals: 0}},
	}
	return NewMetaStore("testnet", meta, spot, time.Now())
}

// respFn returns a canned (httpStatus, jsonBody) per request. For /info it is
// keyed by the request "type"; for /exchange by the signed action's "type".
type respFn func(path, typ string, body map[string]any) (int, string)

// approve wraps a transport so the `maxBuilderFee` approval check reports maxTenths
// (tenths-of-bps), letting a builder-attach test act as a master-APPROVED trader.
// The builder fee ships on by default but only attaches once approved (graceful
// attach, resolveBuilderApproved); without this an attach test would skip fee-free.
func approve(maxTenths int, inner respFn) respFn {
	return func(path, typ string, body map[string]any) (int, string) {
		if path == "/info" && typ == "maxBuilderFee" {
			return 200, strconv.Itoa(maxTenths)
		}
		return inner(path, typ, body)
	}
}

// testHome isolates all file state (halt file, rate log, nonce, audit, meta
// cache) under a temp DELIVERATOR_HOME for the duration of a test.
func testHome(t *testing.T) { t.Helper(); t.Setenv("DELIVERATOR_HOME", t.TempDir()) }

// newCfgClient builds a Client with only cfg + meta wired — enough for the risk
// gauntlet and pure helpers, which never touch the network. File state isolated.
func newCfgClient(t *testing.T, cfg *config.Config) *Client {
	t.Helper()
	testHome(t)
	return &Client{cfg: cfg, meta: testMeta(), queryAddr: testMaster}
}

// newTestClient builds a full Client wired to an httptest server (no real
// network, no keychain): info + a pre-set signing exchange so c.exchange()
// returns it without loading a key. Audit is enabled under the temp HOME.
func newTestClient(t *testing.T, cfg *config.Config, opts Options, resp respFn) (*Client, context.Context) {
	t.Helper()
	testHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		typ, _ := body["type"].(string)
		if r.URL.Path == "/exchange" {
			if action, ok := body["action"].(map[string]any); ok {
				typ, _ = action["type"].(string)
			}
		}
		code, out := 200, `{}`
		if resp != nil {
			code, out = resp(r.URL.Path, typ, body)
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, out)
	}))
	t.Cleanup(srv.Close)

	meta := testMeta()
	key, err := crypto.HexToECDSA(testAgentKeyHex)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c := &Client{
		cfg:       cfg,
		opts:      opts,
		network:   "testnet",
		infoURL:   srv.URL,
		signURL:   srv.URL,
		httpc:     &http.Client{},
		meta:      meta,
		info:      hl.NewInfo(ctx, srv.URL, true, meta.Meta(), meta.SpotMeta(), nil),
		ex:        hl.NewExchange(ctx, key, srv.URL, meta.Meta(), "", testMaster, meta.SpotMeta(), nil),
		queryAddr: testMaster,
		nonce:     state.NewNonceLock(filepath.Join(config.Dir(), "nonce.lock")),
		audit:     state.NewAudit(filepath.Join(config.Dir(), "audit.jsonl"), true),
	}
	return c, ctx
}

// readAudit returns the audit JSONL entries written under the temp HOME.
func readAudit(t *testing.T) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(config.Dir(), "audit.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// okOrder builds an /exchange order response with a single status.
func okOrder(statusJSON string) string {
	return `{"status":"ok","response":{"type":"order","data":{"statuses":[` + statusJSON + `]}}}`
}
