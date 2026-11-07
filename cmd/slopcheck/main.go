// slopcheck: an end-to-end test of a slopbooru binary, from the outside.
//
//	slopcheck -bin ./slopbooru
//	slopcheck -bin ./slopbooru.exe -wrap wine -winpaths
//	slopcheck -bin ./slopbooru -ref 'python3 booru.py'
//
// It builds the fixture, lays the pictures out somewhere else under other
// names (so they can only be found by content), starts the binary, and
// checks every file, every thumbnail, the routes, the hash cache across
// restarts. With -ref it also starts a reference implementation (the
// Python booru this was ported from) on the same dbs and requires every
// page to be byte-identical but for the brand name.
package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"image"
	_ "image/jpeg"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"slopbooru/internal/fixture"
)

var (
	bin      = flag.String("bin", "", "the booru binary under test")
	wrap     = flag.String("wrap", "", "run it through this, e.g. wine")
	winpaths = flag.Bool("winpaths", false, "pass paths as Z:\\... (the binary is a windows one under wine)")
	ref      = flag.String("ref", "", "reference implementation to diff pages against, e.g. 'python3 booru.py'")
	work     = flag.String("work", "", "scratch directory (default: a new temp dir)")
	timeout  = flag.Duration("timeout", 3*time.Minute, "how long a server may take to come up")
	keep     = flag.Bool("keep", false, "keep the scratch directory")
)

var failures int

func fail(format string, a ...any) {
	failures++
	fmt.Fprintf(os.Stderr, "FAIL: "+format+"\n", a...)
}

func say(format string, a ...any) { fmt.Fprintf(os.Stderr, "slopcheck: "+format+"\n", a...) }

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "slopcheck:", err)
		os.Exit(2)
	}
}

