package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadSinceFiltersAndTolerates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	content := `{"ts":1000,"action":"order","oid":1}
{"ts":5000,"action":"order","oid":2}
not valid json at all
{"ts":9000,"action":"cancel","oid":2}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// since=4000 keeps ts 5000 and 9000; drops ts 1000 and the corrupt line.
	rows, err := ReadSince(path, 4000)
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows since=4000, got %d: %v", len(rows), rows)
	}

	// since<=0 returns all parseable rows (the corrupt line is still skipped).
	all, err := ReadSince(path, 0)
	if err != nil {
		t.Fatalf("ReadSince all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 rows since=0, got %d", len(all))
	}

	// A missing file is not an error — a fresh box simply has no trail.
	none, err := ReadSince(filepath.Join(dir, "absent.jsonl"), 0)
	if err != nil || none != nil {
		t.Fatalf("missing file: want (nil,nil), got (%v,%v)", none, err)
	}
}
