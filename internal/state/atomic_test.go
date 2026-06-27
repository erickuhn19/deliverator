package state

// Coverage for the crash-safe state writer that backs the #91 hardening: atomic
// rename, fsync, 0600 enforcement on a pre-existing file, and symlink refusal.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomicRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := WriteFileAtomic(p, []byte(`{"k":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"k":1}` {
		t.Fatalf("content = %q, want the written bytes", b)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	// No stray temp files left behind in the dir.
	ents, _ := os.ReadDir(filepath.Dir(p))
	if len(ents) != 1 {
		t.Fatalf("expected only the target file, got %d entries", len(ents))
	}
}

// A pre-existing world/group-readable file must come back 0600 — mode is honored
// only on create, so the fresh temp+rename is what enforces it (T3-file-mode).
func TestWriteFileAtomicEnforcesModeOnPreExisting(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(p, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("pre-existing 0644 file kept its mode: %v", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(p); string(b) != "new" {
		t.Fatalf("content not replaced: %q", b)
	}
}

// A symlink planted at the target path must be refused, not followed — else a
// local attacker could redirect the write (T3-symlink). The symlink target must
// be left untouched.
func TestWriteFileAtomicRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside.txt")
	if err := os.WriteFile(outside, []byte("victim"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "state.json")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	if err := WriteFileAtomic(link, []byte("evil"), 0o600); err == nil {
		t.Fatal("WriteFileAtomic must refuse a symlinked path")
	}
	if b, _ := os.ReadFile(outside); string(b) != "victim" {
		t.Fatalf("symlink target was overwritten through the link: %q", b)
	}
}

func TestValidateStateFile(t *testing.T) {
	dir := t.TempDir()
	// Missing path: fine (the create case).
	if err := ValidateStateFile(filepath.Join(dir, "absent")); err != nil {
		t.Errorf("missing path should be allowed, got %v", err)
	}
	// Regular file: fine.
	reg := filepath.Join(dir, "reg")
	_ = os.WriteFile(reg, nil, 0o600)
	if err := ValidateStateFile(reg); err != nil {
		t.Errorf("regular file should be allowed, got %v", err)
	}
	// Symlink: refused.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(reg, link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	if err := ValidateStateFile(link); err == nil {
		t.Error("a symlink must be refused")
	}
}

// A bad parent dir surfaces as an error and leaves no target file.
func TestWriteFileAtomicErrorsOnBadDir(t *testing.T) {
	reg := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(reg, []byte("x"), 0o600)
	// reg is a regular file, so reg/sub.json has a non-dir parent.
	if err := WriteFileAtomic(filepath.Join(reg, "sub.json"), []byte("y"), 0o600); err == nil {
		t.Fatal("writing under a regular-file parent should error")
	}
}

// The lock/nonce open paths must refuse a symlinked path (ValidateStateFile +
// O_NOFOLLOW), so a planted symlink can't redirect the flock to an attacker file
// and break the nonce-monotonicity / rate-cap-RMW guarantees (audit #91).
func TestLockRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	_ = os.WriteFile(target, nil, 0o600)
	link := filepath.Join(dir, "rate.lock")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	if l, err := Lock(link); err == nil {
		l.Unlock()
		t.Fatal("Lock must refuse a symlinked path")
	}
}

func TestNonceAcquireRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	_ = os.WriteFile(target, nil, 0o600)
	link := filepath.Join(dir, "nonce.lock")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	if h, err := NewNonceLock(link).Acquire(); err == nil {
		_ = h.Release()
		t.Fatal("NonceLock.Acquire must refuse a symlinked path")
	}
}
