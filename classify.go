package main

// slopbooru classify: ask models about the pictures in a directory, and
// write what they said into the db the booru browses.
//
//	slopbooru classify --images DIR [--db DIR/results.db] [--config FILE] [--models a,b]
//
// The models, and where they are asked, are classify.toml's (built in; a
// --config file is read on top). A backend is anything that speaks the
// OpenAI API -- gpu-timeshare running the fleet, one vllm server, a hosted
// API -- asked through the Batch API (the default: the backend decides the
// order, which is what lets gpu-timeshare answer one model at a time) or
// with one request per picture.
//
// Every answer is kept verbatim in the db (classify_answers), keyed by the
// picture and the question: the model's whole entry in the config. A
// picture that has an answer to the current question is not asked again,
// so a run that stops part way resumes, a new picture costs one request
// per model, and a changed prompt asks everything again (the new answers
// are stored beside the old). --reparse rebuilds the db's rows from the
// kept answers, asking nobody.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"slopbooru/internal/images"
	"slopbooru/internal/imagestore"
	"slopbooru/internal/model"
	"slopbooru/internal/store"
)

//go:embed classify.toml
var defaultClassify string

// ---- configuration ---------------------------------------------------------

type cduration struct{ time.Duration }

func (d *cduration) UnmarshalText(b []byte) error {
	s := strings.TrimSpace(string(b))
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return fmt.Errorf("duration %q", s)
		}
		d.Duration = time.Duration(days * 24 * float64(time.Hour))
		return nil
	}
	v, err := time.ParseDuration(s)
	d.Duration = v
	return err
}

type csize int64

func (z *csize) UnmarshalText(b []byte) error {
	s := strings.TrimSpace(string(b))
	mult := 1.0
	for _, u := range []struct {
		s string
		m float64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"B", 1}} {
		if n, ok := strings.CutSuffix(s, u.s); ok {
			s, mult = strings.TrimSpace(n), u.m
			break
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("size %q", string(b))
	}
	*z = csize(v * mult)
	return nil
}

type backendConf struct {
	URL         string    `toml:"url"`
	Token       string    `toml:"token"`
	TokenEnv    string    `toml:"token_env"`
	TokenFile   string    `toml:"token_file"`
	CACert      string    `toml:"cacert"`
	Mode        string    `toml:"mode"` // batch | requests
	BatchBytes  csize     `toml:"batch_bytes"`
	Concurrency int       `toml:"concurrency"`
	Poll        cduration `toml:"poll"`
	Window      string    `toml:"window"`
	Timeout     cduration `toml:"timeout"` // one request (requests mode)

	name   string
	token  string
	client *http.Client
}

type needsConf struct {
	Model string `toml:"model"`
	Field string `toml:"field"`
}

type modelConf struct {
	Enabled   *bool          `toml:"enabled"`
	Backend   string         `toml:"backend"`
	Model     string         `toml:"model"` // the name at the backend (default: the entry's)
	Kind      string         `toml:"kind"`
	System    string         `toml:"system"`
	Prompt    string         `toml:"prompt"`
	Schema    string         `toml:"schema"`
	MaxPixels int            `toml:"max_pixels"`
	Seed      bool           `toml:"seed"`
	Params    map[string]any `toml:"params"`
	Needs     *needsConf     `toml:"needs"`

	name     string
	backend  *backendConf
	schema   any
	question map[string]any // everything that decides the answer
	qhash    string
	round    int
}

type classifyConf struct {
	Backends map[string]*backendConf `toml:"backends"`
	Models   map[string]*modelConf   `toml:"models"`
}

var kinds = map[string]bool{"fields": true, "caption": true, "ocr": true, "guard": true, "tagger": true}

