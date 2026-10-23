package papertrade

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"golang.org/x/text/unicode/norm"
)

// Builder codes. Sources, checked 2026-10-23 against the live venue:
//
//   - https://docs.papertrade.xyz/how/builder-codes/
//   - https://docs.papertrade.xyz/learn/session-keys/
//   - https://docs.papertrade.xyz/dev/contract-addresses/
//   - https://docs.papertrade.xyz/dev/protocol-changes/
//   - exchange.papertrade.xyz client (builders-CsJAIjbf.js, app bundle, deployment)
//
// A builder code is an address named as feeAddress on RegisterSessionKey. It is
// not a field on open or close. While that address is an approved builder,
// closes the key signs pay it the protocol fee rate (live: 0.5% of adjusted
// PnL, owner-settable through the timelock up to 1%). The trader pays it on a
// win, on top of the win fee. On a loss it comes out of the LP. Liquidations,
// including a close at or past the bust price, pay none. The zero address names
// no builder. papertrade.xyz signs the zero address.
//
// There is no trader approveBuilderFee transaction. Signing RegisterSessionKey
// with the fee address is the approval. The contract owner approves builders
// through the 24-hour timelock; the multisig can remove one immediately.
// Deliverator cannot approve or remove a builder.

const (
	// BuilderApplicationEnvironment is the environment id the production client
	// writes into the signed application (deployment.environmentId).
	BuilderApplicationEnvironment = "production"

	// BuilderFeeWad is the scale of BuilderRegistry.feeRate(). 1e18 is 100%.
	// The live rate on 2026-10-23 was 5e15 (0.5%).
	builderFeeWadExp = 18

	// BuilderFeeRateCapRaw is the largest fee the owner can schedule. The
	// registry's rate-setting function reverts above PUSH7 0x2386f26fc10000,
	// which is 1e16 = 1% of 1e18. The docs state the same 1% cap.
	BuilderFeeRateCapRaw = "10000000000000000"
)

// BuilderApplication is the normalized form POST /query/builders/applications
// signs. Field rules match the venue client.
type BuilderApplication struct {
	AppName     string
	WebsiteURL  string
	ContactKind string
	Contact     string
	Integration string
}

// FieldError names the application field the venue client would reject.
type FieldError struct{ Field string }

func (e *FieldError) Error() string {
	if e == nil || e.Field == "" {
		return "builder application is invalid"
	}
	return "builder application field " + e.Field + " is invalid"
}

var (
	websiteRe  = regexp.MustCompile(`^https://[^\s/?#@]+\.[^\s/?#@]+(?:[/?#]\S*)?$`)
	emailRe    = regexp.MustCompile(`^[^\s@]{1,64}@[^\s@]+\.[^\s@]{2,}$`)
	telegramRe = regexp.MustCompile(`^[A-Za-z0-9_]{5,32}$`)
	xRe        = regexp.MustCompile(`^[A-Za-z0-9_]{1,15}$`)
)

// NormalizeApplication applies the venue client's field rules. The signed
// message is built from the returned value, not from the raw flags.
func NormalizeApplication(appName, website, kind, contact, integration string) (BuilderApplication, error) {
	name := collapseSpace(norm.NFKC.String(appName))
	if utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 64 || hasControl(name) {
		return BuilderApplication{}, &FieldError{Field: "appName"}
	}
	site := collapseSpace(norm.NFKC.String(website))
	if len(site) > 200 || hasControl(site) || !websiteRe.MatchString(site) {
		return BuilderApplication{}, &FieldError{Field: "websiteUrl"}
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case "telegram", "x", "email":
	default:
		return BuilderApplication{}, &FieldError{Field: "contactKind"}
	}
	who := collapseSpace(norm.NFKC.String(contact))
	if kind == "email" {
		who = strings.ToLower(who)
	} else {
		who = strings.TrimPrefix(who, "@")
	}
	if len(who) > 254 || hasControl(who) || !contactOK(kind, who) {
		return BuilderApplication{}, &FieldError{Field: "contact"}
	}
	text := normalizeIntegration(integration)
	if utf8.RuneCountInString(text) > 2000 || hasControl(strings.ReplaceAll(text, "\n", "")) {
		return BuilderApplication{}, &FieldError{Field: "integration"}
	}
	if text == "" {
		return BuilderApplication{}, &FieldError{Field: "integration"}
	}
	return BuilderApplication{
		AppName: name, WebsiteURL: site, ContactKind: kind, Contact: who, Integration: text,
	}, nil
}