func hostPath(p string) string {
	if !*winpaths {
		return p
	}
	return `Z:` + strings.ReplaceAll(p, "/", `\`)
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type server struct {
	cmd  *exec.Cmd
	base string
	log  *bytes.Buffer
}

func start(argv []string, port int) *server {
	cmd := exec.Command(argv[0], argv[1:]...)
	log := &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = log, log
	must(cmd.Start())
	s := &server{cmd, fmt.Sprintf("http://127.0.0.1:%d", port), log}
	deadline := time.Now().Add(*timeout)
	for time.Now().Before(deadline) {
		if r, err := http.Get(s.base + "/stats"); err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return s
			}
		}
		if cmd.ProcessState != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.stop()
	fmt.Fprintf(os.Stderr, "server did not come up: %v\n%s\n", argv, log)
	os.Exit(1)
	return nil
}

func (s *server) stop() {
	s.cmd.Process.Kill()
	s.cmd.Wait()
}

func booru(db, images, thumbs string, port int) []string {
	argv := strings.Fields(*wrap)
	argv = append(argv, *bin, "--db", hostPath(db), "--host", "127.0.0.1",
		"--port", fmt.Sprint(port), "--thumbs", hostPath(thumbs))
	if images != "" {
		argv = append(argv, "--images", hostPath(images))
	}
	return argv
}

type resp struct {
	code  int
	ctype string
	body  []byte
}

func get(base, path string) resp {
	r, err := http.Get(base + path)
	if err != nil {
		fail("GET %s: %v", path, err)
		return resp{}
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return resp{r.StatusCode, r.Header.Get("content-type"), b}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func copyFile(src, dst string) {
	b, err := os.ReadFile(src)
	must(err)
	must(os.MkdirAll(filepath.Dir(dst), 0o755))
	must(os.WriteFile(dst, b, 0o644))
}

func main() {
	flag.Parse()
	if *bin == "" {
		flag.Usage()
		os.Exit(2)
	}
	dir := *work
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "slopcheck-")
		must(err)
		if !*keep {
			defer os.RemoveAll(dir)
		}
	}
	fx, err := fixture.Build(filepath.Join(dir, "fixture"))
	must(err)
	say("fixture: %d files, db %s", len(fx.Files), fx.DB)

	// the pictures somewhere else, renamed and nested: only content can
	// find them now. The duplicate is dropped (its twin carries it) and so
	// is "gone.png", which never existed.
	pics := filepath.Join(dir, "pictures with spaces")
	for i, f := range fx.Files {
		if f.Name == fx.Dup {
			continue
		}
		sub := []string{"", "a", "a/b", "c d"}[i%4]
		copyFile(filepath.Join(fx.Corpus, f.Name), filepath.Join(pics, sub, fmt.Sprintf("renamed-%03d%s", i, filepath.Ext(f.Name))))
	}
	copyFile(filepath.Join(fx.Corpus, "notes.txt"), filepath.Join(pics, "notes.txt"))

	db, err := sql.Open("sqlite", fx.DB)
	must(err)
	shaOf := map[int64]string{}
	rows, err := db.Query("SELECT id, sha256 FROM images")
	must(err)
	for rows.Next() {
		var id int64
		var sha string
		must(rows.Scan(&id, &sha))
		shaOf[id] = sha
	}
	rows.Close()
	have := map[string][]byte{}
	for _, f := range fx.Files {
		have[f.Sha] = f.Data
	}

	thumbs := filepath.Join(dir, "thumbs")
	port := freePort()

	// ---- run 1: a cold start hashes everything
	s := start(booru(fx.DB, pics, thumbs, port), port)
	if !regexp.MustCompile(`index: \d+ file\(s\), \d+ hashed, 0 from cache`).Match(s.log.Bytes()) {
		fail("cold start should hash everything:\n%s", s.log)
	}
	if !strings.Contains(s.log.String(), fmt.Sprintf("%d/%d of the db's images found", len(fx.Files)-1, len(shaOf))) {
		fail("expected %d/%d images found:\n%s", len(fx.Files)-1, len(shaOf), s.log)
	}
	checkFiles(s.base, shaOf, have)
	checkThumbs(s.base, shaOf, have, fx)
	checkRoutes(s.base)
	s.stop()

	// ---- run 2: nothing changed, nothing hashed
	s = start(booru(fx.DB, pics, thumbs, port), port)
	if !regexp.MustCompile(`index: \d+ file\(s\), 0 hashed, \d+ from cache`).Match(s.log.Bytes()) {
		fail("warm start should hash nothing:\n%s", s.log)
	}
	s.stop()

	// ---- run 3: one file replaced with another picture's bytes, one moved:
	// both re-hashed, and the replaced one now serves its new content.
	victim := filepath.Join(pics, "renamed-000.png") // pic00
	must(os.WriteFile(victim, fx.Files[1].Data, 0o644))
	future := time.Now().Add(time.Hour)
	os.Chtimes(victim, future, future)
	must(os.Rename(filepath.Join(pics, "a", "renamed-001.png"), filepath.Join(pics, "moved.png")))
	s = start(booru(fx.DB, pics, thumbs, port), port)
	if !regexp.MustCompile(`index: \d+ file\(s\), 2 hashed, \d+ from cache`).Match(s.log.Bytes()) {
		fail("after a change exactly two files should be re-hashed:\n%s", s.log)
	}
	for id, sha := range shaOf {
		if sha == fx.Files[0].Sha {
			if r := get(s.base, fmt.Sprintf("/file/%d", id)); r.code != 404 {
				fail("pic00's bytes are gone from disk, /file/%d should 404, got %d", id, r.code)
			}
		}
		if sha == fx.Files[1].Sha {
			if r := get(s.base, fmt.Sprintf("/file/%d", id)); sum(r.body) != sha {
				fail("pic01 should still be served (twice on disk now)")
			}
		}
	}
	s.stop()

	// ---- without --images: the recorded paths, which do not exist here
	s = start(booru(fx.DB, "", thumbs, port), port)
	for id := range shaOf {
		if r := get(s.base, fmt.Sprintf("/file/%d", id)); r.code != 404 {
			fail("no --images: /file/%d should 404 (recorded paths are elsewhere), got %d", id, r.code)
		}
	}
	s.stop()

	if *ref != "" {
		for _, db := range []string{fx.DB, fx.DBLike} {
			compare(db, shaOf, dir, thumbs)
		}
	}

	if failures > 0 {
		say("%d failure(s)", failures)
		os.Exit(1)
	}
	say("ok")
}

func checkFiles(base string, shaOf map[int64]string, have map[string][]byte) {
	for id, sha := range shaOf {
		r := get(base, fmt.Sprintf("/file/%d", id))
		if _, ok := have[sha]; !ok {
			if r.code != 404 {
				fail("/file/%d has no file anywhere, want 404, got %d", id, r.code)
			}
			continue
		}
		if r.code != 200 || sum(r.body) != sha {
			fail("/file/%d: code %d, content %s, want %s", id, r.code, sum(r.body)[:12], sha[:12])
		}
	}
	// a range request, as a browser's video/image loader may send
	for id, sha := range shaOf {
		if data, ok := have[sha]; ok && len(data) > 10 {
			req, _ := http.NewRequest("GET", fmt.Sprintf("%s/file/%d", base, id), nil)
			req.Header.Set("Range", "bytes=2-5")
			r, err := http.DefaultClient.Do(req)
			if err != nil {
				fail("range: %v", err)
				break
			}
			b, _ := io.ReadAll(r.Body)
			r.Body.Close()
			if r.StatusCode != 206 || !bytes.Equal(b, data[2:6]) {
				fail("range request: %d %q", r.StatusCode, b)
			}
			break
		}
	}
}

func checkThumbs(base string, shaOf map[int64]string, have map[string][]byte, fx *fixture.Fixture) {
	for id, sha := range shaOf {
		if _, ok := have[sha]; !ok {
			continue
		}
		for _, size := range []int{256, 64} {
			r := get(base, fmt.Sprintf("/thumb/%d?s=%d", id, size))
			im, format, err := image.Decode(bytes.NewReader(r.body))
			if r.code != 200 || err != nil || format != "jpeg" || r.ctype != "image/jpeg" {
				fail("/thumb/%d?s=%d: code %d type %s format %s err %v", id, size, r.code, r.ctype, format, err)
				continue
			}
			if b := im.Bounds(); b.Dx() != size || b.Dy() != size {
				fail("/thumb/%d?s=%d is %dx%d", id, size, b.Dx(), b.Dy())
			}
		}
	}
	// the sideways shot: stored left red / right blue, orientation 6 says
	// turn it clockwise, so the thumbnail has red on top.
	for id, sha := range shaOf {
		for _, f := range fx.Files {
			if f.Name != fx.Rotated || f.Sha != sha {
				continue
			}
			r := get(base, fmt.Sprintf("/thumb/%d?s=64", id))
			im, _, err := image.Decode(bytes.NewReader(r.body))
			if err != nil {
				fail("rotated thumb: %v", err)
				continue
			}
			top, bottom := im.At(32, 4), im.At(32, 60)
			tr, _, tb, _ := top.RGBA()
			br, _, bb, _ := bottom.RGBA()
			if !(tr > 0xc000 && tb < 0x4000 && bb > 0xc000 && br < 0x4000) {
				fail("EXIF orientation not applied: top %v bottom %v", top, bottom)
			}
		}
	}
}

func checkRoutes(base string) {
	for path, code := range map[string]int{
		"/": 200, "/stats": 200, "/image/1": 200, "/image/1?sort=value": 200,
		"/provenance/1": 200, "/value/tag/a%2Fb": 200, "/api/image/1": 200,
		"/image/99999": 404, "/provenance/99999": 404, "/nope": 404, "/image/x": 400,
		"/?page=x": 400, "/thumb/99999": 404, "/file/99999": 404, "//image//1": 200,
		"/?q=%22unterminated": 200, "/?q=*%22%22*": 200, "/?q=model:pixai%20tag:solo": 200,
	} {
		if r := get(base, path); r.code != code {
			fail("%s: want %d, got %d", path, code, r.code)
		}
	}
	// HEAD carries no body
	r, err := http.Head(base + "/")
	if err != nil || r.StatusCode != 200 {
		fail("HEAD /: %v %v", err, r)
	}
	// search works by content: the fixture's tags
	if body := get(base, "/?q=marisa").body; !bytes.Contains(body, []byte("matching <em>marisa</em>")) ||
		bytes.Contains(body, []byte("<h1>0 images")) {
		fail("search for marisa found nothing")
	}
}

// compare serves db from the binary and from the reference, and diffs
// every page either could be asked for.
func compare(dbPath string, shaOf map[int64]string, dir, thumbs string) {
	db, err := sql.Open("sqlite", dbPath)
	must(err)
	defer db.Close()
	urls := []string{
		"/", "/?page=1", "/?page=2", "/?page=-3", "/stats", "/nope", "/image/x", "/image/99999",
		"/provenance/99999", "/value/tag/nothing-at-all", "/?q=", "/?page=&page=1",
		"/api/image/99999",
	}
	for _, q := range []string{
		"marisa", "maris", "Marisa", "rating:safe", "rating:SAFE", "model:pixai", "model:pixai tag:marisa",
		"model:pixai model:qwen tag:solo", `subject:"maid girl"`, `"trying hard"`, `"two lines"`,
		"field:description picture", "naïve", "café", "kero_(cardcaptor_sakura)", "a/b", `say "hi"`,
		`"unterminated`, `""`, `*`, `"*"`, "100%", "<script>", "foo:bar", "tag:", "hazard:hate",
		"verdict:unsafe model:llamaguard", "rating:safe page", "まりさ", "palette:warm", "x\u00a0y",
	} {
		urls = append(urls, "/?q="+urlQuote(q), "/?q="+urlQuote(q)+"&page=1")
	}
	for id := range shaOf {
		for _, s := range []string{"", "?sort=value", "?sort=conf", "?sort=model", "?sort=bogus"} {
			urls = append(urls, fmt.Sprintf("/image/%d%s", id, s))
		}
		urls = append(urls, fmt.Sprintf("/api/image/%d", id))
	}
	rows, err := db.Query("SELECT id FROM provenances")
	must(err)
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		urls = append(urls, fmt.Sprintf("/provenance/%d", id))
	}
	rows.Close()
	rows, err = db.Query("SELECT DISTINCT field, value FROM annotations")
	must(err)
	for rows.Next() {
		var f, v string
		rows.Scan(&f, &v)
		urls = append(urls, "/value/"+pathQuote(f)+"/"+pathQuote(v), "/value/"+pathQuote(f)+"/"+pathQuote(v)+"?page=1")
	}
	rows.Close()

	pa, pb := freePort(), freePort()
	a := start(booru(dbPath, "", thumbs, pa), pa)
	defer a.stop()
	refArgv := append(strings.Fields(*ref), "--db", dbPath, "--host", "127.0.0.1",
		"--port", fmt.Sprint(pb), "--thumbs", filepath.Join(dir, "ref-thumbs"))
	b := start(refArgv, pb)
	defer b.stop()

	brands := regexp.MustCompile(`slopbooru|llm-booru`)
	diffs := 0
	for _, u := range urls {
		ra, rb := get(a.base, u), get(b.base, u)
		ba, bb := brands.ReplaceAll(ra.body, []byte("BRAND")), brands.ReplaceAll(rb.body, []byte("BRAND"))
		if ra.code != rb.code || ra.ctype != rb.ctype || !bytes.Equal(ba, bb) {
			diffs++
			if diffs <= 5 {
				fail("%s differs from the reference: %d %s vs %d %s\n%s", u, ra.code, ra.ctype, rb.code, rb.ctype, firstDiff(ba, bb))
			}
		}
	}
	if diffs > 5 {
		fail("... %d page(s) differ in all", diffs)
	}
	say("%s: %d page(s) compared with the reference, %d differ", filepath.Base(dbPath), len(urls), diffs)
}

func firstDiff(a, b []byte) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	lo := max(0, i-80)
	clip := func(x []byte) string { return string(x[lo:min(len(x), i+120)]) }
	return fmt.Sprintf("  got: ...%s\n  ref: ...%s", clip(a), clip(b))
}

func urlQuote(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-_.~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func pathQuote(s string) string { return urlQuote(s) }
