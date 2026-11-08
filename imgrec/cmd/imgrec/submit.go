package main

// imgrec submit: the API's client.
//
//  1. ask every (picture, model) question, in bundles
//  2. submit each picture whose answers came back Nothing, once, for
//     exactly those models
//  3. ask what's still Nothing again, waiting, until every answer is Just
//
// Answers go to stdout, one json line each, as they arrive. Re-running
// with the same inputs is safe: what's answered comes back at once, and a
// picture is only sent again if its answer is still missing.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type client struct {
	base, token string
	http        *http.Client
}

func (c *client) call(method, path, ctype string, body io.Reader, out any) (int, error) {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if ctype != "" {
		req.Header.Set("content-type", ctype)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode >= 400 {
		var e struct{ Error string }
		json.Unmarshal(data, &e)
		return resp.StatusCode, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, e.Error)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

type question struct {
	Image string `json:"image"`
	Model string `json:"model"`
	file  string
}

// ask returns the answers to qs, in order (nil: Nothing).
func (c *client) ask(qs []question, wait int) ([]map[string]any, error) {
	body, _ := json.Marshal(map[string]any{"questions": qs})
	var res struct {
		Answers []map[string]any `json:"answers"`
	}
	path := "/v1/ask"
	if wait > 0 {
		path += fmt.Sprintf("?wait=%d", wait)
	}
	if _, err := c.call("POST", path, "application/json", bytes.NewReader(body), &res); err != nil {
		return nil, err
	}
	if len(res.Answers) != len(qs) {
		return nil, fmt.Errorf("asked %d questions, got %d answers", len(qs), len(res.Answers))
	}
	return res.Answers, nil
}

func (c *client) submit(file, sha string, models []string, data []byte) error {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("image", sha)
	mw.WriteField("models", strings.Join(models, ","))
	fw, _ := mw.CreateFormFile("file", filepath.Base(file))
	fw.Write(data)
	mw.Close()
	_, err := c.call("POST", "/v1/submit", mw.FormDataContentType(), &body, nil)
	return err
}

// errSomeFailed: every answer was printed, but not every job succeeded.
var errSomeFailed = fmt.Errorf("some jobs failed")

const bundle = 512 // questions per ask

func submitCmd(args []string) error {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	server := fs.String("server", env("SERVER", "http://127.0.0.1:8100"), "the imgrec server")
	token := fs.String("token", env("TOKEN", ""), "your token (or IMGREC_TOKEN)")
	only := fs.String("models", "", "comma-separated models (default: every model the server has)")
	wait := fs.Int("wait", 60, "seconds each ask may wait on the server")
	cacert := fs.String("cacert", env("CACERT", ""), "trust this certificate (PEM) for an https server, e.g. one made by imgrec cert")
	fs.Parse(args)
	if *token == "" || fs.NArg() == 0 {
		return fmt.Errorf("usage: imgrec submit --token T [--server URL] [--cacert F] [--models a,b] FILE...")
	}
	tlsConf, err := clientTLS(*cacert)
	if err != nil {
		return err
	}
	if cleartextRemote(*server) {
		fmt.Fprintf(os.Stderr, "imgrec: warning: %s is plain http: the token crosses the network in the clear\n", *server)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsConf
	c := &client{base: strings.TrimRight(*server, "/"), token: *token,
		http: &http.Client{Timeout: time.Duration(*wait+60) * time.Second, Transport: tr}}

	var info struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if _, err := c.call("GET", "/v1/models", "", nil, &info); err != nil {
		return err
	}
	want, has := map[string]bool{}, map[string]bool{}
	for _, m := range strings.Split(*only, ",") {
		if m = strings.TrimSpace(m); m != "" {
			want[m] = true
		}
	}
	var models []string
	for _, m := range info.Models {
		has[m.Name] = true
		if len(want) == 0 || want[m.Name] {
			models = append(models, m.Name)
		}
	}
	for m := range want {
		if !has[m] {
			return fmt.Errorf("the server has no model %q", m)
		}
	}

	shaOf := map[string]string{}
	var qs []question
	for _, file := range fs.Args() {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		h := sha256.Sum256(data)
		shaOf[file] = hex.EncodeToString(h[:])
		for _, m := range models {
			qs = append(qs, question{Image: shaOf[file], Model: m, file: file})
		}
	}

	out := json.NewEncoder(os.Stdout)
	out.SetEscapeHTML(false)
	done, failed := 0, 0
	emit := func(q question, a map[string]any) {
		a["file"] = q.file
		out.Encode(a)
		if a["success"] == true {
			done++
		} else {
			failed++
		}
	}
	// askAll asks the questions in bundles; returns those still Nothing
	askAll := func(qs []question, wait int) ([]question, error) {
		var pending []question
		for i := 0; i < len(qs); i += bundle {
			chunk := qs[i:min(i+bundle, len(qs))]
			answers, err := c.ask(chunk, wait)
			if err != nil {
				return nil, err
			}
			for j, a := range answers {
				if a == nil {
					pending = append(pending, chunk[j])
				} else {
					emit(chunk[j], a)
				}
			}
		}
		return pending, nil
	}

	// 1. ask everything
	pending, err := askAll(qs, 0)
	if err != nil {
		return err
	}
	// 2. submit each picture that has a Nothing, once, for those models
	need := map[string][]string{}
	var order []string
	for _, q := range pending {
		if need[q.file] == nil {
			order = append(order, q.file)
		}
		need[q.file] = append(need[q.file], q.Model)
	}
	for _, file := range order {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if err := c.submit(file, shaOf[file], need[file], data); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
	}
	fmt.Fprintf(os.Stderr, "imgrec: %d answered already, %d picture(s) sent for %d question(s)\n",
		len(qs)-len(pending), len(order), len(pending))
	// 3. ask again, waiting, until every answer is Just
	start := time.Now()
	for len(pending) > 0 {
		before := len(pending)
		if pending, err = askAll(pending, *wait); err != nil {
			return err
		}
		if len(pending) < before {
			fmt.Fprintf(os.Stderr, "imgrec: %d of %d answered (%s)\n",
				len(qs)-len(pending), len(qs), time.Since(start).Round(time.Second))
		}
	}
	fmt.Fprintf(os.Stderr, "imgrec: %d done, %d failed\n", done, failed)
	if failed > 0 {
		return errSomeFailed
	}
	return nil
}