func contactOK(kind, who string) bool {
	switch kind {
	case "email":
		return emailRe.MatchString(who)
	case "telegram":
		return telegramRe.MatchString(who)
	case "x":
		return xRe.MatchString(who)
	default:
		return false
	}
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return true
		}
	}
	return false
}

func collapseSpace(s string) string {
	var b strings.Builder
	prev := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !prev {
				b.WriteByte(' ')
				prev = true
			}
			continue
		}
		prev = false
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func normalizeIntegration(s string) string {
	s = norm.NFKC.String(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		if r == '\n' {
			b.WriteByte('\n')
			prevSpace = false
			continue
		}
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	lines := strings.Split(b.String(), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	s = strings.Join(lines, "\n")
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
}

// ApplicationMessage is the exact personal_sign text. signedAtMs is Unix
// milliseconds. address is the lowercase builder that will submit it.
func ApplicationMessage(address, environment string, app BuilderApplication, signedAtMs int64) string {
	return strings.Join([]string{
		"Papertrade builder application",
		"Address: " + address,
		"Environment: " + environment,
		"App: " + app.AppName,
		"Website: " + app.WebsiteURL,
		"Contact: " + app.ContactKind + " " + app.Contact,
		"Timestamp: " + strconv.FormatInt(signedAtMs, 10),
		"",
		"Integration:",
		app.Integration,
		"",
		"This signature only submits a builder application. It moves no funds and approves nothing.",
	}, "\n")
}

// SignPersonal signs message the way viem signMessage does: EIP-191, v = 27 or 28.
func SignPersonal(key *ecdsa.PrivateKey, message string) (string, error) {
	if key == nil {
		return "", fmt.Errorf("nil signing key")
	}
	hash := accounts.TextHash([]byte(message))
	sig, err := crypto.Sign(hash, key)
	if err != nil {
		return "", err
	}
	sig[64] += 27
	return "0x" + hex.EncodeToString(sig), nil
}

// ParseBuilderAddress parses a builder code. "none", empty, and the zero
// address all mean no builder. Any other value must be 20 bytes of hex.
func ParseBuilderAddress(s string) (common.Address, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "none") {
		return common.Address{}, nil
	}
	if !common.IsHexAddress(s) {
		return common.Address{}, fmt.Errorf("builder address must be 0x and 40 hex characters, or none")
	}
	return common.HexToAddress(s), nil
}

// ResolveBuilder chooses the fee address for a session registration.
// flag is the --builder value; a non-empty flag wins, including an explicit
// zero address, so one registration can opt out of a configured builder.
// configured is papertrade.builder_address. Both empty means the zero address.
func ResolveBuilder(flag, configured string) (common.Address, error) {
	if strings.TrimSpace(flag) != "" {
		return ParseBuilderAddress(flag)
	}
	if strings.TrimSpace(configured) != "" {
		return ParseBuilderAddress(configured)
	}
	return common.Address{}, nil
}

// FormatFeeRate renders a feeRate() wad as a percent. 5e15 is "0.5%".
func FormatFeeRate(raw *big.Int) string {
	if raw == nil || raw.Sign() < 0 {
		return ""
	}
	num := new(big.Int).Mul(raw, big.NewInt(100))
	den := new(big.Int).Exp(big.NewInt(10), big.NewInt(builderFeeWadExp), nil)
	s := new(big.Rat).SetFrac(num, den).FloatString(4)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-" {
		s = "0"
	}
	return s + "%"
}

