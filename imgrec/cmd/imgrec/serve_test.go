package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"slopbooru/imgrec/internal/api"
	"slopbooru/imgrec/internal/auth"
	"slopbooru/imgrec/internal/sched"
)

type apiClient struct {
	t     *testing.T
	base  string
	token string
}

func (c apiClient) call(method, path string, body []byte) (int, map[string]any, http.Header) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(data, &out)
	return resp.StatusCode, out, resp.Header
}

func job(image, model string) []byte {
	b, _ := json.Marshal(map[string]string{"image": image, "model": model})
	return b
}

func shaOf(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// ask is POST /v1/ask: the answers, in order (nil: Nothing).
func (c apiClient) ask(wait int, qs ...[2]string) (int, []map[string]any) {
	c.t.Helper()
	var questions []map[string]string
	for _, q := range qs {
		questions = append(questions, map[string]string{"image": q[0], "model": q[1]})
	}
	body, _ := json.Marshal(map[string]any{"questions": questions})
	code, out, _ := c.call("POST", fmt.Sprintf("/v1/ask?wait=%d", wait), body)
	var answers []map[string]any
	if raw, ok := out["answers"].([]any); ok {
		for _, a := range raw {
			m, _ := a.(map[string]any)
			answers = append(answers, m)
		}
	}
	return code, answers
}

// submit is POST /v1/submit: the picture and the models to run it on.
func (c apiClient) submit(sha string, data []byte, models ...string) int {
	c.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("image", sha)
	mw.WriteField("models", strings.Join(models, ","))
	fw, _ := mw.CreateFormFile("file", "x.png")
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", c.base+"/v1/submit", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestServe(t *testing.T) {
	dir := t.TempDir()
	pictures(t, filepath.Join(dir, "pics"))
	t.Setenv("IMGREC_STUB_LOG", filepath.Join(dir, "stub.log"))
	f := fleet{db: filepath.Join(dir, "results.db"), specsDir: "../../specs", manifest: manifestFile(t, dir),
		vllm: stub, pixai: stub + " pixai-worker", logs: filepath.Join(dir, "logs"),
		rotation: 2 * time.Hour, maxAttempts: 3}
	p, sc, closeAll, err := f.open()
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()
	sc.Logf = t.Logf
	a, _ := auth.Open(p.Store.DB)
	alice, _ := a.Add("alice", false)
	bob, _ := a.Add("bob", false)
	root, _ := a.Add("root", true)
	if _, err := a.Add("alice", false); err == nil {
		t.Error("a second client named alice")
	}
	ts := httptest.NewServer(&api.Server{P: p, Auth: a, Sched: sc, MaxUpload: 64 << 20, MaxWait: 30 * time.Second})
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	schedDone := make(chan error, 1)
	go func() { schedDone <- sc.Run(ctx, false) }()
	defer func() { cancel(); <-schedDone }()

	A := apiClient{t, ts.URL, alice}
	B := apiClient{t, ts.URL, bob}
	R := apiClient{t, ts.URL, root}
	read := func(name string) ([]byte, string) {
		data, _ := os.ReadFile(filepath.Join(dir, "pics", name))
		return data, shaOf(data)
	}
	onDisk := func(sha string) bool { return p.Images.Path(sha) != "" }
	jobs := func(model, sha string) string {
		return query(t, f.db, "SELECT COUNT(*) FROM jobs WHERE model = '"+model+"' AND image_sha = '"+sha+"'")[0][0]
	}

	// tokens
	if code, _, h := (apiClient{t, ts.URL, ""}).call("GET", "/v1/models", nil); code != 401 || h.Get("WWW-Authenticate") == "" {
		t.Errorf("no token: %d", code)
	}
	if code, _, _ := (apiClient{t, ts.URL, "imgrec_nope"}).call("GET", "/v1/models", nil); code != 401 {
		t.Errorf("wrong token: %d", code)
	}
	if code, models, _ := A.call("GET", "/v1/models", nil); code != 200 || len(models["models"].([]any)) != 7 {
		t.Fatalf("models: %d %v", code, models)
	}

	p00, s00 := read("p00.png")
	// ask: Nothing, and nothing created
	if code, ans := A.ask(0, [2]string{s00, "pixai"}); code != 200 || len(ans) != 1 || ans[0] != nil {
		t.Fatalf("ask, never seen: %d %v", code, ans)
	}
	// submit, then ask (waiting): Just
	if code := A.submit(s00, p00, "pixai"); code != 202 {
		t.Fatalf("submit: %d", code)
	}
	if code, ans := A.ask(20, [2]string{s00, "pixai"}); code != 200 || ans[0] == nil || ans[0]["success"] != true {
		t.Fatalf("alice's answer: %d %v", code, ans)
	}
	// ask is pure: a model nobody submitted is Nothing, and no job appears
	if _, ans := A.ask(0, [2]string{s00, "qwen"}); ans[0] != nil || jobs("qwen", s00) != "0" {
		t.Errorf("ask must not create work: %v, %s jobs", ans, jobs("qwen", s00))
	}
	// bad questions are errors, not Nothing
	if code, _ := A.ask(0, [2]string{s00, "no-such-model"}); code != 400 {
		t.Errorf("ask with an unknown model: %d, want 400", code)
	}

	// bob, for alice's picture: Nothing, as for one nobody has -- a hash
	// alone tells him nothing
	if _, ans := B.ask(0, [2]string{s00, "pixai"}, [2]string{strings.Repeat("0", 64), "pixai"}); ans[0] != nil || ans[1] != nil {
		t.Errorf("bob's ask about alice's picture: %v, want [null null]", ans)
	}
	// bob sends the bytes: answered at once, from the one existing record
	if code := B.submit(s00, p00, "pixai"); code != 202 {
		t.Errorf("bob's submit: %d", code)
	}
	if _, ans := B.ask(0, [2]string{s00, "pixai"}); ans[0] == nil || ans[0]["success"] != true {
		t.Errorf("bob after sending the bytes: %v, want the answer at once", ans)
	}
	if n := query(t, f.db, "SELECT COUNT(*) FROM records WHERE model = 'pixai'")[0][0]; n != "1" {
		t.Errorf("%s pixai records, want 1 (asked once)", n)
	}

	// submits are checked
	if code := A.submit(strings.Repeat("1", 64), p00, "pixai"); code != 400 {
		t.Errorf("bytes that don't match the hash: %d, want 400", code)
	}
	if code := A.submit(shaOf([]byte("not a picture")), []byte("not a picture"), "pixai"); code != 400 {
		t.Errorf("not a picture: %d, want 400", code)
	}
	if code := A.submit(s00, p00, "no-such-model"); code != 400 {
		t.Errorf("submit for an unknown model: %d, want 400", code)
	}

	// a failure is an answer: joycaption's stub fails some pictures for
	// good, and those come back Just {success: false}, not Nothing for ever
	var qs [][2]string
	for i := range 12 {
		data, sha := read(fmt.Sprintf("p%02d.png", i))
		if code := A.submit(sha, data, "joycaption"); code != 202 {
			t.Fatalf("submit p%02d: %d", i, code)
		}
		qs = append(qs, [2]string{sha, "joycaption"})
	}
	_, ans := A.ask(30, qs...)
	failures := 0
	for i, x := range ans {
		if x == nil {
			t.Errorf("joycaption p%02d: still Nothing after the wait", i)
		} else if x["success"] == false {
			failures++
		}
	}
	if failures == 0 {
		t.Error("no failure came back as an answer (the stub fails 1 in 4)")
	}

	// uploads are deleted once no job needs them, stand-ins too; the
	// answers stay; a picture from `imgrec run` is never deleted
	large, sLarge := read("large.png")
	A.submit(sLarge, large, "deepseek-ocr")
	if _, ans := A.ask(20, [2]string{sLarge, "deepseek-ocr"}); ans[0] == nil {
		t.Fatal("large: no answer")
	}
	if standins, _ := filepath.Glob(filepath.Join(p.Images.Root, "standins", sLarge+"-*")); len(standins) != 1 {
		t.Fatalf("stand-in for the large picture: %v", standins)
	}
	// a picture nobody submitted, stored as `imgrec run` stores it
	var fresh bytes.Buffer
	im := image.NewRGBA(image.Rect(0, 0, 7, 5))
	im.Pix[0] = 201
	png.Encode(&fresh, im)
	freshPath := filepath.Join(dir, "batch-only.png")
	os.WriteFile(freshPath, fresh.Bytes(), 0o644)
	batch, err := p.Images.PutFile(freshPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Sweep(0); err != nil {
		t.Fatal(err)
	}
	if onDisk(s00) || onDisk(sLarge) {
		t.Error("an upload no job needs is still on disk")
	}
	if left, _ := filepath.Glob(filepath.Join(p.Images.Root, "standins", sLarge+"-*")); len(left) != 0 {
		t.Errorf("stand-ins left: %v", left)
	}
	if !onDisk(batch.SHA256) {
		t.Error("a picture from `imgrec run` was swept")
	}
	if _, ans := A.ask(0, [2]string{sLarge, "deepseek-ocr"}, [2]string{sLarge, "toriigate"}); ans[0] == nil || ans[1] != nil {
		t.Errorf("after the sweep: %v, want [the answer, null]", ans)
	}

	// the guard reads gemma's description: submitting it queues gemma too
	p01, s01 := read("p01.png")
	A.submit(s01, p01, "llamaguard")
	if _, ans := A.ask(30, [2]string{s01, "llamaguard"}, [2]string{s01, "gemma"}); ans[0] == nil || ans[1] == nil {
		t.Fatalf("guard and its describer: %v", ans)
	}

	// queue, admin
	if code, q, _ := A.call("GET", "/v1/queue", nil); code != 200 || q["client"] != "alice" || q["admin"] != false {
		t.Errorf("queue: %d %v", code, q)
	}
	if _, ans := R.ask(0, [2]string{s01, "gemma"}); ans[0] == nil {
		t.Error("admin can't read alice's answer")
	}

	// the client tool: two models over three pictures, then again --
	// the second time nothing is sent
	submit := func() string {
		out := filepath.Join(dir, "submit.jsonl")
		fh, _ := os.Create(out)
		stdout, stderr := os.Stdout, os.Stderr
		errf, _ := os.Create(filepath.Join(dir, "submit.err"))
		os.Stdout, os.Stderr = fh, errf
		err := submitCmd([]string{"--server", ts.URL, "--token", bob, "--models", "pixai,deepseek-ocr", "--wait", "20",
			filepath.Join(dir, "pics", "p02.png"), filepath.Join(dir, "pics", "p03.png"), filepath.Join(dir, "pics", "p04.png")})
		os.Stdout, os.Stderr = stdout, stderr
		fh.Close()
		errf.Close()
		if err != nil && err != errSomeFailed {
			t.Fatal(err)
		}
		lines, _ := os.ReadFile(out)
		if n := strings.Count(string(lines), "\n"); n != 6 {
			t.Errorf("submit printed %d answers, want 6", n)
		}
		msgs, _ := os.ReadFile(filepath.Join(dir, "submit.err"))
		return string(msgs)
	}
	if msgs := submit(); !strings.Contains(msgs, "3 picture(s) sent") {
		t.Errorf("first submit: %s", msgs)
	}
	if msgs := submit(); !strings.Contains(msgs, "0 picture(s) sent") {
		t.Errorf("second submit should send nothing: %s", msgs)
	}
}

// A db first touched by `imgrec client add` must serve jobs afterwards
// (it once made a placeholder jobs table that the server then kept).
func TestClientThenServe(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "imgrec.db")
	if err := clientCmd([]string{"--db", db, "list"}); err != nil {
		t.Fatal(err)
	}
	f := fleet{db: db, specsDir: "../../specs", manifest: manifestFile(t, dir), vllm: stub,
		pixai: stub + " pixai-worker", logs: filepath.Join(dir, "logs"), maxAttempts: 3}
	p, _, closeAll, err := f.open()
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()
	pictures(t, filepath.Join(dir, "pics"))
	im, err := p.Images.PutFile(filepath.Join(dir, "pics", "p00.png"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Intake("pixai", im.SHA256, "alice"); err != nil {
		t.Fatalf("intake on a db made by `client`: %v", err)
	}
}

// An empty queue leaves no runner up: once a model has had no work for
// --idle it is stopped (the card can power down), and the next job starts
// it again.
func TestIdleUnloads(t *testing.T) {
	dir := t.TempDir()
	pictures(t, filepath.Join(dir, "pics"))
	t.Setenv("IMGREC_STUB_LOG", filepath.Join(dir, "stub.log"))
	f := fleet{db: filepath.Join(dir, "results.db"), specsDir: "../../specs", manifest: manifestFile(t, dir),
		vllm: stub, pixai: stub + " pixai-worker", logs: filepath.Join(dir, "logs"),
		rotation: 2 * time.Hour, idle: 500 * time.Millisecond, maxAttempts: 3}
	p, sc, closeAll, err := f.open()
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()
	sc.Logf = t.Logf
	a, _ := auth.Open(p.Store.DB)
	alice, _ := a.Add("alice", false)
	ts := httptest.NewServer(&api.Server{P: p, Auth: a, Sched: sc, MaxUpload: 64 << 20, MaxWait: 30 * time.Second})
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	schedDone := make(chan error, 1)
	go func() { schedDone <- sc.Run(ctx, false) }()
	defer func() { cancel(); <-schedDone }()

	A := apiClient{t, ts.URL, alice}
	upNow := func() []string {
		var up []string
		for _, m := range sc.Snapshot() {
			if m.State != sched.Down {
				up = append(up, m.Name)
			}
		}
		return up
	}
	waitAllDown := func() {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if len(upNow()) == 0 {
				return
			}
		}
		t.Fatalf("still loaded with nothing to do: %v", upNow())
	}
	for _, name := range []string{"p00.png", "p01.png"} {
		data, _ := os.ReadFile(filepath.Join(dir, "pics", name))
		sha := shaOf(data)
		if code := A.submit(sha, data, "pixai", "gemma"); code != 202 {
			t.Fatalf("submit %s: %d", name, code)
		}
		if _, ans := A.ask(20, [2]string{sha, "pixai"}, [2]string{sha, "gemma"}); ans[0] == nil || ans[1] == nil {
			t.Fatalf("%s: %v", name, ans)
		}
		waitAllDown() // and the second picture brings them back
	}
}
