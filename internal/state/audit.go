package state

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Audit is an append-only JSONL log of every action + result (§8), making an
// autonomous trading session reviewable after the fact. Key material is NEVER
// passed in by callers — redaction is by construction (we only log actions and
// results, never secrets).
type Audit struct {
	path    string
	enabled bool
}

// NewAudit returns an audit logger. When disabled (or path empty) it is a no-op.
func NewAudit(path string, enabled bool) *Audit {
	return &Audit{path: path, enabled: enabled}
}

// Path returns the audit file path ("" when disabled/unset). It lets a reader
// (e.g. reconcile) consume exactly the file this logger writes — so the diff is
// always against the same trail, in production and tests alike.
func (a *Audit) Path() string {
	if a == nil {
		return ""
	}
	return a.path
}

// ReadSince reads the append-only audit JSONL and returns every row with
// ts >= sinceMs (sinceMs <= 0 returns all rows). A missing file is not an error
// (a fresh box has no audit yet) and malformed lines are skipped: reconciliation
// must degrade gracefully, never hard-fail on one bad line.
func ReadSince(path string, sinceMs int64) ([]map[string]any, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // tolerate long batch/bracket rows
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			continue // skip a corrupt line rather than abort the whole read
		}
		if sinceMs > 0 {
			ts, _ := m["ts"].(float64) // JSON numbers decode to float64
			if int64(ts) < sinceMs {
				continue
			}
		}
		out = append(out, m)
	}
	return out, sc.Err()
}

// Append writes one JSON line. Best-effort: I/O errors never block a command.
func (a *Audit) Append(entry map[string]any) {
	if a == nil || !a.enabled || a.path == "" {
		return
	}
	if _, ok := entry["ts"]; !ok {
		entry["ts"] = time.Now().UnixMilli()
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return
	}
	// Append-only, so temp+rename would lose the trail — instead refuse a
	// symlinked path and enforce 0600 on a pre-existing file. O_NOFOLLOW closes
	// the check→open TOCTOU window so a symlink planted after the Lstat can't
	// silently redirect/erase the money trail (audit #91 / T3-symlink).
	if err := ValidateStateFile(a.path); err != nil {
		return
	}
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_ = f.Chmod(0o600)
	_, _ = f.Write(append(b, '\n'))
}