func readClassify(path string) (*classifyConf, error) {
	var c classifyConf
	if _, err := toml.Decode(defaultClassify, &c); err != nil {
		return nil, fmt.Errorf("the built-in classify.toml: %w", err)
	}
	if path != "" {
		var over classifyConf
		md, err := toml.DecodeFile(path, &over)
		if err != nil {
			return nil, err
		}
		if und := md.Undecoded(); len(und) > 0 {
			var keys []string
			for _, k := range und {
				keys = append(keys, k.String())
			}
			return nil, fmt.Errorf("%s: unknown setting(s): %s", path, strings.Join(keys, ", "))
		}
		for k, v := range over.Backends {
			c.Backends[k] = v
		}
		for k, v := range over.Models {
			c.Models[k] = v
		}
	}
	for name, b := range c.Backends {
		b.name = name
		if b.URL == "" {
			return nil, fmt.Errorf("backend %s: no url", name)
		}
		b.URL = strings.TrimRight(b.URL, "/")
		if b.Mode == "" {
			b.Mode = "batch"
		}
		if b.Mode != "batch" && b.Mode != "requests" {
			return nil, fmt.Errorf("backend %s: mode %q: batch or requests", name, b.Mode)
		}
		if b.BatchBytes <= 0 {
			b.BatchBytes = 190e6
		}
		if b.Concurrency <= 0 {
			b.Concurrency = 8
		}
		if b.Poll.Duration <= 0 {
			b.Poll.Duration = 15 * time.Second
		}
		if b.Window == "" {
			b.Window = "24h"
		}
		if b.Timeout.Duration <= 0 {
			b.Timeout.Duration = 30 * time.Minute // a model may be loading
		}
	}
	for name, m := range c.Models {
		if m.Enabled != nil && !*m.Enabled {
			delete(c.Models, name)
			continue
		}
		m.name = name
		if !kinds[m.Kind] {
			return nil, fmt.Errorf("model %s: kind %q: one of fields, caption, ocr, guard, tagger", name, m.Kind)
		}
		if m.backend = c.Backends[m.Backend]; m.backend == nil {
			return nil, fmt.Errorf("model %s: no backend %q", name, m.Backend)
		}
		if m.Model == "" {
			m.Model = name
		}
		if m.Schema != "" {
			d := json.NewDecoder(strings.NewReader(m.Schema))
			d.UseNumber()
			if err := d.Decode(&m.schema); err != nil {
				return nil, fmt.Errorf("model %s: schema: %w", name, err)
			}
		}
	}
	for name, m := range c.Models {
		if m.Needs != nil && c.Models[m.Needs.Model] == nil {
			return nil, fmt.Errorf("model %s needs %s, which isn't configured (or is disabled)", name, m.Needs.Model)
		}
	}
	// rounds: a model is asked after every model it reads
	for _, m := range c.Models {
		seen := map[string]bool{}
		for d := m; d.Needs != nil; d = c.Models[d.Needs.Model] {
			if seen[d.name] {
				return nil, fmt.Errorf("model %s: its needs go round in a circle", m.name)
			}
			seen[d.name] = true
			m.round++
		}
	}
	for _, m := range c.Models {
		m.question = jsonRoundTrip(map[string]any{
			"v": 1, "backend": m.backend.URL, "model": m.Model, "kind": m.Kind,
			"system": m.System, "prompt": m.Prompt, "schema": m.schema, "params": m.Params,
			"seed": m.Seed, "max_pixels": m.MaxPixels, "resize": resizeName(m.MaxPixels), "needs": m.Needs,
		}).(map[string]any)
		m.qhash = sha(canonical(m.question))
	}
	return &c, nil
}

func resizeName(maxPixels int) any {
	if maxPixels <= 0 {
		return nil
	}
	return imagestore.Resize
}

// jsonRoundTrip makes a value plain JSON (maps, slices, json.Number), as
// the store's writer and canonical() expect.
func jsonRoundTrip(v any) any {
	b, _ := json.Marshal(v)
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var out any
	d.Decode(&out)
	return out
}

