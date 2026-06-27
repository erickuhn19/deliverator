package output

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"
)

// SchemaVersion is the stable, additive-only output contract version (§5.1).
// Bump only on breaking changes; the agent asserts compatibility via `version`.
const SchemaVersion = "v1"

// Envelope is the single JSON object every command emits (§5.1). For streams,
// one Envelope is emitted per line (NDJSON).
type Envelope struct {
	Schema   string   `json:"schema"`
	OK       bool     `json:"ok"`
	Ts       int64    `json:"ts"` // server-aligned unix ms
	Cmd      string   `json:"cmd"`
	Data     any      `json:"data"`
	Error    *Error   `json:"error"`
	Warnings []string `json:"warnings"`
	Meta     Meta     `json:"meta"`
}

// Meta is the per-call context block (§5.1).
type Meta struct {
	Network    string `json:"network"`
	Account    string `json:"account"`
	WeightUsed int    `json:"weight_used,omitempty"`
}

var (
	jsonMode              = true
	writer      io.Writer = os.Stdout
	clockSkewMs int64     // server-time offset applied to Now()
)

// Configure sets the global render mode and destination. Called once at startup
// after global flags + TTY detection are resolved.
func Configure(jsonOut bool, w io.Writer) {
	jsonMode = jsonOut
	if w != nil {
		writer = w
	}
}

// SetClockSkew records a measured server-time offset (serverMs - localMs) so
// emitted ts values are server-aligned (§5.1).
func SetClockSkew(ms int64) { atomic.StoreInt64(&clockSkewMs, ms) }

// Now returns the server-aligned unix-ms timestamp used for ts.
func Now() int64 { return time.Now().UnixMilli() + atomic.LoadInt64(&clockSkewMs) }

// JSONMode reports whether output is machine JSON (vs human tables).
func JSONMode() bool { return jsonMode }

// Writer returns the current output destination.
func Writer() io.Writer { return writer }

// Response carries a successful command result.
type Response struct {
	Cmd      string
	Data     any
	Warnings []string
	Meta     Meta
}

// Emit writes a success envelope (one JSON line in JSON mode).
func Emit(r Response) {
	render(Envelope{
		Schema:   SchemaVersion,
		OK:       true,
		Ts:       Now(),
		Cmd:      r.Cmd,
		Data:     r.Data,
		Warnings: r.Warnings,
		Meta:     r.Meta,
	})
}

// Fail writes a failure envelope and returns a *CmdError carrying the exit code.
// A cobra RunE returns this; main() maps it to os.Exit without re-printing.
func Fail(cmd string, err *Error, meta Meta) error {
	render(Envelope{
		Schema: SchemaVersion,
		OK:     false,
		Ts:     Now(),
		Cmd:    cmd,
		Data:   nil,
		Error:  err,
		Meta:   meta,
	})
	return &CmdError{Code: err.ExitCode()}
}

func render(env Envelope) {
	if env.Warnings == nil {
		env.Warnings = []string{}
	}
	if jsonMode {
		enc := json.NewEncoder(writer)
		enc.SetEscapeHTML(false) // keep 0x.., URLs, and math symbols literal
		_ = enc.Encode(env)      // newline-terminated → NDJSON-friendly
		return
	}
	renderHuman(env)
}

// renderHuman is the compact form for an operator at a shell (--no-json). Agents
// always use JSON. Per-command pretty tables can layer on later; this generic
// form keeps every command human-readable today.
func renderHuman(env Envelope) {
	mark := "✓"
	if !env.OK {
		mark = "✗"
	}
	fmt.Fprintf(writer, "%s %s\n", mark, env.Cmd)
	for _, w := range env.Warnings {
		fmt.Fprintf(writer, "  ⚠ %s\n", w)
	}
	if env.Error != nil {
		fmt.Fprintf(writer, "  error [%s/%s]: %s\n", env.Error.Category, env.Error.Code, env.Error.Message)
		if env.Error.Hint != "" {
			fmt.Fprintf(writer, "  hint: %s\n", env.Error.Hint)
		}
		if env.Error.RetryAfterMs != nil {
			fmt.Fprintf(writer, "  retry after: %d ms\n", *env.Error.RetryAfterMs)
		}
		return
	}
	if env.Data != nil {
		b, _ := json.MarshalIndent(env.Data, "  ", "  ")
		fmt.Fprintf(writer, "  %s\n", string(b))
	}
}
