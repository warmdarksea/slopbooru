// pyparity: the Go store must say exactly what the python store says.
//
//	pyparity -python PY -classify classify.py -schema schema.sql -schema-fts schema_fts.sql \
//	         -stub imgrec-stub -imgrec imgrec [-work DIR]
//
// Runs the real python pipeline (src/py-pipeline/classify.py) over a
// generated corpus, with imgrec-stub standing in for vllm and the tagger,
// phase by phase like the Makefile (guard last: it reads the others'
// descriptions). That leaves a db with the python store's rows *and* its
// answer cache. Then `imgrec import-py-cache` rebuilds a store from that
// cache with the Go parsers, and every annotation row must match: image,
// model, field, key, value, confidence, detail.
//
// Floats compare to 1e-12 relative: python's exp() is libm's and Go's is
// its own, so a probability can differ in the last bit.
package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

var (
	python    = flag.String("python", "python3", "python with Pillow")
	classify  = flag.String("classify", "", "src/py-pipeline/classify.py")
	schemaSQL = flag.String("schema", "", "schema.sql")
	schemaFTS = flag.String("schema-fts", "", "schema_fts.sql")
	stub      = flag.String("stub", "", "the imgrec-stub binary")
	imgrecBin = flag.String("imgrec", "", "the imgrec binary")
	work      = flag.String("work", "", "scratch directory (default: a new temp dir)")
	nImages   = flag.Int("images", 40, "corpus size")
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "pyparity:", err)
		os.Exit(2)
	}
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func corpus(dir string, n int) {
	must(os.MkdirAll(dir, 0o755))
	for i := range n {
		w, h := 24+i%17, 18+i%11
		im := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := range h {
			for x := range w {
				im.Set(x, y, color.NRGBA{uint8(i * 37), uint8(x * 9), uint8(y * 13), 255})
			}
		}
		var b bytes.Buffer
		name := fmt.Sprintf("img%03d.png", i)
		if i%3 == 0 {
			name = fmt.Sprintf("img%03d.jpg", i)
			must(jpeg.Encode(&b, im, &jpeg.Options{Quality: 90}))
		} else {
			must(png.Encode(&b, im))
		}
		must(os.WriteFile(filepath.Join(dir, name), b.Bytes(), 0o644))
	}
	// the same bytes under a second name: one image, asked once
	data, err := os.ReadFile(filepath.Join(dir, "img001.png"))
	must(err)
	must(os.WriteFile(filepath.Join(dir, "copy of img001.png"), data, 0o644))
}

func manifest(dir string) string {
	models := map[string]any{}
	for name, spec := range map[string][2]any{
		"pixai": {"tagger", 0.0}, "deepseek-ocr": {"ocr", 0.32}, "joycaption": {"caption", 0.6},
		"qwen": {"fields", 0.95}, "gemma": {"fields", 0.95}, "toriigate": {"caption", 0.9},
		"llamaguard": {"guard", 0.9},
	} {
		weights := filepath.Join(dir, "weights", name)
		must(os.MkdirAll(weights, 0o755))
		closure := filepath.Join(dir, "weights", name+".store-paths")
		must(os.WriteFile(closure, []byte(weights+"\n"), 0o644))
		models[name] = map[string]any{"path": weights, "mode": spec[0], "gpu": spec[1], "closure": closure}
	}
	data, _ := json.Marshal(map[string]any{"models": models})
	path := filepath.Join(dir, "models.json")
	must(os.WriteFile(path, data, 0o644))
	return path
}

func run(name string, args []string, env ...string) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "pyparity: %s %s: %v\n%s\n", name, strings.Join(args, " "), err, out.String())
		os.Exit(1)
	}
}

type row struct {
	Image, Model, Field, Key, Value string
	Conf                            *float64
	Detail                          any
}

