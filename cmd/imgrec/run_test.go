package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var stub string // the imgrec-stub binary, built once

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "imgrec-test-")
	if err != nil {
		panic(err)
	}
	stub = filepath.Join(dir, "imgrec-stub")
	if out, err := exec.Command("go", "build", "-o", stub, "../imgrec-stub").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("building the stub: %v\n%s", err, out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func pictures(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	write := func(name string, w, h int, seed int) {
		im := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y += 1 {
			for x := 0; x < w; x += 1 {
				im.Set(x, y, color.NRGBA{uint8(seed * 41), uint8(x), uint8(y), 255})
			}
		}
		var b bytes.Buffer
		png.Encode(&b, im)
		if err := os.WriteFile(filepath.Join(dir, name), b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 12 {
		write(fmt.Sprintf("p%02d.png", i), 20+i, 16+i, i)
	}
	write("large.png", 2600, 1100, 99) // 2.86 MP: over the 2.5 MP limit
	data, _ := os.ReadFile(filepath.Join(dir, "p00.png"))
	os.WriteFile(filepath.Join(dir, "same as p00.png"), data, 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a picture"), 0o644)
}

func manifestFile(t *testing.T, dir string) string {
	t.Helper()
	models := map[string]any{}
	for _, n := range []string{"qwen", "gemma", "joycaption", "toriigate", "deepseek-ocr", "llamaguard", "pixai"} {
		models[n] = map[string]any{"path": filepath.Join(dir, "weights", n), "closure": ""}
	}
	data, _ := json.Marshal(map[string]any{"models": models})
	path := filepath.Join(dir, "models.json")
	os.WriteFile(path, data, 0o644)
	return path
}

func run(t *testing.T, dir string, extra ...string) int {
	t.Helper()
	log := filepath.Join(dir, "stub.log")
	os.Remove(log)
	t.Setenv("IMGREC_STUB_LOG", log)
	args := append([]string{"--db", filepath.Join(dir, "results.db"), "--specs", "../../specs",
		"--manifest", manifestFile(t, dir), "--vllm", stub, "--pixai", stub + " pixai-worker",
		"--logs", filepath.Join(dir, "logs")}, extra...)
	if err := runCmd(append(args, filepath.Join(dir, "pics"))); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(log)
	starts := map[string]int{}
	for _, l := range strings.Split(string(data), "\n") {
		if f := strings.Fields(l); len(f) > 1 && f[0] == "serve" {
			starts[f[1]]++
			if starts[f[1]] > 1 {
				t.Errorf("%s started twice in one run", f[1])
			}
		}
	}
	n := 0
	for _, c := range starts {
		n += c
	}
	return n
}

func query(t *testing.T, db, q string) [][]string {
	t.Helper()
	d, err := sql.Open("sqlite", "file:"+db+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err := d.Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out [][]string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		rows.Scan(ptrs...)
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = v.String
		}
		out = append(out, row)
	}
	return out
}

func TestRun(t *testing.T) {
	for _, backend := range []string{"sqlite", "jsonl"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			pictures(t, filepath.Join(dir, "pics"))
			var extra []string
			if backend == "jsonl" {
				extra = []string{"--records", filepath.Join(dir, "records"), "--writer", "library"}
			}
			if n := run(t, dir, extra...); n != 6 {
				t.Errorf("first run started %d vllm runner(s), want 6 (one per chat model)", n)
			}
			db := filepath.Join(dir, "results.db")

			// 13 distinct pictures (the copy is the same bytes; the .txt is not a picture)
			if got := query(t, db, "SELECT COUNT(*) FROM images")[0][0]; got != "13" {
				t.Errorf("%s images in the store, want 13", got)
			}
			// every job either answered or out of attempts: nothing silently lost
			left := query(t, db, "SELECT COUNT(*) FROM jobs WHERE attempts < 3")[0][0]
			if left != "0" {
				t.Errorf("%s job(s) neither done nor failed", left)
			}
			// one stand-in, for the large picture, shared by every chat model
			standins, _ := filepath.Glob(filepath.Join(dir, "results.db.images", "standins", "*"))
			if len(standins) != 1 {
				t.Errorf("stand-ins: %v, want exactly one", standins)
			}
			// the guard read gemma's descriptions
			ctx := query(t, db, "SELECT COUNT(*) FROM annotations a JOIN provenances p ON p.id = a.provenance_id "+
				"WHERE p.model = 'llamaguard' AND a.field = 'verdict' AND a.detail LIKE '%Description: %'")[0][0]
			if ctx == "0" {
				t.Error("no guard verdict carries a description as its context")
			}
			if backend == "jsonl" {
				lines, _ := os.ReadFile(filepath.Join(dir, "records", "library.jsonl"))
				if strings.Count(string(lines), "\n") < 13*5 {
					t.Errorf("jsonl backend: %d records", strings.Count(string(lines), "\n"))
				}
			}

			// the second run is all cache: not one runner starts
			if n := run(t, dir, extra...); n != 0 {
				t.Errorf("second run started %d runner(s), want 0", n)
			}
		})
	}
}
