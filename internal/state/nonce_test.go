package state

import (
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

// TestNonceMonotonic models the §16 concurrency requirement: many overlapping
// "processes" (goroutines here, each opening its own fd → its own flock) take
// the lock, allocate persisted+1, and commit. The flock must serialize them so
// the allocated nonces are strictly increasing with no collisions.
func TestNonceMonotonic(t *testing.T) {
	nl := NewNonceLock(filepath.Join(t.TempDir(), "nonce.lock"))
	const n = 64
	got := make([]int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			h, err := nl.Acquire()
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			alloc := h.Persisted() + 1
			got[idx] = alloc
			if err := h.Commit(alloc); err != nil {
				t.Errorf("commit: %v", err)
			}
		}(i)
	}
	wg.Wait()

	sort.Slice(got, func(a, b int) bool { return got[a] < got[b] })
	for i := 0; i < n; i++ {
		if got[i] != int64(i+1) {
			t.Fatalf("nonce collision/gap: sorted[%d]=%d, want %d (full: %v)", i, got[i], i+1, got)
		}
	}
	h, _ := nl.Acquire()
	defer h.Release()
	if h.Persisted() != int64(n) {
		t.Errorf("final high-water = %d, want %d", h.Persisted(), n)
	}
}
