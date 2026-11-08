package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"imgrec/internal/api"
	"imgrec/internal/auth"
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

// upload is a job with the picture attached (after a need_image).
func (c apiClient) upload(sha string, data []byte, models ...string) (int, map[string]any) {
	c.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("image", sha)
	mw.WriteField("models", strings.Join(models, ","))
	fw, _ := mw.CreateFormFile("file", "x.png")
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", c.base+"/v1/jobs", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
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
	// by hash, nothing anywhere: need_image
	if code, res, _ := A.call("POST", "/v1/jobs", job(s00, "pixai")); code != 404 || res["need_image"] != true {
		t.Fatalf("hash-only, never seen: %d %v", code, res)
	}
	// with the picture: queued, then answered
	code, res := A.upload(s00, p00, "pixai")
	if code != 202 {
		t.Fatalf("upload with the job: %d %v", code, res)
	}
	if code, res, _ := A.call("GET", "/v1/jobs/pixai/"+s00+"?wait=20", nil); code != 200 || res["status"] != "done" {
		t.Fatalf("alice's answer: %d %v", code, res)
	}
	// alice again, by hash: answered without sending anything
	if code, res, _ := A.call("POST", "/v1/jobs", job(s00, "pixai")); code != 200 || res["status"] != "done" {
		t.Errorf("alice by hash after her upload: %d %v", code, res)
	}

	// bob, by hash, for the same picture: need_image -- exactly what a miss
	// looks like, though the answer exists. A hash alone tells him nothing.
	code, resB, _ := B.call("POST", "/v1/jobs", job(s00, "pixai"))
	_, resMiss, _ := B.call("POST", "/v1/jobs", job(strings.Repeat("0", 64), "pixai"))
	delete(resB, "image")
	delete(resMiss, "image")
	if code != 404 || !reflect.DeepEqual(resB, resMiss) {
		t.Errorf("bob by hash for alice's picture: %d %v -- must equal a genuine miss %v", code, resB, resMiss)
	}
	if code, _, _ := B.call("GET", "/v1/jobs/pixai/"+s00, nil); code != 404 {
		t.Errorf("bob reading alice's job: %d, want 404", code)
	}
	// bob sends the bytes: answered at once, from the one existing record
	if code, res := B.upload(s00, p00, "pixai"); code != 200 || res["jobs"].([]any)[0].(map[string]any)["status"] != "done" {
		t.Errorf("bob with the bytes: %d %v, want 200 done at once", code, res)
	}
	if n := query(t, f.db, "SELECT COUNT(*) FROM records WHERE model = 'pixai'")[0][0]; n != "1" {
		t.Errorf("%s pixai records, want 1 (asked once)", n)
	}

	// uploads are verified
	if code, _ := A.upload(strings.Repeat("1", 64), p00, "pixai"); code != 400 {
		t.Errorf("bytes that don't match the hash: %d, want 400", code)
	}
	if code, _ := A.upload(shaOf([]byte("not a picture")), []byte("not a picture"), "pixai"); code != 400 {
		t.Errorf("not a picture: %d, want 400", code)
	}

	// an upload is deleted once no job needs it -- its stand-in too -- and
	// its answers stay. A picture from `imgrec run` is never deleted.
	large, sLarge := read("large.png")
	if code, _ := A.upload(sLarge, large, "deepseek-ocr"); code != 202 {
		t.Fatalf("large upload: %d", code)
	}
	if code, _, _ := A.call("GET", "/v1/jobs/deepseek-ocr/"+sLarge+"?wait=20", nil); code != 200 {
		t.Fatalf("large answer: %d", code)
	}
	standins, _ := filepath.Glob(filepath.Join(p.Images.Root, "standins", sLarge+"-*"))
	if len(standins) != 1 {
		t.Fatalf("stand-in for the large picture: %v", standins)
	}
	batch, err := p.Images.PutFile(filepath.Join(dir, "pics", "p05.png")) // as `imgrec run` stores it
	if err != nil {
		t.Fatal(err)
	}
	if n, err := p.Sweep(0); err != nil || n != 2 {
		t.Errorf("sweep: %d %v, want the 2 uploads", n, err)
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
	if code, _, _ := A.call("POST", "/v1/jobs", job(sLarge, "deepseek-ocr")); code != 200 {
		t.Errorf("the answer outlives the picture: %d", code)
	}
	// a model that hasn't answered it yet needs the bytes again
	if code, res, _ := A.call("POST", "/v1/jobs", job(sLarge, "joycaption")); code != 404 || res["need_image"] != true {
		t.Errorf("a new model after the sweep: %d %v, want need_image", code, res)
	}

	// the guard reads gemma's description: asking for it queues gemma too
	p01, s01 := read("p01.png")
	if code, _ := A.upload(s01, p01, "llamaguard"); code != 202 {
		t.Fatalf("guard upload: %d", code)
	}
	if code, res, _ := A.call("GET", "/v1/jobs/llamaguard/"+s01+"?wait=30", nil); code != 200 || res["status"] != "done" {
		t.Fatalf("guard answer: %d %v", code, res)
	}
	if code, _, _ := A.call("GET", "/v1/jobs/gemma/"+s01, nil); code != 200 {
		t.Errorf("the guard's dependency gemma: %d, want done", code)
	}

	// cancel, queue, admin
	if code, _, _ := A.call("DELETE", "/v1/jobs/qwen/"+s01, nil); code != 204 {
		t.Errorf("cancel: %d", code)
	}
	if code, q, _ := A.call("GET", "/v1/queue", nil); code != 200 || q["client"] != "alice" || q["admin"] != false {
		t.Errorf("queue: %d %v", code, q)
	}
	if code, res, _ := R.call("GET", "/v1/jobs/gemma/"+s01, nil); code != 200 || res["status"] != "done" {
		t.Errorf("admin reading alice's answer: %d", code)
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
	if msgs := submit(); !strings.Contains(msgs, "3 picture(s) had to be sent") {
		t.Errorf("first submit: %s", msgs)
	}
	if msgs := submit(); !strings.Contains(msgs, "0 picture(s) had to be sent") {
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
