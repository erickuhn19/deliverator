// Package state holds machine-local coordination with no daemon (§8): a
// flock-guarded nonce high-water mark shared across concurrent one-shot
// processes, an append-only audit log, and (future) account registry helpers.
package state

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// NonceLock coordinates a monotonic nonce across overlapping `deliverator`
// processes that sign with the same agent key. Hyperliquid keeps only the 100
// highest nonces per signer and requires each new nonce to exceed the prior;
// two processes picking the same `time.Now().UnixMilli()` would collide. The
// flock serializes the critical section and the persisted high-water mark
// guarantees strict monotonicity. (Daemon-grade safety without a daemon.)
type NonceLock struct{ path string }

// NewNonceLock returns a lock backed by the given lockfile path.
func NewNonceLock(path string) *NonceLock { return &NonceLock{path: path} }

// NonceHandle is an acquired exclusive lock plus the persisted high-water mark.
type NonceHandle struct {
	f         *os.File
	persisted int64
}

// Acquire takes the exclusive flock (blocking) and reads the persisted nonce.
// The caller MUST Commit or Release to unlock.
func (n *NonceLock) Acquire() (*NonceHandle, error) {
	if err := os.MkdirAll(filepath.Dir(n.path), 0o700); err != nil {
		return nil, err
	}
	// The nonce file is the cross-process monotonicity guarantee — refuse a
	// symlinked path. ValidateStateFile gives a clear error for a pre-existing
	// symlink; O_NOFOLLOW closes the check→open TOCTOU window atomically (a
	// symlink planted after the Lstat makes the open fail with ELOOP), so a race
	// can't redirect the flock to an attacker file (audit #91 / T3-symlink).
	if err := ValidateStateFile(n.path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(n.path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	// 0600 is honored only on create; enforce it on a pre-existing file too.
	if err := os.Chmod(n.path, 0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	b, _ := io.ReadAll(f)
	persisted, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return &NonceHandle{f: f, persisted: persisted}, nil
}

// Persisted returns the high-water nonce from the last committed write. The
// caller floors the signer at this value (e.g. Exchange.SetLastNonce) so the
// next generated nonce strictly exceeds anything a prior process used.
func (h *NonceHandle) Persisted() int64 { return h.persisted }

// Commit persists max(persisted+1, highWater) and releases the lock. The
// persisted+1 floor is the safety invariant: after SetLastNonce(persisted) the
// SDK's next nonce is at least persisted+1, so even if the supplied highWater
// (e.g. time.Now().UnixMilli()) is behind the clock, we never under-count and
// hand the same nonce to the next process.
func (h *NonceHandle) Commit(highWater int64) error {
	v := h.persisted + 1
	if highWater > v {
		v = highWater
	}
	if _, err := h.f.Seek(0, io.SeekStart); err != nil {
		return h.releaseWith(err)
	}
	if err := h.f.Truncate(0); err != nil {
		return h.releaseWith(err)
	}
	if _, err := h.f.WriteString(strconv.FormatInt(v, 10)); err != nil {
		return h.releaseWith(err)
	}
	_ = h.f.Sync()
	return h.Release()
}

// Release unlocks without persisting (use on the read/error path). Idempotent.
func (h *NonceHandle) Release() error {
	if h == nil || h.f == nil {
		return nil
	}
	_ = syscall.Flock(int(h.f.Fd()), syscall.LOCK_UN)
	err := h.f.Close()
	h.f = nil
	return err
}

func (h *NonceHandle) releaseWith(err error) error {
	_ = h.Release()
	return err
}