// FeeWarning is the operator-facing note attached when a registration names a
// builder. rate is a formatted percent ("0.5%"); empty falls back to a generic
// phrase so a missing rate read does not invent one.
func FeeWarning(builder common.Address, rate string) string {
	if builder == (common.Address{}) {
		return ""
	}
	cost := "the live builder fee"
	if rate != "" {
		cost = rate + " of adjusted PnL"
	}
	return fmt.Sprintf(
		"this session key will name Papertrade builder %s. While that builder stays approved, closes the key signs pay it %s: the trader pays that on a win, on top of the win fee; on a loss it comes out of the LP. Liquidations pay none. Open and close do not take a builder. Register again to change it.",
		addr(builder), cost,
	)
}

// BoundFeeWarning is the shorter note for a key that is already registered
// with a non-zero fee address.
func BoundFeeWarning(builder common.Address) string {
	if builder == (common.Address{}) {
		return ""
	}
	return fmt.Sprintf(
		"this session key names Papertrade builder %s; closes it signs pay that builder while it stays approved (the trader pays the fee on a win)",
		addr(builder),
	)
}

// Leaderboard is GET /query/builders/leaderboard, reduced to the fields the
// CLI shows. Chart series are left on the venue.
type Leaderboard struct {
	Registry   string
	FeeRateRaw string
	AsOfMs     int64
	Builders   int64
	Users      int64
	Closes     int64
	AccruedRaw string
	Rows       []LeaderboardRow
}

// LeaderboardRow is one approved builder on the board.
type LeaderboardRow struct {
	Address    string
	Name       string
	Users      int64
	Closes     int64
	AccruedRaw string
}

// BuilderStats is GET /query/builders/{address}/stats, without chart series.
type BuilderStats struct {
	Address      string
	Approved     bool
	UnclaimedRaw string
	Users        int64
	SessionKeys  int64
	Closes       int64
	NotionalRaw  string
	AsOfMs       int64
	AccruedRaw   string
	ClaimedRaw   string
	PaidRaw      string
	HasEarnings  bool
}

// BuilderApplicationView is GET /query/builders/{address}/application.
// Status is "", "pending", "approved", or "rejected". Known is false when the
// field was absent rather than JSON null.
type BuilderApplicationView struct {
	Address       string
	Status        string
	StatusKnown   bool
	SubmittedAtMs int64
	HasSubmitted  bool
}

// ParseLeaderboard decodes the venue leaderboard document.
func ParseLeaderboard(body []byte) (Leaderboard, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw struct {
		OK         bool            `json:"ok"`
		Registry   json.RawMessage `json:"registry"`
		FeeRateRaw json.RawMessage `json:"feeRateRaw"`
		AsOfMs     json.Number     `json:"asOfMs"`
		Totals     struct {
			Builders   json.Number `json:"builders"`
			Users      json.Number `json:"users"`
			Closes     json.Number `json:"closes"`
			AccruedRaw string      `json:"accruedRaw"`
		} `json:"totals"`
		Builders []struct {
			Address    string      `json:"address"`
			Name       *string     `json:"name"`
			Users      json.Number `json:"users"`
			Closes     json.Number `json:"closes"`
			AccruedRaw string      `json:"accruedRaw"`
		} `json:"builders"`
	}
	if err := dec.Decode(&raw); err != nil {
		return Leaderboard{}, fmt.Errorf("builder leaderboard: %w", err)
	}
	if !raw.OK {
		return Leaderboard{}, fmt.Errorf("builder leaderboard: ok is not true")
	}
	reg, err := optionalAddress(raw.Registry)
	if err != nil {
		return Leaderboard{}, fmt.Errorf("builder leaderboard registry: %w", err)
	}
	rate, err := optionalDecimal(raw.FeeRateRaw)
	if err != nil {
		return Leaderboard{}, fmt.Errorf("builder leaderboard fee rate: %w", err)
	}
	asOf, err := raw.AsOfMs.Int64()
	if err != nil {
		return Leaderboard{}, fmt.Errorf("builder leaderboard asOfMs: %w", err)
	}
	out := Leaderboard{
		Registry: reg, FeeRateRaw: rate, AsOfMs: asOf,
		Builders: numOr(raw.Totals.Builders, 0), Users: numOr(raw.Totals.Users, 0),
		Closes: numOr(raw.Totals.Closes, 0), AccruedRaw: raw.Totals.AccruedRaw,
	}
	for i, row := range raw.Builders {
		if !common.IsHexAddress(row.Address) {
			return Leaderboard{}, fmt.Errorf("builder leaderboard row %d address", i)
		}
		item := LeaderboardRow{
			Address: addr(common.HexToAddress(row.Address)),
			Users:   numOr(row.Users, 0), Closes: numOr(row.Closes, 0), AccruedRaw: row.AccruedRaw,
		}
		if row.Name != nil {
			item.Name = *row.Name
		}
		out.Rows = append(out.Rows, item)
	}
	return out, nil
}