func rows(path string) []row {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	must(err)
	defer db.Close()
	q, err := db.Query("SELECT i.sha256, p.model, a.field, a.key, a.value, a.confidence, a.detail " +
		"FROM annotations a JOIN images i ON i.id = a.image_id JOIN provenances p ON p.id = a.provenance_id")
	must(err)
	defer q.Close()
	var out []row
	for q.Next() {
		var r row
		var conf sql.NullFloat64
		var det sql.NullString
		must(q.Scan(&r.Image, &r.Model, &r.Field, &r.Key, &r.Value, &conf, &det))
		if conf.Valid {
			r.Conf = &conf.Float64
		}
		if det.Valid {
			var v any
			d := json.NewDecoder(strings.NewReader(det.String))
			d.UseNumber()
			must(d.Decode(&v))
			r.Detail = v
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id() < out[j].id() })
	return out
}

func (r row) id() string {
	return strings.Join([]string{r.Image, r.Model, r.Field, r.Key, r.Value}, "\x00")
}

func close(a, b float64) bool {
	return a == b || math.Abs(a-b) <= 1e-12*math.Max(math.Abs(a), math.Abs(b))
}

// same compares json values, numbers to 1e-12 relative.
func same(a, b any) bool {
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		fa, e1 := x.Float64()
		fb, e2 := y.Float64()
		return e1 == nil && e2 == nil && close(fa, fb)
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			if !same(v, y[k]) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !same(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	return a == b
}

func show(r row) string {
	conf := "NULL"
	if r.Conf != nil {
		conf = fmt.Sprint(*r.Conf)
	}
	d, _ := json.Marshal(r.Detail)
	return fmt.Sprintf("%s %s %s[%s]=%q conf=%s detail=%s", r.Image[:8], r.Model, r.Field, r.Key, r.Value, conf, d)
}

func main() {
	flag.Parse()
	if *classify == "" || *stub == "" || *imgrecBin == "" {
		flag.Usage()
		os.Exit(2)
	}
	dir := *work
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "pyparity-")
		must(err)
		defer os.RemoveAll(dir)
	}
	images := filepath.Join(dir, "images")
	corpus(images, *nImages)
	mf := manifest(dir)
	pyDB := filepath.Join(dir, "python.db")

	// the python pipeline, phase by phase, guard last
	for _, phase := range []string{"pixai", "deepseek-ocr,joycaption", "qwen", "gemma", "toriigate", "llamaguard"} {
		run(*python, []string{*classify, "--dir", images, "--db", pyDB, "--models", phase,
			"--manifest", mf, "--vllm", *stub, "--tagger-python", *stub, "--tagger-script", *classify,
			"--schema", *schemaSQL, "--schema-fts", *schemaFTS, "--port-base", fmt.Sprint(freePort()),
			"--no-trace", "--start-timeout", "30", "--timeout", "30"})
	}
	goDB := filepath.Join(dir, "go.db")
	run(*imgrecBin, []string{"import-py-cache", "--py-db", pyDB, "--images", images,
		"--manifest", mf, "--db", goDB})

	py, gd := rows(pyDB), rows(goDB)
	pyBy, goBy := map[string]row{}, map[string]row{}
	for _, r := range py {
		pyBy[r.id()] = r
	}
	for _, r := range gd {
		goBy[r.id()] = r
	}
	var diffs []string
	for id, p := range pyBy {
		g, ok := goBy[id]
		switch {
		case !ok:
			diffs = append(diffs, "only in python: "+show(p))
		case (p.Conf == nil) != (g.Conf == nil) || (p.Conf != nil && !close(*p.Conf, *g.Conf)):
			diffs = append(diffs, "confidence differs:\n    python "+show(p)+"\n    go     "+show(g))
		case !same(p.Detail, g.Detail):
			diffs = append(diffs, "detail differs:\n    python "+show(p)+"\n    go     "+show(g))
		}
	}
	for id, g := range goBy {
		if _, ok := pyBy[id]; !ok {
			diffs = append(diffs, "only in go: "+show(g))
		}
	}
	models := map[string]int{}
	for _, r := range py {
		models[r.Model]++
	}
	fmt.Printf("pyparity: python %d rows, go %d rows %v\n", len(py), len(gd), models)
	if len(diffs) > 0 {
		sort.Strings(diffs)
		for i, d := range diffs {
			if i == 20 {
				fmt.Printf("... and %d more\n", len(diffs)-20)
				break
			}
			fmt.Println(d)
		}
		os.Exit(1)
	}
	fmt.Println("pyparity: ok, every row identical")
}
