package state

import (
	"strings"
	"testing"
)

func TestFormatLogEntry(t *testing.T) {
	cmd := FormatLogEntry(map[string]any{"ts": float64(0), "argv": []any{"buy", "BTC", "0.1"}, "exit": float64(20)})
	if !strings.Contains(cmd, "deliverator buy BTC 0.1") || !strings.Contains(cmd, "exit 20") {
		t.Errorf("command-log line: %q", cmd)
	}
	ok := FormatLogEntry(map[string]any{"ts": float64(0), "argv": []any{"mids"}, "exit": float64(0)})
	if !strings.Contains(ok, "→ ok") {
		t.Errorf("exit 0 should read ok: %q", ok)
	}
	au := FormatLogEntry(map[string]any{"ts": float64(0), "action": "order", "coin": "BTC", "status": "filled"})
	if !strings.Contains(au, "order") || !strings.Contains(au, "coin=BTC") || !strings.Contains(au, "status=filled") {
		t.Errorf("audit line: %q", au)
	}
	fb := FormatLogEntry(map[string]any{"ts": float64(0), "weird": "x"})
	if !strings.Contains(fb, "weird") {
		t.Errorf("unknown shape should fall back to raw JSON: %q", fb)
	}
}