// ParseBuilderStats decodes one builder's stats document.
func ParseBuilderStats(body []byte) (BuilderStats, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw struct {
		OK           bool   `json:"ok"`
		Address      string `json:"address"`
		Approved     bool   `json:"approved"`
		UnclaimedRaw string `json:"unclaimedRaw"`
		Routing      struct {
			Users       json.Number `json:"users"`
			SessionKeys json.Number `json:"sessionKeys"`
			Closes      json.Number `json:"closes"`
			NotionalRaw string      `json:"notionalRaw"`
			AsOfMs      json.Number `json:"asOfMs"`
		} `json:"routing"`
		Earnings *struct {
			AccruedRaw string `json:"accruedRaw"`
			ClaimedRaw string `json:"claimedRaw"`
			PaidRaw    string `json:"paidRaw"`
		} `json:"earnings"`
	}
	if err := dec.Decode(&raw); err != nil {
		return BuilderStats{}, fmt.Errorf("builder stats: %w", err)
	}
	if !raw.OK || !common.IsHexAddress(raw.Address) {
		return BuilderStats{}, fmt.Errorf("builder stats: missing address")
	}
	asOf, err := raw.Routing.AsOfMs.Int64()
	if raw.Routing.AsOfMs != "" && err != nil {
		return BuilderStats{}, fmt.Errorf("builder stats asOfMs: %w", err)
	}
	out := BuilderStats{
		Address: addr(common.HexToAddress(raw.Address)), Approved: raw.Approved,
		UnclaimedRaw: raw.UnclaimedRaw,
		Users:        numOr(raw.Routing.Users, 0), SessionKeys: numOr(raw.Routing.SessionKeys, 0),
		Closes: numOr(raw.Routing.Closes, 0), NotionalRaw: raw.Routing.NotionalRaw, AsOfMs: asOf,
	}
	if out.UnclaimedRaw != "" {
		if _, ok := new(big.Int).SetString(out.UnclaimedRaw, 10); !ok {
			return BuilderStats{}, fmt.Errorf("builder stats unclaimedRaw")
		}
	}
	if raw.Earnings != nil {
		out.HasEarnings = true
		out.AccruedRaw, out.ClaimedRaw, out.PaidRaw = raw.Earnings.AccruedRaw, raw.Earnings.ClaimedRaw, raw.Earnings.PaidRaw
	}
	return out, nil
}