func canonical(v any) string {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	e.Encode(v) // maps encode with sorted keys
	return strings.TrimSuffix(buf.String(), "\n")
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// seed is per picture, so a re-run reproduces the sample.
func seed(inputSHA string) int64 {
	n, _ := strconv.ParseUint(inputSHA[:8], 16, 32)
	return int64(n & 0x7fffffff)
}

// ---- the backend's client --------------------------------------------------

func (b *backendConf) setup() error {
	switch {
	case b.Token != "":
		b.token = b.Token
	case b.TokenFile != "":
		t, err := os.ReadFile(b.TokenFile)
		if err != nil {
			return fmt.Errorf("backend %s: %w", b.name, err)
		}
		b.token = strings.TrimSpace(string(t))
	case b.TokenEnv != "":
		b.token = os.Getenv(b.TokenEnv)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if b.CACert != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		pem, err := os.ReadFile(b.CACert)
		if err != nil {
			return fmt.Errorf("backend %s: %w", b.name, err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return fmt.Errorf("backend %s: %s holds no PEM certificate", b.name, b.CACert)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	b.client = &http.Client{Transport: tr}
	return nil
}

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string { return fmt.Sprintf("%d: %s", e.Status, clip(e.Body, 300)) }

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// call makes one API call and returns the body; anything but a 2xx is an
// *apiError.
func (b *backendConf) call(ctx context.Context, method, path, ctype string, body io.Reader, timeout time.Duration) ([]byte, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, b.URL+path, body)
	if err != nil {
		return nil, err
	}
	if b.token != "" {
		req.Header.Set("Authorization", "Bearer "+b.token)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return out, &apiError{resp.StatusCode, string(out)}
	}
	return out, nil
}

// ---- the db's own tables ---------------------------------------------------

// classify's bookkeeping, beside the store's tables (which it writes
// through internal/store). The booru reads none of these.
const classifySchema = `
CREATE TABLE IF NOT EXISTS classify_questions (
  question TEXT PRIMARY KEY,   -- sha256 of the canonical json
  model    TEXT NOT NULL,
  json     TEXT NOT NULL
);
-- every answer, verbatim: what --reparse rebuilds the rows from
CREATE TABLE IF NOT EXISTS classify_answers (
  image_sha   TEXT NOT NULL,
  question    TEXT NOT NULL REFERENCES classify_questions(question),
  context_sha TEXT NOT NULL,   -- '' unless the question reads another model's answer
  context     TEXT,
  fingerprint TEXT,            -- the backend's system_fingerprint
  raw         TEXT NOT NULL,   -- the response body
  at          TEXT NOT NULL,
  PRIMARY KEY (image_sha, question, context_sha)
);
-- batches submitted and not yet read back: a run that stops resumes them
CREATE TABLE IF NOT EXISTS classify_batches (
  id       TEXT NOT NULL,
  backend  TEXT NOT NULL,      -- its url
  model    TEXT NOT NULL,
  question TEXT NOT NULL,
  created  TEXT NOT NULL,
  PRIMARY KEY (backend, id)
);
CREATE TABLE IF NOT EXISTS classify_pending (
  backend     TEXT NOT NULL,
  batch_id    TEXT NOT NULL,
  custom_id   TEXT NOT NULL,
  image_sha   TEXT NOT NULL,
  context_sha TEXT NOT NULL,
  context     TEXT,
  PRIMARY KEY (backend, batch_id, custom_id)
);`

// ---- one question about one picture ----------------------------------------

type item struct {
	sha     string // the picture
	path    string
	context string // what a guard reads ("" otherwise)
	ctxSHA  string
}

func (it item) customID() string {
	if it.ctxSHA == "" {
		return it.sha
	}
	return it.sha + ":" + it.ctxSHA[:16]
}

type classifier struct {
	st      *store.Store
	cfg     *classifyConf
	index   *Index
	standin string // stand-ins' directory
	mu      sync.Mutex
	asked   map[string]int // per model, this run
	failed  map[string]int
	logged  time.Time
}

// body is the request for one picture.
func (c *classifier) body(m *modelConf, it item) (map[string]any, error) {
	in, err := imagestore.StandIn(it.path, it.sha, c.standin, m.MaxPixels)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(in)
	if err != nil {
		return nil, err
	}
	mime := "image/png"
	if in == it.path {
		d, err := images.DescribeKnown(it.path, it.sha)
		if err == nil && d.Mime != "" {
			mime = d.Mime
		}
	}
	body := map[string]any{}
	params, _ := jsonRoundTrip(m.Params).(map[string]any) // nil: no params
	for k, v := range params {
		body[k] = v
	}
	body["model"] = m.Model
	var msgs []any
	if m.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": m.System})
	}
	content := []any{map[string]any{"type": "image_url", "image_url": map[string]any{
		"url": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)}}}
	if m.Prompt != "" {
		content = append(content, map[string]any{"type": "text", "text": strings.ReplaceAll(m.Prompt, "{context}", it.context)})
	}
	body["messages"] = append(msgs, map[string]any{"role": "user", "content": content})
	if m.schema != nil {
		body["response_format"] = map[string]any{"type": "json_schema",
			"json_schema": map[string]any{"name": "slopbooru_" + m.Kind, "schema": m.schema}}
	}
	if m.Seed {
		h := sha256.Sum256(data)
		body["seed"] = seed(hex.EncodeToString(h[:]))
	}
	return body, nil
}

var contextLabels = map[string]string{"description": "Description", "text": "Text in the image",
	"tag": "Tags", "subject": "Characters"}

// contextFor is what a guard reads about a picture: the needed model's
// value -- the most confident, then the longest -- labelled, whitespace
// collapsed, clipped to 700 characters. "" when that model said nothing.
func (c *classifier) contextFor(n *needsConf, imageSHA string) (string, error) {
	var v string
	err := c.st.DB.QueryRow(`SELECT a.value FROM annotations a
		JOIN provenances p ON p.id = a.provenance_id JOIN images i ON i.id = a.image_id
		WHERE p.model = ? AND a.field = ? AND i.sha256 = ?
		ORDER BY a.confidence IS NULL, a.confidence DESC, length(a.value) DESC, a.value LIMIT 1`,
		n.Model, n.Field, imageSHA).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	v = strings.Join(strings.Fields(v), " ")
	if v == "" {
		return "", nil
	}
	if utf8.RuneCountInString(v) > 700 {
		v = string([]rune(v)[:699]) + "…"
	}
	label := contextLabels[n.Field]
	if label == "" {
		label = n.Field
	}
	return label + ": " + v, nil
}

// todo is every picture this model has no answer to the current question for.
func (c *classifier) todo(m *modelConf, shas []string) ([]item, error) {
	have := map[string]bool{}
	rows, err := c.st.DB.Query("SELECT image_sha, context_sha FROM classify_answers WHERE question = ?", m.qhash)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s, cs string
		rows.Scan(&s, &cs)
		have[s+"\x00"+cs] = true
	}
	rows.Close()
	var out []item
	for _, s := range shas {
		it := item{sha: s, path: c.index.Path(s)}
		if m.Needs != nil {
			if it.context, err = c.contextFor(m.Needs, s); err != nil {
				return nil, err
			}
			it.ctxSHA = sha(it.context)
		}
		if !have[s+"\x00"+it.ctxSHA] {
			out = append(out, it)
		}
	}
	return out, nil
}

