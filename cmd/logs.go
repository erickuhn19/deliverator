package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/erickuhn19/deliverator/internal/config"
	"github.com/erickuhn19/deliverator/internal/output"
	"github.com/erickuhn19/deliverator/internal/state"
)

var (
	logsFollow bool
	logsAudit  bool
	logsTail   int
)

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Watch what the CLI/agent runs — the command log (or --audit money trail), formatted; -f to follow live",
	Long: `Stream the local logs so a human can watch what an agent is doing in a second
terminal. The COMMAND log (one line per CLI invocation: argv + exit) requires
state.command_log or $DELIVERATOR_COMMAND_LOG to be set. The --audit trail (every
signed action) is on by default. Both are plain JSONL on disk — pipe to jq for
machine use; this command formats them for humans.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		path := commandLogPath()
		if logsAudit {
			path = filepath.Join(config.Dir(), "audit.jsonl")
			if Cfg != nil && Cfg.State.AuditPath != "" {
				path = config.ExpandPath(Cfg.State.AuditPath)
			}
		}
		if path == "" {
			return fail("logs", output.Validation("no_log",
				"no command log is configured").
				WithHint("set state.command_log or $DELIVERATOR_COMMAND_LOG, or use --audit for the trade trail"))
		}

		w := cmd.OutOrStdout()
		rows, err := state.ReadSince(path, 0)
		if err != nil {
			return fail("logs", output.Unknown("read_log", err.Error()))
		}
		from := 0
		if logsTail > 0 && len(rows) > logsTail {
			from = len(rows) - logsTail
		}
		for _, r := range rows[from:] {
			fmt.Fprintln(w, formatLogEntry(r))
		}
		if !logsFollow {
			return nil
		}
		// Follow until interrupted (Ctrl-C). The file is JSONL appended by other
		// `deliverator` processes; we print only NEW lines from end-of-file.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return followLog(ctx, w, path)
	},
}

// formatLogEntry renders one JSONL log row (command-log or audit) as a compact
// human line. Unknown shapes fall back to the raw JSON.
func formatLogEntry(e map[string]any) string {
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

// followLog tails path, printing each newly-appended JSONL line formatted, until
// ctx is cancelled. Offset-based so a partial line (writer mid-append) is held
// until complete.
func followLog(ctx context.Context, w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fail("logs", output.Unknown("open_log", err.Error()))
	}
	defer f.Close()
	off, _ := f.Seek(0, io.SeekEnd)
	var pending []byte
	for {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return nil
		}
		chunk, _ := io.ReadAll(f)
		if len(chunk) > 0 {
			off += int64(len(chunk))
			pending = append(pending, chunk...)
			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				line := pending[:i]
				pending = pending[i+1:]
				var m map[string]any
				if json.Unmarshal(bytes.TrimSpace(line), &m) == nil {
					fmt.Fprintln(w, formatLogEntry(m))
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(400 * time.Millisecond):
		}
	}
}

func init() {
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "stream new entries live until Ctrl-C")
	logsCmd.Flags().BoolVar(&logsAudit, "audit", false, "follow the audit (signed-action) trail instead of the command log")
	logsCmd.Flags().IntVarP(&logsTail, "tail", "n", 20, "show the last N existing entries before following (0 = all)")
	rootCmd.AddCommand(logsCmd)
}