// ParseBuilderApplication decodes the application document.
func ParseBuilderApplication(body []byte) (BuilderApplicationView, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw struct {
		OK            bool            `json:"ok"`
		Address       string          `json:"address"`
		Status        json.RawMessage `json:"status"`
		SubmittedAtMs json.RawMessage `json:"submittedAtMs"`
	}
	if err := dec.Decode(&raw); err != nil {
		return BuilderApplicationView{}, fmt.Errorf("builder application: %w", err)
	}
	if !raw.OK || !common.IsHexAddress(raw.Address) {
		return BuilderApplicationView{}, fmt.Errorf("builder application: missing address")
	}
	out := BuilderApplicationView{Address: addr(common.HexToAddress(raw.Address))}
	if len(bytes.TrimSpace(raw.Status)) > 0 {
		out.StatusKnown = true
		if string(bytes.TrimSpace(raw.Status)) != "null" {
			var status string
			if err := json.Unmarshal(raw.Status, &status); err != nil {
				return BuilderApplicationView{}, fmt.Errorf("builder application status: %w", err)
			}
			switch status {
			case "pending", "approved", "rejected":
				out.Status = status
			default:
				return BuilderApplicationView{}, fmt.Errorf("builder application status %q", status)
			}
		}
	}
	if ms, ok, err := optionalInt(raw.SubmittedAtMs); err != nil {
		return BuilderApplicationView{}, fmt.Errorf("builder application submittedAtMs: %w", err)
	} else if ok {
		out.SubmittedAtMs = ms
		out.HasSubmitted = true
	}
	return out, nil
}

func optionalInt(raw json.RawMessage) (int64, bool, error) {
	s, err := optionalDecimal(raw)
	if err != nil || s == "" {
		return 0, false, err
	}
	n := json.Number(s)
	i, err := n.Int64()
	if err != nil {
		return 0, false, err
	}
	return i, true, nil
}

func optionalAddress(raw json.RawMessage) (string, error) {
	s, err := optionalDecimal(raw)
	if err != nil || s == "" {
		return "", err
	}
	if !common.IsHexAddress(s) {
		return "", fmt.Errorf("not an address")
	}
	return addr(common.HexToAddress(s)), nil
}

func optionalDecimal(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return "", err
	}
	return n.String(), nil
}

func builderPath(address common.Address, suffix string) (string, error) {
	if address == (common.Address{}) && suffix != "" {
		return "", fmt.Errorf("builder address is empty")
	}
	hexAddr := addr(address)
	if !common.IsHexAddress(hexAddr) {
		return "", fmt.Errorf("builder address")
	}
	return "/query/builders/" + hexAddr + suffix, nil
}

// BuilderLeaderboard fetches GET /query/builders/leaderboard.
func (c *Client) BuilderLeaderboard(ctx context.Context) (Leaderboard, error) {
	body, err := c.get(ctx, "/query/builders/leaderboard")
	if err != nil {
		return Leaderboard{}, err
	}
	return ParseLeaderboard(body)
}

// BuilderStats fetches GET /query/builders/{address}/stats.
func (c *Client) BuilderStats(ctx context.Context, address common.Address) (BuilderStats, error) {
	path, err := builderPath(address, "/stats")
	if err != nil {
		return BuilderStats{}, err
	}
	body, err := c.get(ctx, path)
	if err != nil {
		return BuilderStats{}, err
	}
	view, err := ParseBuilderStats(body)
	if err != nil {
		return BuilderStats{}, err
	}
	if view.Address != addr(address) {
		return BuilderStats{}, fmt.Errorf("builder stats address mismatch")
	}
	return view, nil
}

// BuilderApplication fetches GET /query/builders/{address}/application.
func (c *Client) BuilderApplication(ctx context.Context, address common.Address) (BuilderApplicationView, error) {
	path, err := builderPath(address, "/application")
	if err != nil {
		return BuilderApplicationView{}, err
	}
	body, err := c.get(ctx, path)
	if err != nil {
		return BuilderApplicationView{}, err
	}
	view, err := ParseBuilderApplication(body)
	if err != nil {
		return BuilderApplicationView{}, err
	}
	if view.Address != addr(address) {
		return BuilderApplicationView{}, fmt.Errorf("builder application address mismatch")
	}
	return view, nil
}

// ApplicationReceipt is a successful POST /query/builders/applications.
type ApplicationReceipt struct {
	Address            string
	Status             string
	SubmittedAtMs      int64
	NextSubmissionAtMs int64
}

// ApplicationError is a known rejection, or an unknown outcome after the POST
// may have been received.
type ApplicationError struct {
	Status   int
	Code     string
	Field    string
	Message  string
	NextAtMs int64
	HasNext  bool
	Unknown  bool
}