// record keeps an answer and writes what it says into the store.
func (c *classifier) record(m *modelConf, q map[string]any, it item, status int, raw []byte, failure string) error {
	im, err := images.DescribeKnown(it.path, it.sha)
	if err != nil {
		im = store.Image{SHA256: it.sha, Path: it.path}
	}
	fingerprint := ""
	var head struct {
		SystemFingerprint *string `json:"system_fingerprint"`
	}
	if json.Unmarshal(raw, &head) == nil && head.SystemFingerprint != nil {
		fingerprint = *head.SystemFingerprint
	}
	var cont model.Contribution
	switch {
	case failure != "":
		cont = model.Contribution{Image: it.path, Model: m.name, Error: failure}
	case status != 200:
		cont = model.Contribution{Image: it.path, Model: m.name, Error: fmt.Sprintf("%d: %s", status, clip(string(raw), 300))}
	default:
		cont = parseAnswer(m.Kind, m.name, it, string(raw))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !cont.Success || cont.Error != "" {
		c.failed[m.name]++
	}
	c.asked[m.name]++
	if cont.Success && cont.Error == "" {
		if _, err := c.st.DB.Exec(`INSERT INTO classify_answers(image_sha, question, context_sha, context, fingerprint, raw, at)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO UPDATE SET context = excluded.context,
			fingerprint = excluded.fingerprint, raw = excluded.raw, at = excluded.at`,
			it.sha, m.qhash, it.ctxSHA, nullIfEmpty(it.context), nullIfEmpty(fingerprint), string(raw),
			time.Now().UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	_, err = c.st.Write(im, provenance(m.name, q, m.backend.URL, fingerprint), kindOf(q), cont)
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func kindOf(q map[string]any) string { k, _ := q["kind"].(string); return k }

// provenance: the question is the params, so a changed prompt is a new
// provenance and its rows stand beside the old ones; the model path is the
// backend's fingerprint (on gpu-timeshare with the fleet, the weights'
// store path).
func provenance(name string, q map[string]any, backendURL, fingerprint string) store.Provenance {
	upstream, _ := q["model"].(string)
	path := fingerprint
	if path == "" {
		path = upstream
	}
	return store.Provenance{Model: name, Mode: kindOf(q), ModelPath: path,
		RunnerPath: "slopbooru classify", Engine: backendURL, Params: q}
}

// parseAnswer reads a chat completion as the contribution its kind means.
func parseAnswer(kind, name string, it item, raw string) model.Contribution {
	content, logprobs, err := model.ChatAnswer(raw)
	if err != nil {
		return model.Contribution{Image: it.path, Model: name, Error: "the reply is not JSON: " + err.Error()}
	}
	if kind == "tagger" {
		c, ok := model.ParseTagger(it.path, content)
		if !ok {
			return model.Contribution{Image: it.path, Model: name, Error: "the tagger's answer is not its JSON"}
		}
		c.Model = name
		return c
	}
	return model.Parse(kind, name, it.path, content, logprobs, it.context)
}

func (c *classifier) progress(force bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && time.Since(c.logged) < 30*time.Second {
		return
	}
	c.logged = time.Now()
	var parts []string
	for _, n := range sortedKeys(c.asked) {
		s := fmt.Sprintf("%s %d", n, c.asked[n])
		if c.failed[n] > 0 {
			s += fmt.Sprintf(" (%d failed)", c.failed[n])
		}
		parts = append(parts, s)
	}
	if len(parts) > 0 {
		logf("answers so far: %s", strings.Join(parts, ", "))
	}
}

func sortedKeys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- requests mode ---------------------------------------------------------

func (c *classifier) ask(ctx context.Context, m *modelConf, items []item) error {
	b := m.backend
	work := make(chan item)
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for range b.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range work {
				body, err := c.body(m, it)
				if err != nil {
					c.record(m, m.question, it, 0, nil, "the picture: "+err.Error())
					continue
				}
				data, _ := json.Marshal(body)
				raw, err := b.call(ctx, "POST", "/chat/completions", "application/json", bytes.NewReader(data), b.Timeout.Duration)
				var ae *apiError
				switch {
				case errors.As(err, &ae):
					err = c.record(m, m.question, it, ae.Status, raw, "")
				case err != nil:
					if ctx.Err() != nil {
						return
					}
					err = c.record(m, m.question, it, 0, nil, err.Error())
				default:
					err = c.record(m, m.question, it, 200, raw, "")
				}
				if err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
				}
				c.progress(false)
			}
		}()
	}
	for _, it := range items {
		select {
		case work <- it:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(work)
	wg.Wait()
	return firstErr
}

// ---- batch mode ------------------------------------------------------------

// submit writes the items' requests as batch input files of at most
// batch_bytes, uploads each and starts a batch on it.
func (c *classifier) submit(ctx context.Context, m *modelConf, items []item) error {
	b := m.backend
	tmp, err := os.MkdirTemp("", "slopbooru-batch-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	var f *os.File
	var w *bufio.Writer
	var size int64
	var chunk []item
	n := 0
	flush := func() error {
		if f == nil {
			return nil
		}
		if err := w.Flush(); err != nil {
			return err
		}
		f.Close()
		err := c.startBatch(ctx, m, f.Name(), chunk)
		os.Remove(f.Name())
		f, chunk, size = nil, nil, 0
		return err
	}
	for _, it := range items {
		body, err := c.body(m, it)
		if err != nil {
			if err := c.record(m, m.question, it, 0, nil, "the picture: "+err.Error()); err != nil {
				return err
			}
			continue
		}
		line, _ := json.Marshal(map[string]any{"custom_id": it.customID(), "method": "POST",
			"url": "/v1/chat/completions", "body": body})
		line = append(line, '\n')
		if int64(len(line)) > int64(b.BatchBytes) {
			c.record(m, m.question, it, 0, nil, fmt.Sprintf("the request is %d bytes, over the backend's batch_bytes", len(line)))
			continue
		}
		if f != nil && size+int64(len(line)) > int64(b.BatchBytes) {
			if err := flush(); err != nil {
				return err
			}
		}
		if f == nil {
			n++
			if f, err = os.Create(filepath.Join(tmp, fmt.Sprintf("%s-%d.jsonl", m.name, n))); err != nil {
				return err
			}
			w = bufio.NewWriterSize(f, 1<<20)
		}
		w.Write(line)
		size += int64(len(line))
		chunk = append(chunk, it)
	}
	return flush()
}

func (c *classifier) startBatch(ctx context.Context, m *modelConf, path string, items []item) error {
	b := m.backend
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			if err := mw.WriteField("purpose", "batch"); err != nil {
				return err
			}
			part, err := mw.CreateFormFile("file", filepath.Base(path))
			if err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(part, f); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()
	raw, err := b.call(ctx, "POST", "/files", mw.FormDataContentType(), pr, 0)
	if err != nil {
		return fmt.Errorf("%s: uploading a batch file: %w", m.name, err)
	}
	var file struct {
		ID string `json:"id"`
	}
	json.Unmarshal(raw, &file)
	req, _ := json.Marshal(map[string]any{"input_file_id": file.ID, "endpoint": "/v1/chat/completions",
		"completion_window": b.Window, "metadata": map[string]string{"slopbooru_model": m.name}})
	raw, err = b.call(ctx, "POST", "/batches", "application/json", bytes.NewReader(req), 5*time.Minute)
	if err != nil {
		return fmt.Errorf("%s: starting a batch: %w", m.name, err)
	}
	var batch struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	json.Unmarshal(raw, &batch)
	if batch.ID == "" {
		return fmt.Errorf("%s: the backend made no batch: %s", m.name, clip(string(raw), 300))
	}
	tx, err := c.st.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO classify_batches(id, backend, model, question, created) VALUES (?, ?, ?, ?, ?)",
		batch.ID, b.URL, m.name, m.qhash, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	for _, it := range items {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO classify_pending(backend, batch_id, custom_id, image_sha, context_sha, context)
			VALUES (?, ?, ?, ?, ?, ?)`, b.URL, batch.ID, it.customID(), it.sha, it.ctxSHA, nullIfEmpty(it.context)); err != nil {
			return err
		}
	}
	logf("%s: batch %s, %d picture(s) (%s)", m.name, batch.ID, len(items), batch.Status)
	return tx.Commit()
}

type openBatch struct {
	id, model, question string
	backend             *backendConf
}

// await polls every batch this db has outstanding until each has ended,
// reading each one's results as it does.
func (c *classifier) await(ctx context.Context) error {
	byURL := map[string]*backendConf{}
	for _, b := range c.cfg.Backends {
		byURL[b.URL] = b
	}
	rows, err := c.st.DB.Query("SELECT id, backend, model, question FROM classify_batches ORDER BY created")
	if err != nil {
		return err
	}
	var open []openBatch
	for rows.Next() {
		var ob openBatch
		var url string
		rows.Scan(&ob.id, &url, &ob.model, &ob.question)
		if ob.backend = byURL[url]; ob.backend == nil {
			logf("batch %s on %s: that backend is no longer configured; leaving it", ob.id, url)
			continue
		}
		open = append(open, ob)
	}
	rows.Close()
	last := map[string]string{}
	for len(open) > 0 {
		var still []openBatch
		for _, ob := range open {
			raw, err := ob.backend.call(ctx, "GET", "/batches/"+ob.id, "", nil, 2*time.Minute)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				var ae *apiError
				if errors.As(err, &ae) && ae.Status == 404 {
					logf("batch %s: the backend doesn't know it any more; dropping it", ob.id)
					c.forgetBatch(ob)
					continue
				}
				logf("batch %s: %v (asking again later)", ob.id, err)
				still = append(still, ob)
				continue
			}
			var st struct {
				Status        string `json:"status"`
				OutputFileID  string `json:"output_file_id"`
				ErrorFileID   string `json:"error_file_id"`
				RequestCounts struct {
					Total, Completed, Failed int
				} `json:"request_counts"`
			}
			json.Unmarshal(raw, &st)
			line := fmt.Sprintf("%s: batch %s %s, %d/%d done", ob.model, ob.id, st.Status,
				st.RequestCounts.Completed+st.RequestCounts.Failed, st.RequestCounts.Total)
			if st.RequestCounts.Failed > 0 {
				line += fmt.Sprintf(" (%d failed)", st.RequestCounts.Failed)
			}
			if line != last[ob.id] {
				logf("%s", line)
				last[ob.id] = line
			}
			switch st.Status {
			case "completed", "expired", "cancelled", "failed":
				if err := c.ingest(ctx, ob, st.OutputFileID, st.ErrorFileID); err != nil {
					return err
				}
				c.progress(true)
			default:
				still = append(still, ob)
			}
		}
		open = still
		if len(open) == 0 {
			break
		}
		poll := open[0].backend.Poll.Duration
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
	return nil
}

func (c *classifier) forgetBatch(ob openBatch) {
	c.st.DB.Exec("DELETE FROM classify_pending WHERE backend = ? AND batch_id = ?", ob.backend.URL, ob.id)
	c.st.DB.Exec("DELETE FROM classify_batches WHERE backend = ? AND id = ?", ob.backend.URL, ob.id)
}

// ingest reads a finished batch's output and error files into the store.
func (c *classifier) ingest(ctx context.Context, ob openBatch, outID, errID string) error {
	m := c.cfg.Models[ob.model]
	var q map[string]any
	var qjson string
	if err := c.st.DB.QueryRow("SELECT json FROM classify_questions WHERE question = ?", ob.question).Scan(&qjson); err == nil {
		q, _ = jsonRoundTrip(json.RawMessage(qjson)).(map[string]any)
	}
	if q == nil {
		return fmt.Errorf("batch %s: its question %s is not in the db", ob.id, ob.question)
	}
	// the model as it was asked (the config may have changed since)
	asked := &modelConf{name: ob.model, Kind: kindOf(q), backend: ob.backend, qhash: ob.question, question: q}
	if m != nil && m.qhash == ob.question {
		asked = m
	}
	pending := map[string]item{}
	rows, err := c.st.DB.Query("SELECT custom_id, image_sha, context_sha, COALESCE(context, '') FROM classify_pending WHERE backend = ? AND batch_id = ?",
		ob.backend.URL, ob.id)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var it item
		rows.Scan(&id, &it.sha, &it.ctxSHA, &it.context)
		it.path = c.index.Path(it.sha)
		pending[id] = it
	}
	rows.Close()
	for _, fid := range []string{outID, errID} {
		if fid == "" {
			continue
		}
		raw, err := ob.backend.call(ctx, "GET", "/files/"+fid+"/content", "", nil, 30*time.Minute)
		if err != nil {
			return fmt.Errorf("batch %s: reading %s: %w", ob.id, fid, err)
		}
		sc := bufio.NewScanner(bytes.NewReader(raw))
		sc.Buffer(make([]byte, 1<<20), 1<<30)
		for sc.Scan() {
			var l struct {
				CustomID string `json:"custom_id"`
				Response *struct {
					StatusCode int             `json:"status_code"`
					Body       json.RawMessage `json:"body"`
				} `json:"response"`
				Error *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(sc.Bytes(), &l) != nil {
				continue
			}
			it, ok := pending[l.CustomID]
			if !ok {
				continue
			}
			delete(pending, l.CustomID)
			switch {
			case l.Response != nil:
				err = c.record(asked, q, it, l.Response.StatusCode, l.Response.Body, "")
			case l.Error != nil:
				err = c.record(asked, q, it, 0, nil, l.Error.Code+": "+l.Error.Message)
			default:
				err = c.record(asked, q, it, 0, nil, "no response")
			}
			if err != nil {
				return err
			}
		}
	}
	if len(pending) > 0 {
		logf("%s: batch %s left %d picture(s) unanswered; the next run asks again", ob.model, ob.id, len(pending))
	}
	c.forgetBatch(ob)
	return nil
}

// ---- reparse ---------------------------------------------------------------

// reparse rebuilds the store's rows from the kept answers.
func (c *classifier) reparse() error {
	rows, err := c.st.DB.Query(`SELECT a.image_sha, a.question, q.model, q.json, a.context_sha, COALESCE(a.context, ''), a.raw
		FROM classify_answers a JOIN classify_questions q USING (question) ORDER BY a.at`)
	if err != nil {
		return err
	}
	type ans struct {
		it          item
		qhash, name string
		q           map[string]any
		raw         string
	}
	var all []ans
	for rows.Next() {
		var a ans
		var qj string
		rows.Scan(&a.it.sha, &a.qhash, &a.name, &qj, &a.it.ctxSHA, &a.it.context, &a.raw)
		a.q, _ = jsonRoundTrip(json.RawMessage(qj)).(map[string]any)
		all = append(all, a)
	}
	rows.Close()
	n := 0
	for _, a := range all {
		a.it.path = c.index.Path(a.it.sha)
		backendURL, _ := a.q["backend"].(string)
		m := &modelConf{name: a.name, Kind: kindOf(a.q), qhash: a.qhash, question: a.q,
			backend: &backendConf{URL: backendURL}}
		if err := c.record(m, a.q, a.it, 200, []byte(a.raw), ""); err != nil {
			return err
		}
		n++
	}
	logf("reparse: %d answer(s) written into the store again", n)
	return nil
}

// ---- the command -----------------------------------------------------------

func classifyRun(args []string) error {
	fs := flag.NewFlagSet(brand+" classify", flag.ExitOnError)
	imagesDir := fs.String("images", "", "the pictures (searched recursively, found by content) (required)")
	db := fs.String("db", "", "the db to write (default: DIR/results.db)")
	config := fs.String("config", "", "a classify.toml read on top of the built-in one")
	only := fs.String("models", "", "comma-separated: ask only these models (default: every configured one)")
	jobs := fs.Int("hash-jobs", min(4, runtime.NumCPU()), "files hashed in parallel")
	dry := fs.Bool("dry-run", false, "say what would be asked, ask nothing")
	reparse := fs.Bool("reparse", false, "rebuild the db's rows from the kept answers, ask nothing")
	showConfig := fs.Bool("print-config", false, "print the built-in classify.toml and exit")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "%s classify: ask models about pictures, into the db\n\n"+
			"usage: %s classify --images DIR [--db results.db] [--config classify.toml] [--models a,b]\n\n",
			brand, filepath.Base(os.Args[0]))
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if *showConfig {
		fmt.Print(defaultClassify)
		return nil
	}
	if *imagesDir == "" || fs.NArg() > 0 {
		fs.Usage()
		return errUsage
	}
	if st, err := os.Stat(*imagesDir); err != nil || !st.IsDir() {
		return fmt.Errorf("no such directory: %s", *imagesDir)
	}
	if *db == "" {
		*db = filepath.Join(*imagesDir, "results.db")
	}
	cfg, err := readClassify(*config)
	if err != nil {
		return err
	}
	if *only != "" {
		want := map[string]bool{}
		for _, n := range strings.Split(*only, ",") {
			n = strings.TrimSpace(n)
			if cfg.Models[n] == nil {
				return fmt.Errorf("--models: no model %q (configured: %s)", n, strings.Join(sortedKeys(cfg.Models), ", "))
			}
			want[n] = true
		}
		for n := range cfg.Models {
			if !want[n] {
				delete(cfg.Models, n)
			}
		}
	}
	for _, b := range cfg.Backends {
		if err := b.setup(); err != nil {
			return err
		}
	}

	dbPath, _ := filepath.Abs(*db)
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("cannot open %s: %v", *db, err)
	}
	defer st.Close()
	if _, err := st.DB.Exec(classifySchema); err != nil {
		return err
	}
	thumbs := dbPath + ".thumbs"
	start := time.Now()
	skip := []string{dbPath, dbPath + "-wal", dbPath + "-shm", dbPath + "-journal", thumbs}
	ix, err := BuildIndex(*imagesDir, filepath.Join(thumbs, "files.db"), skip, *jobs)
	if err != nil {
		return fmt.Errorf("cannot index %s: %v", *imagesDir, err)
	}
	var shas []string
	for _, s := range ix.Hashes() {
		if d, err := images.DescribeKnown(ix.Path(s), s); err == nil && d.Width > 0 {
			shas = append(shas, s)
		}
	}
	logf("%d picture(s) in %s (%d file(s), %d hashed, in %.1fs)", len(shas), *imagesDir, ix.Files, ix.Hashed,
		time.Since(start).Seconds())

	c := &classifier{st: st, cfg: cfg, index: ix, standin: filepath.Join(thumbs, "standins"),
		asked: map[string]int{}, failed: map[string]int{}}
	if *reparse {
		if err := c.reparse(); err != nil {
			return err
		}
		return nil
	}
	for _, m := range cfg.Models {
		if _, err := st.DB.Exec("INSERT OR IGNORE INTO classify_questions(question, model, json) VALUES (?, ?, ?)",
			m.qhash, m.name, canonical(m.question)); err != nil {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if !*dry {
		var n int
		st.DB.QueryRow("SELECT COUNT(*) FROM classify_batches").Scan(&n)
		if n > 0 {
			logf("%d batch(es) from the last run first", n)
			if err := c.await(ctx); err != nil {
				return err
			}
		}
	}

	rounds := map[int][]*modelConf{}
	maxRound := 0
	for _, m := range cfg.Models {
		rounds[m.round] = append(rounds[m.round], m)
		maxRound = max(maxRound, m.round)
	}
	for r := 0; r <= maxRound; r++ {
		ms := rounds[r]
		sort.Slice(ms, func(i, j int) bool { return ms[i].name < ms[j].name })
		var batched bool
		var wg sync.WaitGroup
		errs := make(chan error, len(ms))
		for _, m := range ms {
			items, err := c.todo(m, shas)
			if err != nil {
				return err
			}
			logf("%s: %d of %d picture(s) to ask (%s, %s)", m.name, len(items), len(shas), m.backend.name, m.backend.Mode)
			if *dry || len(items) == 0 {
				continue
			}
			if m.backend.Mode == "batch" {
				// one at a time: building a batch reads every picture
				if err := c.submit(ctx, m, items); err != nil {
					return err
				}
				batched = true
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- c.ask(ctx, m, items)
			}()
		}
		if batched {
			if err := c.await(ctx); err != nil {
				return err
			}
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				return err
			}
		}
		if ctx.Err() != nil {
			return fmt.Errorf("interrupted; the next run resumes")
		}
	}
	c.progress(true)
	logf("done in %s", time.Since(start).Round(time.Second))
	return nil
}

var errUsage = errors.New("usage")

func classifyMain(args []string) {
	if err := classifyRun(args); errors.Is(err, errUsage) {
		os.Exit(2)
	} else if err != nil {
		die("%v", err)
	}
}
