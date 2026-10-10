package papertrade

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// The exchange worker answers "Not found" unless the request carries browser
// fetch metadata. These headers are what a successful read used.
const browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// Client is the relayer + HyperEVM RPC. It does not sign.
type Client struct {
	Exchange string
	RPC      string
	Info     string
	HTTP     *http.Client
}

func NewClient(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &Client{
		Exchange: ExchangeURL(),
		RPC:      RPCURL(),
		HTTP: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Markets fetches and decodes /state/trading.
func (c *Client) Markets(ctx context.Context) (Markets, error) {
	body, err := c.get(ctx, "/state/trading")
	if err != nil {
		return Markets{}, err
	}
	return ParseMarkets(body)
}

// Summary returns the protocol summary JSON with numbers preserved (json.Number).
func (c *Client) Summary(ctx context.Context) (any, error) {
	body, err := c.get(ctx, "/query/protocol/summary")
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("protocol summary: %w", err)
	}
	return v, nil
}

// Account reads one wallet frame from the SSE stream and closes it.
func (c *Client) Account(ctx context.Context, wallet common.Address) (Account, error) {
	url := c.Exchange + "/state/user/live?wallet=" + strings.ToLower(wallet.Hex())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Account{}, err
	}
	setReadHeaders(req)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Account{}, fmt.Errorf("account stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Account{}, fmt.Errorf("account stream: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	frame, err := readWalletEvent(resp.Body)
	if err != nil {
		return Account{}, err
	}
	return ParseAccount(frame)
}

// DepositProxy calls computeProxyAddress(user) on the factory.
func (c *Client) DepositProxy(ctx context.Context, user common.Address) (common.Address, error) {
	data := proxyCalldata(user)
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_call",
		"params": []any{
			map[string]string{"to": DepositProxyFactory, "data": data},
			"latest",
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.RPC, bytes.NewReader(payload))
	if err != nil {
		return common.Address{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return common.Address{}, fmt.Errorf("deposit proxy: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return common.Address{}, fmt.Errorf("deposit proxy: %w", err)
	}
	if out.Error != nil {
		return common.Address{}, fmt.Errorf("deposit proxy: %s", out.Error.Message)
	}
	raw := strings.TrimPrefix(out.Result, "0x")
	if len(raw) < 40 {
		return common.Address{}, fmt.Errorf("deposit proxy: short result")
	}
	return common.HexToAddress(raw[len(raw)-40:]), nil
}

// proxyCalldata is computeProxyAddress(address).
func proxyCalldata(user common.Address) string {
	sig := crypto.Keccak256([]byte("computeProxyAddress(address)"))[:4]
	var word [32]byte
	copy(word[12:], user.Bytes())
	return "0x" + common.Bytes2Hex(sig) + common.Bytes2Hex(word[:])
}

// PostIntent posts a stored body. path is absolute on the exchange origin
// ("/intents" or "/intents/<id>/cancel").
func (c *Client) PostIntent(ctx context.Context, path string, body []byte) (*Receipt, error) {
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		return nil, fmt.Errorf("intent path")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Exchange+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	setReadHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &TransportError{Err: err, Sent: !preSend(err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &TransportError{Err: err, Sent: true}
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, &TransportError{Err: fmt.Errorf("HTTP %d", resp.StatusCode), Sent: true}
	}
	if resp.StatusCode >= 500 {
		return nil, &TransportError{Err: fmt.Errorf("HTTP %d", resp.StatusCode), Sent: true, Status: resp.StatusCode, Body: raw}
	}
	rec, perr := parseReceipt(raw)
	if resp.StatusCode == http.StatusTooManyRequests {
		if perr != nil {
			rec = &Receipt{Code: "rate_limited", Message: strings.TrimSpace(string(raw))}
		}
		rec.Status = resp.StatusCode
		return rec, nil
	}
	if resp.StatusCode >= 400 {
		if perr != nil {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		rec.Status = resp.StatusCode
		return rec, nil
	}
	if perr != nil {
		return nil, &TransportError{Err: perr, Sent: true, Status: resp.StatusCode, Body: raw}
	}
	rec.Status = resp.StatusCode
	return rec, nil
}

// Receipt is a relayer intent response. State is empty when the body was only
// an error. Happy states are queued, prepared, and confirmed.
type Receipt struct {
	Status        int    `json:"-"`
	OK            *bool  `json:"ok"`
	Action        string `json:"action"`
	IntentID      string `json:"intentId"`
	State         string `json:"state"`
	QueueRevision string `json:"queueRevision"`
	Code          string `json:"code"`
	Error         string `json:"error"`
	Message       string `json:"message"`
	Reason        string `json:"reason"`
}

func (r *Receipt) failureCode() string {
	if r == nil {
		return ""
	}
	if r.Code != "" {
		return r.Code
	}
	return r.Error
}

func parseReceipt(raw []byte) (*Receipt, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("empty relayer body")
	}
	var rec Receipt
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("relayer body: %w", err)
	}
	return &rec, nil
}

// TransportError distinguishes a request that never left the host from one
// that may have.
type TransportError struct {
	Err    error
	Sent   bool
	Status int
	Body   []byte
}

func (e *TransportError) Error() string { return e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

func preSend(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	return false
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Exchange+path, nil)
	if err != nil {
		return nil, err
	}
	setReadHeaders(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if strings.TrimSpace(string(body)) == "Not found" {
		return nil, fmt.Errorf("GET %s: exchange worker rejected the request", path)
	}
	return body, nil
}

func setReadHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Referer", "https://exchange.papertrade.xyz/house")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
}

func readWalletEvent(r io.Reader) ([]byte, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var event string
	var data []byte
	flush := func() ([]byte, bool) {
		if event == "wallet" && len(data) > 0 {
			return append([]byte(nil), data...), true
		}
		event, data = "", nil
		return nil, false
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if frame, ok := flush(); ok {
				return frame, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, value...)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if frame, ok := flush(); ok {
		return frame, nil
	}
	return nil, fmt.Errorf("account stream ended before a wallet frame")
}
