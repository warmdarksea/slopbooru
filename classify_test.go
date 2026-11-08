package main

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeAPI is an OpenAI-compatible backend with canned answers per model:
// single requests, files and batches (a batch answers on its second poll).
type fakeAPI struct {
	t       *testing.T
	mu      sync.Mutex
	files   map[string][]byte
	batches map[string]*fakeBatch
	asked   map[string]int    // per model
	context map[string]string // what the guard was told, by picture
	sizes   []image.Point     // what pictures it was shown
	n       int
}

type fakeBatch struct {
	polls int
	out   string
}

func newFakeAPI(t *testing.T) (*fakeAPI, *httptest.Server) {
	f := &fakeAPI{t: t, files: map[string][]byte{}, batches: map[string]*fakeBatch{},
		asked: map[string]int{}, context: map[string]string{}}
	return f, httptest.NewServer(f)
}

func (f *fakeAPI) answer(body []byte) (int, []byte) {
	var req struct {
		Model    string `json:"model"`
		Seed     *int64 `json:"seed"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		ResponseFormat any `json:"response_format"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return 400, []byte(`{"error":{"message":"bad json"}}`)
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	json.Unmarshal(req.Messages[len(req.Messages)-1].Content, &parts)
	var pic, text string
	for _, p := range parts {
		if p.Type == "image_url" {
			_, b64, _ := strings.Cut(p.ImageURL.URL, ";base64,")
			data, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				return 400, []byte(`{"error":{"message":"bad image"}}`)
			}
			cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil {
				return 400, []byte(`{"error":{"message":"not a picture"}}`)
			}
			f.mu.Lock()
			f.sizes = append(f.sizes, image.Pt(cfg.Width, cfg.Height))
			f.mu.Unlock()
			pic = fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)
		} else {
			text = p.Text
		}
	}
	f.mu.Lock()
	f.asked[req.Model]++
	f.mu.Unlock()
	var content string
	var logprobs any
	switch req.Model {
	case "tag":
		content = `{"model":"tag","success":true,"fields":{"tags":["one_tag","two_tag"],"rating":"safe"},` +
			`"detail":{"scores":{"tag":{"one_tag":0.9,"two_tag":0.6},"rating":{"safe":0.8}}}}`
	case "cap":
		content = "A caption of a " + pic + " picture."
	case "fld":
		if req.ResponseFormat == nil || req.Seed == nil {
			return 400, []byte(`{"error":{"message":"fld wants a schema and a seed"}}`)
		}
		content = fmt.Sprintf(`{"description":"a %s picture, described","tags":["fld_tag"],"rating":"safe","confidence":{"description":0.75}}`, pic)
	case "grd":
		f.mu.Lock()
		f.context[pic] = text
		f.mu.Unlock()
		content = "safe"
		logprobs = map[string]any{"content": []any{map[string]any{"token": "safe", "logprob": -0.01,
			"top_logprobs": []any{map[string]any{"token": "safe", "logprob": -0.01}, map[string]any{"token": "unsafe", "logprob": -4.6}}}}}
	case "bad":
		return 500, []byte(`{"error":{"message":"broken"}}`)
	default:
		return 404, []byte(`{"error":{"message":"no such model"}}`)
	}
	out, _ := json.Marshal(map[string]any{"object": "chat.completion", "model": req.Model,
		"system_fingerprint": "fp-" + req.Model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content},
			"logprobs": logprobs, "finish_reason": "stop"}}})
	return 200, out
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer sekrit" {
		http.Error(w, `{"error":{"message":"no token"}}`, 401)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1")
	switch {
	case r.Method == "POST" && path == "/chat/completions":
		body, _ := io.ReadAll(r.Body)
		status, out := f.answer(body)
		w.WriteHeader(status)
		w.Write(out)
	case r.Method == "POST" && path == "/files":
		file, _, err := r.FormFile("file")
		if err != nil || r.FormValue("purpose") != "batch" {
			http.Error(w, "bad upload", 400)
			return
		}
		data, _ := io.ReadAll(file)
		f.mu.Lock()
		f.n++
		id := fmt.Sprintf("file-%d", f.n)
		f.files[id] = data
		f.mu.Unlock()
		fmt.Fprintf(w, `{"id":%q,"object":"file"}`, id)
	case r.Method == "GET" && strings.HasPrefix(path, "/files/") && strings.HasSuffix(path, "/content"):
		f.mu.Lock()
		data := f.files[strings.TrimSuffix(strings.TrimPrefix(path, "/files/"), "/content")]
		f.mu.Unlock()
		w.Write(data)
	case r.Method == "POST" && path == "/batches":
		var req struct {
			InputFileID string `json:"input_file_id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		input := f.files[req.InputFileID]
		f.mu.Unlock()
		var out strings.Builder
		for _, l := range strings.Split(strings.TrimSpace(string(input)), "\n") {
			var line struct {
				CustomID string          `json:"custom_id"`
				Body     json.RawMessage `json:"body"`
			}
			json.Unmarshal([]byte(l), &line)
			status, body := f.answer(line.Body)
			res, _ := json.Marshal(map[string]any{"custom_id": line.CustomID,
				"response": map[string]any{"status_code": status, "body": json.RawMessage(body)}})
			out.Write(append(res, '\n'))
		}
		f.mu.Lock()
		f.n++
		id := fmt.Sprintf("batch_%d", f.n)
		f.files["out-"+id] = []byte(out.String())
		f.batches[id] = &fakeBatch{}
		f.mu.Unlock()
		fmt.Fprintf(w, `{"id":%q,"status":"in_progress"}`, id)
	case r.Method == "GET" && strings.HasPrefix(path, "/batches/"):
		id := strings.TrimPrefix(path, "/batches/")
		f.mu.Lock()
		b := f.batches[id]
		if b != nil {
			b.polls++
		}
		f.mu.Unlock()
		if b == nil {
			http.Error(w, "no batch", 404)
			return
		}
		if b.polls < 2 {
			fmt.Fprintf(w, `{"id":%q,"status":"in_progress","request_counts":{"total":1,"completed":0,"failed":0}}`, id)
			return
		}
		fmt.Fprintf(w, `{"id":%q,"status":"completed","output_file_id":"out-%s","request_counts":{"total":1,"completed":1,"failed":0}}`, id, id)
	default:
		http.Error(w, "no route "+r.Method+" "+path, 404)
	}
}

func (f *fakeAPI) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, v := range f.asked {
		n += v
	}
	return n
}

func writePNG(t *testing.T, path string, w, h int, c color.RGBA) {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{c.R, c.G, uint8(x), 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	png.Encode(f, img)
	f.Close()
}

const testModels = `
[models.tag]
backend = "fake"
kind = "tagger"

[models.cap]
backend = "fake"
kind = "caption"
max_pixels = 40000
prompt = "Describe it."

[models.fld]
backend = "fake"
kind = "fields"
max_pixels = 40000
seed = true
prompt = "Fill it in."
schema = '{"type": "object", "properties": {"description": {"type": "string"}}}'
params = { temperature = 0.2 }

[models.grd]
backend = "fake"
kind = "guard"
prompt = "{context}"
params = { logprobs = true, top_logprobs = 5 }
needs = { model = "fld", field = "description" }
`

func classifyEnv(t *testing.T, mode, extra string) (dir, cfg string) {
	dir = t.TempDir()
	pics := filepath.Join(dir, "pics")
	os.MkdirAll(filepath.Join(pics, "sub"), 0o755)
	writePNG(t, filepath.Join(pics, "a.png"), 100, 80, color.RGBA{200, 10, 0, 255})
	writePNG(t, filepath.Join(pics, "sub", "big.png"), 400, 300, color.RGBA{10, 200, 0, 255}) // over max_pixels
	os.WriteFile(filepath.Join(pics, "readme.txt"), []byte("not a picture"), 0o644)
	cfg = filepath.Join(dir, "classify.toml")
	return dir, cfg
}

func writeCfg(t *testing.T, path, url, mode, models string) {
	t.Helper()
	s := fmt.Sprintf("[backends.fake]\nurl = %q\ntoken = \"sekrit\"\nmode = %q\npoll = \"10ms\"\n%s", url+"/v1", mode, models)
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rowsOf(t *testing.T, dbPath string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT p.model, p.model_path, a.field, a.key, a.value, IFNULL(a.confidence, -1)
		FROM annotations a JOIN provenances p ON p.id = a.provenance_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m, mp, f, k, v string
		var c float64
		rows.Scan(&m, &mp, &f, &k, &v, &c)
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%s|%.4f", m, mp, f, k, v, c))
	}
	sort.Strings(out)
	return out
}

func run(t *testing.T, args ...string) {
	t.Helper()
	if err := classifyRun(args); err != nil {
		t.Fatalf("classify %v: %v", args, err)
	}
}

func TestClassify(t *testing.T) {
	results := map[string][]string{}
	for _, mode := range []string{"batch", "requests"} {
		t.Run(mode, func(t *testing.T) {
			fake, srv := newFakeAPI(t)
			defer srv.Close()
			dir, cfg := classifyEnv(t, mode, "")
			writeCfg(t, cfg, srv.URL, mode, testModels)
			db := filepath.Join(dir, "results.db")
			args := []string{"--images", filepath.Join(dir, "pics"), "--db", db, "--config", cfg, "--models", "tag,cap,fld,grd"}
			run(t, args...)

			if fake.asked["tag"] != 2 || fake.asked["cap"] != 2 || fake.asked["fld"] != 2 || fake.asked["grd"] != 2 {
				t.Errorf("asked %v, want each model twice (the readme isn't a picture)", fake.asked)
			}
			for _, p := range fake.sizes {
				if p.X*p.Y > 40000 && p != image.Pt(400, 300) {
					t.Errorf("shown a %v picture: over max_pixels and not the original", p)
				}
			}
			// the tagger has no max_pixels: it saw the big one whole
			sawBig := false
			for _, p := range fake.sizes {
				sawBig = sawBig || p == image.Pt(400, 300)
			}
			if !sawBig {
				t.Errorf("nobody saw the original big picture: %v", fake.sizes)
			}
			// the guard read fld's description of the same (downscaled) picture
			for pic, ctx := range fake.context {
				if !strings.HasPrefix(ctx, "Description: a ") || !strings.HasSuffix(ctx, " picture, described") {
					t.Errorf("guard's context for %s: %q", pic, ctx)
				}
			}
			rows := rowsOf(t, db)
			want := []string{
				"cap|fp-cap|description||A caption of a 100x80 picture.|-1.0000",
				"fld|fp-fld|description||a 100x80 picture, described|0.7500",
				"grd|fp-grd|verdict||safe|0.9900",
				"tag|fp-tag|tag||one_tag|0.9000",
			}
			joined := strings.Join(rows, "\n")
			for _, w := range want {
				if !strings.Contains(joined, w) {
					t.Errorf("no row %q in\n%s", w, joined)
				}
			}
			results[mode] = rows

			// a second run asks nothing
			before := fake.total()
			run(t, args...)
			if n := fake.total() - before; n != 0 {
				t.Errorf("a second run asked %d time(s)", n)
			}
			// reparse rebuilds the same rows from the kept answers
			d, _ := sql.Open("sqlite", db)
			d.Exec("DELETE FROM annotations")
			d.Close()
			run(t, "--images", filepath.Join(dir, "pics"), "--db", db, "--config", cfg, "--reparse")
			if got := rowsOf(t, db); strings.Join(got, "\n") != joined {
				t.Errorf("reparse:\n%s\nwant\n%s", strings.Join(got, "\n"), joined)
			}

			// a changed prompt is a new question: asked again, beside the old
			writeCfg(t, cfg, srv.URL, mode, strings.Replace(testModels, "Describe it.", "Describe it again.", 1))
			before = fake.total()
			run(t, args...)
			if n := fake.total() - before; n != 2 {
				t.Errorf("a changed prompt asked %d time(s), want 2", n)
			}
			var provs int
			d, _ = sql.Open("sqlite", db)
			d.QueryRow("SELECT COUNT(*) FROM provenances WHERE model = 'cap'").Scan(&provs)
			d.Close()
			if provs != 2 {
				t.Errorf("cap has %d provenance(s), want the old and the new", provs)
			}
		})
	}
	if strings.Join(results["batch"], "\n") != strings.Join(results["requests"], "\n") {
		t.Errorf("batch and requests modes wrote different rows:\n%s\n---\n%s",
			strings.Join(results["batch"], "\n"), strings.Join(results["requests"], "\n"))
	}
}

// A model that fails is recorded as failed, keeps no answer, and is asked
// again next time.
func TestClassifyFailure(t *testing.T) {
	fake, srv := newFakeAPI(t)
	defer srv.Close()
	dir, cfg := classifyEnv(t, "batch", "")
	writeCfg(t, cfg, srv.URL, "batch", "[models.bad]\nbackend = \"fake\"\nkind = \"caption\"\nprompt = \"x\"\n")
	db := filepath.Join(dir, "results.db")
	args := []string{"--images", filepath.Join(dir, "pics"), "--db", db, "--config", cfg, "--models", "bad"}
	run(t, args...)
	run(t, args...)
	if fake.asked["bad"] != 4 {
		t.Errorf("asked bad %d times over two runs, want 4", fake.asked["bad"])
	}
	d, _ := sql.Open("sqlite", db)
	defer d.Close()
	var failures, answers int
	d.QueryRow("SELECT COUNT(*) FROM failures").Scan(&failures)
	d.QueryRow("SELECT COUNT(*) FROM classify_answers").Scan(&answers)
	if failures != 2 || answers != 0 {
		t.Errorf("failures %d (want 2), kept answers %d (want 0)", failures, answers)
	}
}

func TestClassifyConfig(t *testing.T) {
	c, err := readClassify("")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"pixai", "qwen", "gemma", "toriigate", "joycaption", "deepseek-ocr", "llamaguard"} {
		if c.Models[m] == nil {
			t.Errorf("the built-in config has no %s", m)
		}
	}
	if c.Models["llamaguard"].round != 1 || c.Models["gemma"].round != 0 {
		t.Error("llamaguard isn't asked after gemma")
	}
	// gemma needs its schema's keys sorted (see classify.toml)
	if !strings.Contains(c.Models["gemma"].Schema, "\"confidence\"") ||
		strings.Index(c.Models["gemma"].Schema, `"confidence"`) > strings.Index(c.Models["gemma"].Schema, `"description"`) {
		t.Error("gemma's schema doesn't put confidence before description")
	}
	dir := t.TempDir()
	for want, s := range map[string]string{
		"unknown setting":   "[models.x]\nbackend = \"timeshare\"\nkind = \"caption\"\nprompto = \"x\"\n",
		"kind":              "[models.x]\nbackend = \"timeshare\"\nkind = \"poetry\"\n",
		"no backend":        "[models.x]\nbackend = \"nowhere\"\nkind = \"caption\"\n",
		"needs":             "[models.x]\nbackend = \"timeshare\"\nkind = \"guard\"\nneeds = { model = \"nobody\", field = \"description\" }\n",
		"isn't configured":  "[models.gemma]\nenabled = false\n",
		"round in a circle": "[models.x]\nbackend = \"timeshare\"\nkind = \"guard\"\nneeds = { model = \"y\", field = \"d\" }\n[models.y]\nbackend = \"timeshare\"\nkind = \"guard\"\nneeds = { model = \"x\", field = \"d\" }\n",
	} {
		p := filepath.Join(dir, "c.toml")
		os.WriteFile(p, []byte(s), 0o644)
		if _, err := readClassify(p); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", s, err, want)
		}
	}
}
