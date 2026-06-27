package state

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ValidateStateFile rejects a path that already exists as a symlink. A symlink
// planted at a state-file path (config.toml, nonce.lock, risk_state.json,
// audit.jsonl, meta.json) would otherwise be followed — letting a local
// attacker redirect a 0600 write, or feed a poisoned coin→assetId cache that
// makes orders sign against the wrong market. A non-existent path is fine (the
// common create case); any other Lstat error is returned so the caller fails
// closed rather than writing blind. (audit #91 / T3-symlink)
func ValidateStateFile(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to use state file %q: it is a symlink", path)
	}
	return nil
}

// WriteFileAtomic writes data to path crash-safely and with perm enforced:
//   - refuses to follow a symlink planted at path (ValidateStateFile);
//   - writes a fresh perm-mode temp file in the same dir, so a pre-existing
//     world/group-readable file's mode cannot survive the write (the temp is
//     created anew and renamed over the target — audit #91 / T3-file-mode);
//   - fsyncs the file, renames over path (atomic on POSIX — audit #91 / S12),
//     then best-effort fsyncs the dir so the rename itself survives a crash.
//
// The temp file is removed if the rename never happens. WriteFileAtomic does
// NOT create the parent dir — callers MkdirAll first, matching the other state
// writers (Audit.Append, NonceLock.Acquire).
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	if err := ValidateStateFile(path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op once renamed away
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	fsyncDir(dir)
	return nil
}

// fsyncDir flushes a directory entry so a rename/create survives a crash.
// Best-effort: some platforms refuse to open a directory for sync, and at that
// point the rename has already succeeded — a missing dir-fsync only risks losing
// the rename on a power-loss, not corrupting the file.
func fsyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