func (e *ApplicationError) Error() string {
	if e == nil {
		return "builder application"
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Code != "" {
		return e.Code
	}
	return "builder application rejected"
}

// SubmitApplication POSTs a signed builder application. The signature is
// personal_sign over ApplicationMessage. This does not approve the builder;
// the venue reviews it, and the contract owner schedules approval through the
// timelock.
func (c *Client) SubmitApplication(ctx context.Context, signer common.Address, app BuilderApplication, signedAtMs int64, signature string) (*ApplicationReceipt, error) {
	if !strings.HasPrefix(signature, "0x") || len(signature) != 132 {
		return nil, fmt.Errorf("builder application signature")
	}
	payload := map[string]any{
		"address": addr(signer), "appName": app.AppName, "websiteUrl": app.WebsiteURL,
		"contactKind": app.ContactKind, "contact": app.Contact, "integration": app.Integration,
		"signedAtMs": signedAtMs, "signature": signature,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	raw, status, err := c.postJSON(ctx, "/query/builders/applications", body)
	if err != nil {
		return nil, err
	}
	if status == http.StatusTooManyRequests {
		return nil, &ApplicationError{Status: status, Code: "rate_limited", Message: "builder application was rate-limited"}
	}
	if status >= 500 {
		return nil, &ApplicationError{Status: status, Unknown: true, Message: fmt.Sprintf("builder application HTTP %d; it may have been saved", status)}
	}
	if status >= 400 {
		return nil, parseApplicationError(status, raw)
	}
	rec, err := parseApplicationReceipt(raw)
	if err != nil || rec.Address != addr(signer) {
		return nil, &ApplicationError{Status: status, Unknown: true, Message: "builder application response could not be confirmed; check application status before applying again"}
	}
	return rec, nil
}

func parseApplicationReceipt(raw []byte) (*ApplicationReceipt, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var body struct {
		OK                 bool        `json:"ok"`
		Address            string      `json:"address"`
		Status             string      `json:"status"`
		SubmittedAtMs      json.Number `json:"submittedAtMs"`
		NextSubmissionAtMs json.Number `json:"nextSubmissionAtMs"`
	}
	if err := dec.Decode(&body); err != nil {
		return nil, err
	}
	if !body.OK || body.Status != "pending" || !common.IsHexAddress(body.Address) {
		return nil, fmt.Errorf("application receipt")
	}
	sub, err1 := body.SubmittedAtMs.Int64()
	next, err2 := body.NextSubmissionAtMs.Int64()
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("application receipt times")
	}
	return &ApplicationReceipt{
		Address: addr(common.HexToAddress(body.Address)), Status: body.Status,
		SubmittedAtMs: sub, NextSubmissionAtMs: next,
	}, nil
}

func parseApplicationError(status int, raw []byte) *ApplicationError {
	var body struct {
		Error              string      `json:"error"`
		Code               string      `json:"code"`
		Field              string      `json:"field"`
		NextSubmissionAtMs json.Number `json:"nextSubmissionAtMs"`
	}
	_ = json.Unmarshal(raw, &body)
	out := &ApplicationError{Status: status, Code: body.Code, Field: body.Field, Message: body.Error}
	if body.NextSubmissionAtMs != "" {
		if ms, err := body.NextSubmissionAtMs.Int64(); err == nil {
			out.NextAtMs = ms
			out.HasNext = true
		}
	}
	if out.Code == "" && out.Message == "" {
		out.Message = strings.TrimSpace(string(raw))
	}
	return out
}

func (c *Client) postJSON(ctx context.Context, path string, body []byte) ([]byte, int, error) {
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		return nil, 0, fmt.Errorf("path")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Exchange+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	setReadHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, &TransportError{Err: err, Sent: !preSend(err)}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, &TransportError{Err: err, Sent: true, Status: resp.StatusCode}
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return raw, resp.StatusCode, &TransportError{Err: fmt.Errorf("HTTP %d", resp.StatusCode), Sent: true, Status: resp.StatusCode, Body: raw}
	}
	return raw, resp.StatusCode, nil
}
