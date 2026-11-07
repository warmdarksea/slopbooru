package main

import (
	"bytes"
	"image"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"slopbooru/internal/fixture"
)

func TestPyfloat(t *testing.T) {
	for f, want := range map[float64]string{
		0: "0.0", 1: "1.0", 0.1: "0.1", 0.5: "0.5", 1e-05: "1e-05", 0.0001: "0.0001",
		1e16: "1e+16", 1e15: "1000000000000000.0", 123.456: "123.456", -2.5: "-2.5",
		0.3333333333333333: "0.3333333333333333", 1e20: "1e+20", 1.5e-07: "1.5e-07",
	} {
		if got := pyfloat(f); got != want {
			t.Errorf("pyfloat(%v) = %q, want %q", f, got, want)
		}
	}
}

func TestPyquote(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"a/b c", "/"}:           "a/b%20c",
		{"a/b c", ""}:            "a%2Fb%20c",
		{"naïve~_.-", ""}:        "na%C3%AFve~_.-",
		{`say "hi" & 100%`, "/"}: "say%20%22hi%22%20%26%20100%25",
	} {
		if got := pyquote(in[0], in[1]); got != want {
			t.Errorf("pyquote(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestPyjson(t *testing.T) {
	v, err := jsonDecode(`{"b": [1, 2.50, {"z": null}], "a": "\u00fc\ud83d\ude00\u007f", "e": {}, "n": 1e20, "big": 12345678901234567890}`)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "a": "\u00fc\ud83d\ude00\u007f",
  "b": [
    1,
    2.5,
    {
      "z": null
    }
  ],
  "big": 12345678901234567890,
  "e": {},
  "n": 1e+20
}`
	if got := pyjson(v, 2, true); got != want {
		t.Errorf("pyjson:\n%s\nwant\n%s", got, want)
	}
}

func TestParseQuery(t *testing.T) {
	text, f := parseQuery(`marisa rating:safe subject:"maid girl" foo:bar "a phrase" model:pixai まりさ:x`)
	if text != `marisa foo:bar "a phrase" まりさ:x` {
		t.Errorf("text = %q", text)
	}
	want := map[string][]string{"rating": {"safe"}, "subject": {"maid girl"}, "model": {"pixai"}}
	if !reflect.DeepEqual(f, want) {
		t.Errorf("filters = %v", f)
	}
}

func TestFtsExpr(t *testing.T) {
	for in, want := range map[string]string{
		`maris`:            `"maris"*`,
		`"two words" x`:    `"two words" AND "x"*`,
		`a"b*c`:            `"abc"*`,
		`""`:               ``,
		`"unterminated go`: `"unterminated"* AND "go"*`,
	} {
		if got := ftsExpr(in); got != want {
			t.Errorf("ftsExpr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSqliteURI(t *testing.T) {
	for in, want := range map[string]string{
		"/a b/r?#%.db":    "file:///a%20b/r%3F%23%25.db?mode=ro",
		`C:\Users\x\r.db`: "file:///C:/Users/x/r.db?mode=ro",
	} {
		in := in
		if !strings.HasPrefix(in, "/") && filepath.Separator == '/' {
			// ToSlash is a no-op off windows; feed the slashed form
			in = strings.ReplaceAll(in, `\`, "/")
		}
		if got := sqliteURI(in, "mode=ro"); got != want {
			t.Errorf("sqliteURI(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOrient(t *testing.T) {
	// 2x2: R G / B W. Orientation 6 displays it turned clockwise: B R / W G.
	src := image.NewRGBA(image.Rect(0, 0, 2, 2))
	R, G, B, W := color.RGBA{255, 0, 0, 255}, color.RGBA{0, 255, 0, 255}, color.RGBA{0, 0, 255, 255}, color.RGBA{255, 255, 255, 255}
	src.SetRGBA(0, 0, R)
	src.SetRGBA(1, 0, G)
	src.SetRGBA(0, 1, B)
	src.SetRGBA(1, 1, W)
	at := func(im image.Image, x, y int) color.RGBA { return im.(*image.RGBA).RGBAAt(x, y) }
	for o, want := range map[int][4]color.RGBA{
		1: {R, G, B, W}, 2: {G, R, W, B}, 3: {W, B, G, R}, 4: {B, W, R, G},
		5: {R, B, G, W}, 6: {B, R, W, G}, 7: {W, G, B, R}, 8: {G, W, R, B},
	} {
		im := orient(src, o)
		got := [4]color.RGBA{at(im, 0, 0), at(im, 1, 0), at(im, 0, 1), at(im, 1, 1)}
		if got != want {
			t.Errorf("orientation %d: %v, want %v", o, got, want)
		}
	}
}

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

// The handler in-process, on the fixture: every page renders, files are
// found by content.
func TestServer(t *testing.T) {
	dir := t.TempDir()
	fx, err := fixture.Build(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fx.DB, fx.DBLike} {
		store, err := OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		ix, err := BuildIndex(fx.Corpus, filepath.Join(dir, "files.db"), nil, 2)
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(&Server{store: store, index: ix, thumbs: filepath.Join(dir, "thumbs")})
		get := func(p string) (int, []byte) {
			r, err := http.Get(ts.URL + p)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Body.Close()
			b, _ := io.ReadAll(r.Body)
			return r.StatusCode, b
		}
		if code, body := get("/"); code != 200 || !bytes.Contains(body, []byte("<h1>66 images</h1>")) {
			t.Errorf("%s /: %d %.200s", path, code, body)
		}
		if _, body := get("/?q=marisa"); !bytes.Contains(body, []byte(`matching <em>marisa</em>`)) {
			t.Errorf("%s search", path)
		}
		if code, body := get("/file/1"); code != 200 || !bytes.Equal(body, fx.Files[0].Data) {
			t.Errorf("%s /file/1: %d", path, code)
		}
		if code, _ := get("/image/1"); code != 200 {
			t.Errorf("%s /image/1: %d", path, code)
		}
		if code, _ := get("/image/nope"); code != 400 {
			t.Errorf("%s /image/nope: %d", path, code)
		}
		ts.Close()
		store.db.Close()
	}
}
