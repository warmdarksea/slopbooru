package main

// imgrec submit: the client. Asks the server which models it has, uploads
// each picture, asks for each (picture, model) job, and waits for the
// answers. Every call is idempotent, so a crashed or interrupted submit is
// simply run again with the same inputs: what's answered comes back at
// once, and nothing is queued twice. Results go to stdout, one json line
// per job.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type client struct {
	base, token string
	http        *http.Client
}

func (c *client) do(method, path string, body io.Reader, out any) (int, string, error) {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if method == "POST" && path == "/v1/jobs" {
		req.Header.Set("content-type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", err
	}
	if resp.StatusCode >= 400 {
		var e struct{ Error string }
		json.Unmarshal(data, &e)
		return resp.StatusCode, "", fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, e.Error)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, "", err
		}
	}
	return resp.StatusCode, resp.Header.Get("Location"), nil
}

// errSomeFailed: every answer was printed, but not every job succeeded.
var errSomeFailed = fmt.Errorf("some jobs failed")

func submitCmd(args []string) error {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	server := fs.String("server", env("SERVER", "http://127.0.0.1:8100"), "the imgrec server")
	token := fs.String("token", env("TOKEN", ""), "your token (or IMGREC_TOKEN)")
	only := fs.String("models", "", "comma-separated models (default: every model the server has)")
	wait := fs.Int("wait", 60, "seconds each poll may wait on the server")
	parallel := fs.Int("parallel", 8, "polls in flight")
	fs.Parse(args)
	if *token == "" || fs.NArg() == 0 {
		return fmt.Errorf("usage: imgrec submit --token T [--server URL] [--models a,b] FILE...")
	}
	c := &client{base: strings.TrimRight(*server, "/"), token: *token,
		http: &http.Client{Timeout: time.Duration(*wait+30) * time.Second}}

	var info struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if _, _, err := c.do("GET", "/v1/models", nil, &info); err != nil {
		return err
	}
	var models []string
	want, has := map[string]bool{}, map[string]bool{}
	for _, m := range strings.Split(*only, ",") {
		if m = strings.TrimSpace(m); m != "" {
			want[m] = true
		}
	}
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

	out := json.NewEncoder(os.Stdout)
	out.SetEscapeHTML(false)
	var outMu sync.Mutex
	emit := func(file string, v map[string]any) {
		v["file"] = file
		outMu.Lock()
		out.Encode(v)
		outMu.Unlock()
	}
	type pending struct{ file, url string }
	var waiting []pending
	done, failed := 0, 0
	for _, file := range fs.Args() {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		h := sha256.Sum256(data)
		sha := hex.EncodeToString(h[:])
		if _, _, err := c.do("PUT", "/v1/images/"+sha, bytes.NewReader(data), nil); err != nil {
			fmt.Fprintf(os.Stderr, "imgrec: %s: %v\n", file, err)
			failed += len(models)
			continue
		}
		for _, m := range models {
			body, _ := json.Marshal(map[string]string{"image": sha, "model": m})
			var res map[string]any
			code, loc, err := c.do("POST", "/v1/jobs", bytes.NewReader(body), &res)
			switch {
			case err != nil:
				fmt.Fprintf(os.Stderr, "imgrec: %s on %s: %v\n", m, file, err)
				failed++
			case code == 202:
				waiting = append(waiting, pending{file, loc})
			default:
				emit(file, res)
				if res["status"] == "done" {
					done++
				} else {
					failed++
				}
			}
		}
	}
	fmt.Fprintf(os.Stderr, "imgrec: %d answered already, %d queued, %d failed\n", done, len(waiting), failed)

	// wait for the rest, a few at a time, each poll held by the server
	var mu sync.Mutex
	work := make(chan pending)
	var wg sync.WaitGroup
	start := time.Now()
	for range max(1, *parallel) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range work {
				for {
					var res map[string]any
					code, _, err := c.do("GET", fmt.Sprintf("%s?wait=%d", p.url, *wait), nil, &res)
					if err != nil {
						fmt.Fprintf(os.Stderr, "imgrec: %s: %v\n", p.url, err)
						mu.Lock()
						failed++
						mu.Unlock()
						break
					}
					if code == 202 {
						continue
					}
					emit(p.file, res)
					mu.Lock()
					if res["status"] == "done" {
						done++
					} else {
						failed++
					}
					n := done + failed
					mu.Unlock()
					if n%25 == 0 {
						fmt.Fprintf(os.Stderr, "imgrec: %d answered (%s)\n", n, time.Since(start).Round(time.Second))
					}
					break
				}
			}
		}()
	}
	for _, p := range waiting {
		work <- p
	}
	close(work)
	wg.Wait()
	fmt.Fprintf(os.Stderr, "imgrec: %d done, %d failed\n", done, failed)
	if failed > 0 {
		return errSomeFailed
	}
	return nil
}
