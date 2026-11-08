package records

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func rec(key string) Record {
	return Record{Key: key, Model: "qwen", SpecHash: "s", ImageSHA: "i", InputSHA: "i",
		Raw: `{"choices": []}` + key, Elapsed: 1.5, Engine: "vllm", At: "2026-11-08T00:00:00Z"}
}

func exercise(t *testing.T, b Backend) {
	t.Helper()
	if r, err := b.Get("a"); r != nil || err != nil {
		t.Fatalf("empty backend: %v %v", r, err)
	}
	if err := b.Put(rec("a")); err != nil {
		t.Fatal(err)
	}
	changed := rec("a")
	changed.Raw = "different"
	if err := b.Put(changed); err != nil { // an existing key is kept, not overwritten
		t.Fatal(err)
	}
	b.Put(rec("b"))
	r, err := b.Get("a")
	if err != nil || r == nil || r.Raw != rec("a").Raw || r.Elapsed != 1.5 {
		t.Fatalf("get a: %+v %v", r, err)
	}
	n := 0
	b.Scan(func(Record) error { n++; return nil })
	if n != 2 {
		t.Errorf("scan: %d records, want 2", n)
	}
}

func TestSqlite(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b, err := OpenSqlite(db)
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, b)
}

func TestJsonl(t *testing.T) {
	dir := t.TempDir()
	b, err := OpenJsonl(dir, "library")
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, b)
	b.Close()

	// another machine's file, and a torn last line from a crash
	other, _ := OpenJsonl(dir, "laptop")
	other.Put(rec("c"))
	other.Close()
	f, _ := os.OpenFile(filepath.Join(dir, "laptop.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"key": "d", "raw": "torn`)
	f.Close()

	merged, err := OpenJsonl(dir, "library")
	if err != nil {
		t.Fatal(err)
	}
	defer merged.Close()
	for _, k := range []string{"a", "b", "c"} {
		if r, _ := merged.Get(k); r == nil {
			t.Errorf("%s missing after reopening the merged directory", k)
		}
	}
	if r, _ := merged.Get("d"); r != nil {
		t.Error("a torn line must not count as a record")
	}
}
