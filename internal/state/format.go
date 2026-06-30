package state

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// FormatLogEntry renders one JSONL log row (command-log or audit) as a compact
// human line. It is stream-agnostic — it dispatches on which key is present:
//
//	command-log: 15:04:05  $ deliverator buy BTC 0.1  → ok
//	audit:       15:04:05  order         coin=BTC side=buy size=0.1 status=filled
//
// Unknown shapes fall back to the raw JSON. Shared by `deliverator logs` and the
// console TUI activity feed so both render identically.
func FormatLogEntry(e map[string]any) string {
	ts := ""
	if v, ok := e["ts"].(float64); ok {
		ts = time.UnixMilli(int64(v)).Format("15:04:05")
	}
	switch {
	case e["argv"] != nil: // command-log line
		argv, _ := e["argv"].([]any)
		parts := make([]string, len(argv))
		for i, a := range argv {
			parts[i] = fmt.Sprint(a)
		}
		exit := 0
		if v, ok := e["exit"].(float64); ok {
			exit = int(v)
		}
		outcome := "ok"
		if exit != 0 {
			outcome = fmt.Sprintf("exit %d", exit)
		}
		return fmt.Sprintf("%s  $ deliverator %s  → %s", ts, strings.Join(parts, " "), outcome)
	case e["action"] != nil: // audit line
		action, _ := e["action"].(string)
		var kv []string
		for _, k := range []string{"coin", "side", "size", "status", "oid", "canceled", "secs", "complete"} {
			if v, ok := e[k]; ok {
				kv = append(kv, fmt.Sprintf("%s=%v", k, v))
			}
		}
		return fmt.Sprintf("%s  %-13s %s", ts, action, strings.Join(kv, " "))
	default:
		b, _ := json.Marshal(e)
		return fmt.Sprintf("%s  %s", ts, b)
	}
}
