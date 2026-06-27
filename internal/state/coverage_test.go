package state

// Coverage for the concurrency-safety primitives the audit (#89) flagged at 0%:
// FileLock (the rate-cap/risk-state serializer), audit.Append (the money trail),
// and the nonce high-water + error-release paths.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- FileLock ---

func TestFileLockMutualExclusion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rate.lock")
	l1, err := Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan struct{})
	go func() {
		l2, e := Lock(p) // must block until l1.Unlock
		if e == nil {
			l2.Unlock()
		}
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("a second Lock acquired while the first was held")
	case <-time.After(150 * time.Millisecond):
		// good: still blocked
	}
	l1.Unlock()
	select {
	case <-acquired:
		// good: unblocked after Unlock
	case <-time.After(2 * time.Second):
		t.Fatal("the second Lock did not acquire after Unlock")
	}
}

func TestFileLockSerializesRMW(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "rmw.lock")
	dataPath := filepath.Join(dir, "rmw.dat")
	if err := os.WriteFile(dataPath, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := Lock(lockPath)
			if err != nil {
				return
			}
			defer l.Unlock()
			b, _ := os.ReadFile(dataPath)
			n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			_ = os.WriteFile(dataPath, []byte(strconv.Itoa(n+1)), 0o600)
		}()
	}
	wg.Wait()
	b, _ := os.ReadFile(dataPath)
	if strings.TrimSpace(string(b)) != "30" {
		t.Fatalf("serialized read-modify-write should reach 30, got %q (lock not excluding?)", b)
	}
}

func TestFileLockUnlockNilSafeAndIdempotent(t *testing.T) {
	var nilLock *FileLock
	nilLock.Unlock() // must not panic
	l, err := Lock(filepath.Join(t.TempDir(), "x.lock"))
	if err != nil {
		t.Fatal(err)
	}
	l.Unlock()
	l.Unlock() // idempotent — must not panic
}

func TestLockErrorOnBadParent(t *testing.T) {
	reg := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(reg, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(filepath.Join(reg, "sub.lock")); err == nil {
		t.Fatal("Lock under a regular-file parent should error")
	}
}

// --- Audit ---

func TestAuditAppendRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	a := NewAudit(p, true)
	if a.Path() != p {
		t.Errorf("Path() = %q, want %q", a.Path(), p)
	}
	a.Append(map[string]any{"action": "order", "oid": 1})
	a.Append(map[string]any{"action": "cancel", "oid": 2})
	rows, err := ReadSince(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("two Appends should produce two lines (append, not truncate), got %d", len(rows))
	}
	if rows[0]["action"] != "order" || rows[1]["action"] != "cancel" {
		t.Fatalf("writer->reader round-trip mismatch: %+v", rows)
	}
	if _, ok := rows[0]["ts"]; !ok {
		t.Error("Append must inject a ts when absent")
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("audit file mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestAuditPreservesProvidedTs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.jsonl")
	NewAudit(p, true).Append(map[string]any{"action": "x", "ts": int64(1234)})
	rows, _ := ReadSince(p, 0)
	if len(rows) != 1 || rows[0]["ts"].(float64) != 1234 {
		t.Fatalf("a provided ts must be preserved, got %+v", rows)
	}
}

func TestAuditNoOps(t *testing.T) {
	dir := t.TempDir()
	disabled := filepath.Join(dir, "disabled.jsonl")
	NewAudit(disabled, false).Append(map[string]any{"action": "x"})
	if _, err := os.Stat(disabled); !os.IsNotExist(err) {
		t.Error("a disabled audit must not write a file")
	}
	var nilAudit *Audit
	nilAudit.Append(map[string]any{"action": "x"})           // must not panic
	NewAudit("", true).Append(map[string]any{"action": "x"}) // empty path: no-op, no panic
}

// --- Nonce ---

func TestNonceHighWaterAndPersistence(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nonce.lock")
	h, err := NewNonceLock(p).Acquire()
	if err != nil {
		t.Fatal(err)
	}
	// Production path: Commit(now-ms) where highWater >> persisted+1.
	if err := h.Commit(1_700_000_000_000); err != nil {
		t.Fatal(err)
	}
	h2, err := NewNonceLock(p).Acquire() // persists across a fresh lock instance
	if err != nil {
		t.Fatal(err)
	}
	if got := h2.Persisted(); got != 1_700_000_000_000 {
		t.Fatalf("persisted = %d, want 1700000000000", got)
	}
	// A tiny highWater after a large commit must NEVER shrink the value (monotonic).
	if err := h2.Commit(5); err != nil {
		t.Fatal(err)
	}
	h3, _ := NewNonceLock(p).Acquire()
	if got := h3.Persisted(); got < 1_700_000_000_000 {
		t.Fatalf("nonce regressed to %d", got)
	}
	_ = h3.Release()
}

func TestNonceCommitErrorReleasesLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "n.lock")
	h, err := NewNonceLock(p).Acquire()
	if err != nil {
		t.Fatal(err)
	}
	_ = h.f.Close() // force Commit's Seek/Write to fail -> releaseWith error path
	if err := h.Commit(123); err == nil {
		t.Fatal("Commit on a broken handle should error")
	}
	// The lock must be released (not wedged) so the next process can acquire.
	done := make(chan struct{})
	go func() {
		h2, e := NewNonceLock(p).Acquire()
		if e == nil {
			_ = h2.Release()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a Commit error wedged the lock — the next Acquire blocked")
	}
}

func TestNonceNilReleaseSafe(t *testing.T) {
	var h *NonceHandle
	if err := h.Release(); err != nil {
		t.Errorf("nil Release should be a no-op, got %v", err)
	}
}
