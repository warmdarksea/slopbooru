package store

import (
	"path/filepath"
	"testing"

	"imgrec/internal/model"
)

// A newer answer to the same question replaces the older one's rows, even
// from a rebuilt engine (another provenance); another question's stay.
func TestWriteReplacesTheSameQuestion(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	im := Image{SHA256: "ab", Path: "/x.png", Bytes: 1}
	guard := func(engine, verdict string, params any) {
		t.Helper()
		c := model.Contribution{Success: true, Fields: map[string]any{"verdict": verdict}}
		if _, err := st.Write(im, Provenance{Model: "llamaguard", Mode: "guard", Engine: engine, Params: params}, "guard", c); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		var n int
		st.DB.QueryRow("SELECT COUNT(*) FROM annotations WHERE field = 'verdict'").Scan(&n)
		return n
	}
	q1 := map[string]any{"v": 1}
	guard("vllm build 1", "unsafe", q1)
	guard("vllm build 2", "safe", q1) // re-asked, another engine build
	if n := count(); n != 1 {
		t.Errorf("%d verdict rows after a newer answer to the same question, want 1", n)
	}
	var v string
	st.DB.QueryRow("SELECT value FROM annotations WHERE field = 'verdict'").Scan(&v)
	if v != "safe" {
		t.Errorf("verdict %q, want the newer answer", v)
	}
	guard("vllm build 2", "unsafe", map[string]any{"v": 2}) // a different question: beside it
	if n := count(); n != 2 {
		t.Errorf("%d verdict rows with two questions answered, want 2", n)
	}
}
