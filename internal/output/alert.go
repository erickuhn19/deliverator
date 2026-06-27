package output

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// Alerting (#48): a provider-agnostic webhook that fires when a command fails with
// a RED-state category (halt/auth/timeout by default; risk/network/etc. opt-in).
// The operator points webhook_url at Slack/Discord/a relay; Deliverator POSTs a
// compact JSON event. Best-effort: errors are swallowed and never change the
// command's outcome. Because a CLI exits right after the failure, the POST is
// SYNCHRONOUS but bounded by a short timeout — it adds at most that timeout to a
// failing command, and only when alerting is configured.

// AlertEvent is the webhook payload.
type AlertEvent struct {
	Ts       int64  `json:"ts"`
	ExitCode int    `json:"exit_code"`
	Category string `json:"category"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Cmd      string `json:"cmd"`
	Network  string `json:"network"`
	Account  string `json:"account"`
}

// Emitter posts alerts to a webhook. The zero value (and a "" url) is a no-op.
type Emitter struct {
	url        string
	timeout    time.Duration
	categories map[string]bool
}

// defaultAlertCategories — the RED states an away operator must hear about.
var defaultAlertCategories = []string{string(CatHalt), string(CatAuth), string(CatTimeout)}

// NewEmitter builds an emitter. webhookURL "" => disabled. Empty categories =>
// the default RED set (halt/auth/timeout). timeoutSec <= 0 => 5s.
func NewEmitter(webhookURL string, categories []string, timeoutSec int) *Emitter {
	if webhookURL == "" {
		return &Emitter{}
	}
	if len(categories) == 0 {
		categories = defaultAlertCategories
	}
	set := make(map[string]bool, len(categories))
	for _, c := range categories {
		if c = strings.TrimSpace(strings.ToLower(c)); c != "" {
			set[c] = true
		}
	}
	to := 5 * time.Second
	if timeoutSec > 0 {
		to = time.Duration(timeoutSec) * time.Second
	}
	return &Emitter{url: webhookURL, timeout: to, categories: set}
}

// Enabled reports whether the emitter would send anything.
func (e *Emitter) Enabled() bool { return e != nil && e.url != "" }

// Fire POSTs the event if its category is in the configured set. Best-effort:
// any error (marshal/build/network/timeout/panic) is swallowed.
func (e *Emitter) Fire(evt AlertEvent) {
	if !e.Enabled() || !e.categories[evt.Category] {
		return
	}
	e.send(evt)
}

// FireAlways POSTs the event regardless of the category filter (still a no-op if
// no webhook is configured). Used when the caller has explicitly asked for an
// alert — e.g. `watch --action alert` — rather than the passive RED-state hook.
func (e *Emitter) FireAlways(evt AlertEvent) {
	if !e.Enabled() {
		return
	}
	e.send(evt)
}

// send POSTs the event body. Best-effort: any error (marshal/build/network/
// timeout/panic) is swallowed.
func (e *Emitter) send(evt AlertEvent) {
	defer func() { _ = recover() }()
	b, err := json.Marshal(evt)
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, e.url, bytes.NewReader(b))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: e.timeout}).Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}
