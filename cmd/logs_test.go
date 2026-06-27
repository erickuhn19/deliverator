package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/state"
)

func TestCommandLogPathResolution(t *testing.T) {
	saveCfg := Cfg
	t.Cleanup(func() { Cfg = saveCfg })

	// env override wins
	t.Setenv("DELIVERATOR_COMMAND_LOG", "/tmp/x.jsonl")
	if got := commandLogPath(); got != "/tmp/x.jsonl" {
		t.Fatalf("env override: got %q", got)
	}
	// no env -> config field
	t.Setenv("DELIVERATOR_COMMAND_LOG", "")
	Cfg = config.Default()
	Cfg.State.CommandLog = "/tmp/cfg.jsonl"
	if got := commandLogPath(); got != "/tmp/cfg.jsonl" {
		t.Fatalf("config field: got %q", got)
	}
	// neither -> off
	Cfg.State.CommandLog = ""
	if got := commandLogPath(); got != "" {
		t.Fatalf("off: got %q", got)
	}
}

func TestLogInvocationWritesArgvAndExit(t *testing.T) {
	log := filepath.Join(t.TempDir(), "cmds.jsonl")
	t.Setenv("DELIVERATOR_COMMAND_LOG", log)
	saveArgs := os.Args
	os.Args = []string{"deliverator", "buy", "BTC", "0.1"}
	t.Cleanup(func() { os.Args = saveArgs })

	logInvocation(60) // a partial-fill exit, say

	rows, err := state.ReadSince(log, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("want 1 logged invocation, got %d (%v)", len(rows), err)
	}
	r := rows[0]
	if r["exit"].(float64) != 60 || r["ok"].(bool) {
		t.Fatalf("exit/ok wrong: %+v", r)
	}
	argv, _ := r["argv"].([]any)
	if len(argv) != 3 || argv[0] != "buy" || argv[1] != "BTC" {
		t.Fatalf("argv not the command (without the binary name): %v", argv)
	}
}

func TestLogInvocationNoOpWhenUnconfigured(t *testing.T) {
	t.Setenv("DELIVERATOR_COMMAND_LOG", "")
	saveCfg := Cfg
	Cfg = nil
	t.Cleanup(func() { Cfg = saveCfg })
	logInvocation(0) // must not panic, must write nothing
}

func TestFormatLogEntry(t *testing.T) {
	cmd := formatLogEntry(map[string]any{"ts": float64(0), "argv": []any{"buy", "BTC", "0.1"}, "exit": float64(20)})
	if !strings.Contains(cmd, "deliverator buy BTC 0.1") || !strings.Contains(cmd, "exit 20") {
		t.Fatalf("command line format wrong: %q", cmd)
	}
	ok := formatLogEntry(map[string]any{"ts": float64(0), "argv": []any{"mids"}, "exit": float64(0)})
	if !strings.Contains(ok, "→ ok") {
		t.Fatalf("exit-0 should read ok: %q", ok)
	}
	au := formatLogEntry(map[string]any{"ts": float64(0), "action": "order", "coin": "BTC", "status": "filled"})
	if !strings.Contains(au, "order") || !strings.Contains(au, "coin=BTC") || !strings.Contains(au, "status=filled") {
		t.Fatalf("audit line format wrong: %q", au)
	}
}

func TestLogsCmdReadsAndFormats(t *testing.T) {
	log := filepath.Join(t.TempDir(), "cmds.jsonl")
	a := state.NewAudit(log, true)
	a.Append(map[string]any{"argv": []any{"mids"}, "exit": 0, "ok": true})
	a.Append(map[string]any{"argv": []any{"buy", "BTC"}, "exit": 10, "ok": false})
	t.Setenv("DELIVERATOR_COMMAND_LOG", log)

	saveF, saveA, saveN := logsFollow, logsAudit, logsTail
	logsFollow, logsAudit, logsTail = false, false, 20
	t.Cleanup(func() { logsFollow, logsAudit, logsTail = saveF, saveA, saveN })

	var buf bytes.Buffer
	logsCmd.SetOut(&buf)
	t.Cleanup(func() { logsCmd.SetOut(nil) })
	if err := logsCmd.RunE(logsCmd, nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "deliverator mids") || !strings.Contains(out, "buy BTC") || !strings.Contains(out, "exit 10") {
		t.Fatalf("logs output should list both commands formatted, got:\n%s", out)
	}
}

func TestLogsCmdErrorsWhenUnconfigured(t *testing.T) {
	t.Setenv("DELIVERATOR_COMMAND_LOG", "")
	saveCfg := Cfg
	Cfg = config.Default() // no command_log set
	t.Cleanup(func() { Cfg = saveCfg })
	saveA := logsAudit
	logsAudit = false
	t.Cleanup(func() { logsAudit = saveA })

	var buf bytes.Buffer
	logsCmd.SetOut(&buf)
	t.Cleanup(func() { logsCmd.SetOut(nil) })
	if err := logsCmd.RunE(logsCmd, nil); err == nil {
		t.Fatal("logs with no command log configured must error")
	}
}
