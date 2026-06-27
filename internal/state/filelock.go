package state

import (
	"os"
	"path/filepath"
	"syscall"
)

// FileLock is a simple advisory flock used to serialize a read-modify-write of a
// small state file across concurrent one-shot `deliverator` processes (e.g. the
// local rate-cap log). It coordinates on the inode, so holding it around plain
// os.ReadFile/os.WriteFile of a sibling file is sufficient.
type FileLock struct{ f *os.File }

// Lock opens path (creating it) and takes the exclusive flock, blocking until it
// is available. The caller MUST Unlock.
func Lock(path string) (*FileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Refuse a symlinked lockfile and enforce 0600 on a pre-existing one.
	// O_NOFOLLOW closes the check→open TOCTOU window atomically so a planted
	// symlink can't redirect the rate-cap lock and defeat the RMW serialization
	// risk.go relies on (audit #91 / T3-symlink, T3-file-mode).
	if err := ValidateStateFile(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &FileLock{f: f}, nil
}

// Unlock releases the flock and closes the handle. Idempotent.
func (l *FileLock) Unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
