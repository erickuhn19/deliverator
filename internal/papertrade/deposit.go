package papertrade

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// A HyperCore deposit is two steps. The wallet sends HyperCore USDC to its
// deposit proxy (computeProxyAddress). Then the venue client POSTs
// /funding/deposits/check with {"user": owner}; the venue deploys the proxy
// and sweeps the USDC into the exchange (relayer action registerDeposit).
// The check is unsigned. Source: exchange.papertrade.xyz bundle
// (assets/execution-*.js, deposit-reconcile mutation), which calls it once
// the proxy's spot+perp available USDC is at least MinDepositRaw.
const (
	DepositCheckPath = "/funding/deposits/check"
	// DepositBuild is the venue client's X-Papertrade-Build header value.
	DepositBuild = "d912992162fe"
	// MinDepositRaw is 10 USDC in HyperCore 6-decimal units.
	MinDepositRaw  = 10_000_000
	defaultInfoURL = "https://api.hyperliquid.xyz/info"
)

// InfoURL is the Hyperliquid info API. DELIVERATOR_PAPERTRADE_INFO overrides it.
func InfoURL() string { return trimURL(envOr("DELIVERATOR_PAPERTRADE_INFO", defaultInfoURL)) }

// ProxyBalance is the USDC sitting on HyperCore at a deposit proxy.
type ProxyBalance struct {
	Proxy        string `json:"proxy"`
	SpotUSDC     string `json:"spot_usdc"`
	PerpUSDC     string `json:"perp_withdrawable_usdc"`
	TotalRaw     int64  `json:"total_raw_6dp"`
	MeetsMinimum bool   `json:"meets_minimum"`
}

// CoreBalance reads spotClearinghouseState and clearinghouseState for proxy.
func (c *Client) CoreBalance(ctx context.Context, proxy common.Address) (ProxyBalance, error) {
	out := ProxyBalance{Proxy: strings.ToLower(proxy.Hex()), SpotUSDC: "0", PerpUSDC: "0"}
	var spot struct {
		Balances []struct {
			Coin  string `json:"coin"`
			Total string `json:"total"`
			Hold  string `json:"hold"`
		} `json:"balances"`
	}
	if err := c.info(ctx, map[string]string{"type": "spotClearinghouseState", "user": out.Proxy}, &spot); err != nil {
		return out, err
	}
	var perp struct {
		Withdrawable string `json:"withdrawable"`
	}
	if err := c.info(ctx, map[string]string{"type": "clearinghouseState", "user": out.Proxy}, &perp); err != nil {
		return out, err
	}
	var total int64
	for _, b := range spot.Balances {
		if b.Coin == "USDC" {
			out.SpotUSDC = b.Total
			avail, err := subDec(b.Total, b.Hold)
			if err != nil {
				return out, err
			}
			total += avail
		}
	}
	if perp.Withdrawable != "" {
		out.PerpUSDC = perp.Withdrawable
		p, err := usdc6(perp.Withdrawable)
		if err != nil {
			return out, err
		}
		total += p
	}
	out.TotalRaw = total
	out.MeetsMinimum = total >= MinDepositRaw
	return out, nil
}

func subDec(a, b string) (int64, error) {
	x, err := usdc6(a)
	if err != nil {
		return 0, err
	}
	if b == "" {
		return x, nil
	}
	y, err := usdc6(b)
	if err != nil {
		return 0, err
	}
	return x - y, nil
}

// usdc6 parses a decimal string into 6-decimal units (truncating).
func usdc6(s string) (int64, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok {
		return 0, fmt.Errorf("bad decimal %q", s)
	}
	r.Mul(r, big.NewRat(1_000_000, 1))
	return new(big.Int).Quo(r.Num(), r.Denom()).Int64(), nil
}

func (c *Client) info(ctx context.Context, body any, out any) error {
	url := c.Info
	if url == "" {
		url = InfoURL()
	}
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("hyperliquid info: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hyperliquid info: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return json.Unmarshal(raw, out)
}

// DepositCheckBody is the exact JSON the venue client posts.
func DepositCheckBody(user common.Address) []byte {
	b, _ := json.Marshal(map[string]string{"user": strings.ToLower(user.Hex())})
	return b
}

// DepositCheckError is a non-2xx answer from /funding/deposits/check.
type DepositCheckError struct {
	Status int
	Body   string
}

func (e *DepositCheckError) Error() string {
	return fmt.Sprintf("deposit check: HTTP %d: %s", e.Status, e.Body)
}

// CheckDeposit asks the venue to register HyperCore USDC sitting at the
// user's proxy. It returns the proxy the venue reports.
func (c *Client) CheckDeposit(ctx context.Context, user common.Address) (common.Address, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Exchange+DepositCheckPath, bytes.NewReader(DepositCheckBody(user)))
	if err != nil {
		return common.Address{}, err
	}
	setReadHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://exchange.papertrade.xyz")
	req.Header.Set("X-Papertrade-Build", DepositBuild)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return common.Address{}, &TransportError{Err: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return common.Address{}, &DepositCheckError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	var out struct {
		Proxy string `json:"proxy"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || !common.IsHexAddress(out.Proxy) {
		return common.Address{}, fmt.Errorf("deposit check: unexpected body: %s", strings.TrimSpace(string(raw)))
	}
	return common.HexToAddress(out.Proxy), nil
}
