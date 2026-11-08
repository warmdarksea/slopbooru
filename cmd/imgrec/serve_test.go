package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

	// tokens
	if code, _, h := (apiClient{t, ts.URL, ""}).call("GET", "/v1/models", nil); code != 401 || h.Get("WWW-Authenticate") == "" {
		t.Errorf("no token: %d", code)
	}
	if code, _, _ := (apiClient{t, ts.URL, "imgrec_nope"}).call("GET", "/v1/models", nil); code != 401 {
		t.Errorf("wrong token: %d", code)
	}
	code, models, _ := A.call("GET", "/v1/models", nil)
	if code != 200 || len(models["models"].([]any)) != 7 {
		t.Fatalf("models: %d %v", code, models)
	}

	// uploads: by hash, verified
	p00, _ := os.ReadFile(filepath.Join(dir, "pics", "p00.png"))
	p01, _ := os.ReadFile(filepath.Join(dir, "pics", "p01.png"))
	s00, s01 := shaOf(p00), shaOf(p01)
	if code, _, _ := A.call("PUT", "/v1/images/"+s00, p00); code != 201 {
		t.Errorf("first upload: %d, want 201", code)
	}
	if code, _, _ := A.call("PUT", "/v1/images/"+s00, p00); code != 200 {
		t.Errorf("same upload again: %d, want 200", code)
	}
	if code, _, _ := A.call("PUT", "/v1/images/"+s01, p00); code != 400 {
		t.Errorf("bytes that don't match the hash: %d, want 400", code)
	}
	if code, _, _ := A.call("POST", "/v1/images", []byte("not a picture")); code != 400 {
		t.Errorf("not a picture: %d, want 400", code)
	}
	if code, body, _ := A.call("POST", "/v1/images", p01); code != 201 || body["image"] != s01 {
		t.Errorf("upload without a hash: %d %v", code, body)
	}

	// bob can't see alice's picture -- 404, not 403
	if code, _, _ := B.call("POST", "/v1/jobs", job(s00, "pixai")); code != 404 {
		t.Errorf("bob asking about alice's picture: %d, want 404", code)
	}

	// alice: queued, then answered
	code, res, h := A.call("POST", "/v1/jobs", job(s00, "pixai"))
	if code != 202 || h.Get("Location") != "/v1/jobs/pixai/"+s00 {
		t.Fatalf("alice's job: %d %v %q", code, res, h.Get("Location"))
	}
	code, res, _ = A.call("GET", "/v1/jobs/pixai/"+s00+"?wait=20", nil)
	if code != 200 || res["status"] != "done" {
		t.Fatalf("alice's answer: %d %v", code, res)
	}
	if B.token != "" {
		if code, _, _ := B.call("GET", "/v1/jobs/pixai/"+s00, nil); code != 404 {
			t.Errorf("bob reading alice's answer: %d, want 404", code)
		}
	}

	// bob uploads the same bytes: new to him (201), and the answer is
	// already there -- one record, asked once
	if code, _, _ := B.call("PUT", "/v1/images/"+s00, p00); code != 201 {
		t.Errorf("bob's upload of the same bytes: %d, want 201", code)
	}
	if code, res, _ := B.call("POST", "/v1/jobs", job(s00, "pixai")); code != 200 || res["status"] != "done" {
		t.Errorf("bob's job: %d %v, want 200 at once", code, res)
	}
	if n := query(t, f.db, "SELECT COUNT(*) FROM records WHERE model = 'pixai'")[0][0]; n != "1" {
		t.Errorf("%s pixai records, want 1", n)
	}

	// the guard reads gemma's description: asking for it queues gemma too
	if code, _, _ := A.call("POST", "/v1/jobs", job(s01, "llamaguard")); code != 202 {
		t.Fatalf("guard job: %d", code)
	}
	code, res, _ = A.call("GET", "/v1/jobs/llamaguard/"+s01+"?wait=30", nil)
	if code != 200 || res["status"] != "done" {
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

	// the client tool, end to end: two models over three pictures
	out := filepath.Join(dir, "submit.jsonl")
	fh, _ := os.Create(out)
	stdout := os.Stdout
	os.Stdout = fh
	err = submitCmd([]string{"--server", ts.URL, "--token", alice, "--models", "pixai,deepseek-ocr", "--wait", "20",
		filepath.Join(dir, "pics", "p02.png"), filepath.Join(dir, "pics", "p03.png"), filepath.Join(dir, "pics", "large.png")})
	os.Stdout = stdout
	fh.Close()
	if err != nil && err != errSomeFailed {
		t.Fatal(err)
	}
	lines := 0
	sc2, _ := os.Open(out)
	scanner := bufio.NewScanner(sc2)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	for scanner.Scan() {
		var r map[string]any
		json.Unmarshal(scanner.Bytes(), &r)
		if r["status"] != "done" && !strings.Contains(r["file"].(string), "large") {
			t.Errorf("submit: %v", r)
		}
		lines++
	}
	sc2.Close()
	if lines != 6 {
		t.Errorf("submit printed %d answers, want 6", lines)
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
