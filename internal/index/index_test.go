package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIndexCache(t *testing.T) {
	dir := t.TempDir()
	pics := filepath.Join(dir, "pics")
	os.MkdirAll(filepath.Join(pics, "sub"), 0o755)
	os.WriteFile(filepath.Join(pics, "a.png"), []byte("aaa"), 0o644)
	os.WriteFile(filepath.Join(pics, "sub", "b.png"), []byte("bbb"), 0o644)
	os.WriteFile(filepath.Join(pics, "sub", "a-again.png"), []byte("aaa"), 0o644)
	os.MkdirAll(filepath.Join(pics, "skipme"), 0o755)
	os.WriteFile(filepath.Join(pics, "skipme", "c.png"), []byte("ccc"), 0o644)
	cache := filepath.Join(dir, "cache", "files.db")
	skip := []string{filepath.Join(pics, "skipme")}
	shaA := "9834876dcfb05cb167a5c24953eba58c4ac89b1adf57f28f2f9d09af107ee8f0"

	ix, err := BuildIndex(pics, cache, skip, 2)
	if err != nil {
		t.Fatal(err)
	}
	if ix.Files != 3 || ix.Hashed != 3 || ix.Cached != 0 {
		t.Fatalf("cold: %+v", ix)
	}
	if got := ix.byHash[shaA]; len(got) != 2 || filepath.Base(got[0]) != "a.png" {
		t.Errorf("duplicates: %v", got)
	}
	if ix.Path(strings.Repeat("0", 64)) != "" {
		t.Error("unknown hash found a path")
	}

	ix, _ = BuildIndex(pics, cache, skip, 2)
	if ix.Hashed != 0 || ix.Cached != 3 {
		t.Fatalf("warm: %+v", ix)
	}

	// same name and size, new content and mtime: hashed again
	os.WriteFile(filepath.Join(pics, "a.png"), []byte("zzz"), 0o644)
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(pics, "a.png"), later, later)
	ix, _ = BuildIndex(pics, cache, skip, 2)
	if ix.Hashed != 1 || ix.Cached != 2 || len(ix.byHash[shaA]) != 1 {
		t.Fatalf("changed: %+v %v", ix, ix.byHash[shaA])
	}

	// the stale row was pruned: the cache holds exactly the three files
	db, _ := openHashCache(cache)
	defer db.Close()
	m, _ := loadHashCache(db)
	if len(m) != 3 {
		t.Errorf("cache has %d rows, want 3", len(m))
	}
}
