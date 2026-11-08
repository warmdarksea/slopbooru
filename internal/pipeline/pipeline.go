// Package pipeline is what a job is: one image on one model.
//
// Intake is the whole cache rule: compute the key; a record under it means
// done; otherwise make sure a jobs row exists. Execute runs one job and
// leaves a record behind, so running intake again is a hit. The jobs
// table only ever holds outstanding work: a crash loses nothing, the
// answer simply isn't there yet.
package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"imgrec/internal/grounding"
	"imgrec/internal/images"
	"imgrec/internal/imagestore"
	"imgrec/internal/model"
	"imgrec/internal/pyjson"
	"imgrec/internal/records"
	"imgrec/internal/spec"
	"imgrec/internal/store"
)

const jobsSchema = `
CREATE TABLE IF NOT EXISTS jobs (
  model      TEXT    NOT NULL,
  image_sha  TEXT    NOT NULL,
  client     TEXT    NOT NULL,
  created    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  attempts   INTEGER NOT NULL DEFAULT 0,
  last_error TEXT,
  PRIMARY KEY (model, image_sha, client)
);`

type Pipeline struct {
	Specs       map[string]*spec.Spec
	Store       *store.Store
	Records     records.Backend
	Images      *imagestore.Store
	MaxAttempts int
	Engine      func(model string) string // what ran it, for the record

	mu sync.Mutex // serializes the store and jobs writes
}

// Schema creates the jobs table: the one definition, for every command
// that opens the db (a different one made first would be kept forever by
// CREATE TABLE IF NOT EXISTS).
func Schema(db *sql.DB) error {
	_, err := db.Exec(jobsSchema)
	return err
}

func New(specs map[string]*spec.Spec, st *store.Store, rec records.Backend, im *imagestore.Store) (*Pipeline, error) {
	if err := Schema(st.DB); err != nil {
		return nil, err
	}
	return &Pipeline{Specs: specs, Store: st, Records: rec, Images: im, MaxAttempts: 3,
		Engine: func(string) string { return "" }}, nil
}

type Status int

const (
	Queued Status = iota
	Done
	Failed
)

func (s Status) String() string { return [...]string{"queued", "done", "failed"}[s] }

func maxPixels(s *spec.Spec) int {
	n, ok := s.Input["max_pixels"].(json.Number)
	if !ok {
		return 0
	}
	v, _ := n.Int64()
	return int(v)
}

// input is what the model is shown for a picture, and its sha.
func (p *Pipeline) input(s *spec.Spec, sha string) (string, string, error) {
	if s.Kind == "tagger" {
		path := p.Images.Path(sha)
		if path == "" {
			return "", "", fmt.Errorf("no picture %s", sha)
		}
		return path, sha, nil
	}
	return p.Images.Input(sha, maxPixels(s))
}

// key is the cache key for a job; ok is false while a dependency it
// reads has no answer yet.
func (p *Pipeline) key(s *spec.Spec, sha string) (key, inputSHA, ctxText string, ok bool, err error) {
	_, inputSHA, err = p.input(s, sha)
	if err != nil {
		return "", "", "", false, err
	}
	if s.Needs != nil {
		ctxText, ok, err = p.context(s.Needs, sha)
		if err != nil || !ok {
			return "", inputSHA, "", ok, err
		}
	}
	return s.Key(inputSHA, ctxText), inputSHA, ctxText, true, nil
}

// context is the text a guard reads: what the needed model said about the
// picture. ok is false until that model has answered (or given up for good,
// in which case the guard reads nothing, as the python pipeline did).
func (p *Pipeline) context(n *spec.Needs, sha string) (string, bool, error) {
	dep, exists := p.Specs[n.Model]
	if !exists {
		return "", true, nil // a describer that isn't configured says nothing
	}
	key, _, _, ok, err := p.key(dep, sha)
	if err != nil || !ok {
		return "", ok, err
	}
	rec, err := p.Records.Get(key)
	if err != nil {
		return "", false, err
	}
	if rec == nil {
		if failed, err := p.failedForGood(n.Model, sha); err != nil || !failed {
			return "", false, err
		}
		return "", true, nil
	}
	c, ok := p.parse(dep, rec)
	if !ok {
		return "", true, nil
	}
	v, present := c.Fields[n.Field]
	if !present || v == nil {
		return "", true, nil
	}
	text := grounding.PyStrip(pyjson.Str(v))
	if text == "" {
		return "", true, nil
	}
	return contextLine(n.Field, text), true, nil
}

var labels = map[string]string{"description": "Description", "text": "Text in the image",
	"tag": "Tags", "subject": "Characters"}

// contextLine is infer.guard_context for one value: the label, then the
// text with its whitespace collapsed, clipped to 700 characters.
func contextLine(field, text string) string {
	text = strings.Join(strings.FieldsFunc(text, grounding.IsPySpace), " ")
	if utf8.RuneCountInString(text) > 700 {
		text = string([]rune(text)[:699]) + "…"
	}
	label := labels[field]
	if label == "" {
		label = field
	}
	return label + ": " + text
}

func (p *Pipeline) failedForGood(model, sha string) (bool, error) {
	var n int
	err := p.Store.DB.QueryRow("SELECT COUNT(*) FROM jobs WHERE model = ? AND image_sha = ? AND attempts >= ?",
		model, sha, p.MaxAttempts).Scan(&n)
	return n > 0, err
}

// parse reads a record as the contribution it means.
func (p *Pipeline) parse(s *spec.Spec, r *records.Record) (model.Contribution, bool) {
	path := p.Images.Path(r.ImageSHA)
	if s.Kind == "tagger" {
		return model.ParseTagger(path, r.Raw)
	}
	content, logprobs, err := model.ChatAnswer(r.Raw)
	if err != nil {
		return model.Contribution{}, false
	}
	return model.Parse(s.Kind, s.Name, path, content, logprobs, r.Context), true
}

// Result is the answer to an intake.
type Result struct {
	Status Status
	Record *records.Record // when done
	Error  string          // when failed
}

// Intake is the cache rule for one job: done if answered, queued (with a
// jobs row for this client) if not, failed if it ran out of attempts.
func (p *Pipeline) Intake(model, sha, client string) (Result, error) {
	s, ok := p.Specs[model]
	if !ok {
		return Result{}, fmt.Errorf("no model %q", model)
	}
	if p.Images.Path(sha) == "" {
		return Result{}, fmt.Errorf("no picture %s", sha)
	}
	key, _, _, ready, err := p.key(s, sha)
	if err != nil {
		return Result{}, err
	}
	if ready {
		rec, err := p.Records.Get(key)
		if err != nil {
			return Result{}, err
		}
		if rec != nil {
			return Result{Status: Done, Record: rec}, nil
		}
	} else if s.Needs != nil {
		// it reads another model's answer: ask for that first
		if _, err := p.Intake(s.Needs.Model, sha, client); err != nil {
			return Result{}, err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.Store.DB.Exec("INSERT OR IGNORE INTO jobs(model, image_sha, client) VALUES (?, ?, ?)",
		model, sha, client); err != nil {
		return Result{}, err
	}
	var attempts int
	var lastErr sql.NullString
	if err := p.Store.DB.QueryRow("SELECT MAX(attempts), MAX(last_error) FROM jobs WHERE model = ? AND image_sha = ?",
		model, sha).Scan(&attempts, &lastErr); err != nil {
		return Result{}, err
	}
	if attempts >= p.MaxAttempts {
		return Result{Status: Failed, Error: lastErr.String}, nil
	}
	return Result{Status: Queued}, nil
}

// Job is one image on one model, with the clients waiting for it.
type Job struct {
	Model, SHA string
	Clients    []string
	Created    string
}

// Pending lists the jobs that can run now, per model, oldest first:
// attempts left, and every dependency answered.
func (p *Pipeline) Pending() (map[string][]Job, int, error) {
	rows, err := p.Store.DB.Query("SELECT model, image_sha, client, created FROM jobs WHERE attempts < ? "+
		"ORDER BY created, rowid", p.MaxAttempts)
	if err != nil {
		return nil, 0, err
	}
	byKey := map[string]*Job{}
	var order []*Job
	for rows.Next() {
		var m, sha, client, created string
		if err := rows.Scan(&m, &sha, &client, &created); err != nil {
			rows.Close()
			return nil, 0, err
		}
		k := m + "\x00" + sha
		j := byKey[k]
		if j == nil {
			j = &Job{Model: m, SHA: sha, Created: created}
			byKey[k] = j
			order = append(order, j)
		}
		j.Clients = append(j.Clients, client)
	}
	rows.Close()
	out, waiting := map[string][]Job{}, 0
	for _, j := range order {
		s := p.Specs[j.Model]
		if s == nil {
			continue
		}
		if s.Needs != nil {
			if _, ok, err := p.context(s.Needs, j.SHA); err != nil || !ok {
				waiting++
				continue
			}
		}
		out[j.Model] = append(out[j.Model], *j)
	}
	return out, waiting, nil
}

// Asker sends one request to a model's runner and returns the reply.
type Asker func(ctx context.Context, request []byte) ([]byte, error)

// Execute runs one job through a runner: build the input, ask, keep the
// record, write the store, and clear the job. A failure counts an attempt.
func (p *Pipeline) Execute(ctx context.Context, name, sha string, ask Asker) error {
	s := p.Specs[name]
	key, inputSHA, ctxText, ready, err := p.key(s, sha)
	if err == nil && !ready {
		err = errors.New("a dependency has no answer yet")
	}
	if err != nil {
		return p.fail(name, sha, err)
	}
	if rec, _ := p.Records.Get(key); rec != nil { // answered meanwhile
		return p.finish(s, sha, rec)
	}
	path, _, err := p.input(s, sha)
	if err != nil {
		return p.fail(name, sha, err)
	}
	var request []byte
	if s.Kind == "tagger" {
		request, _ = json.Marshal(map[string]any{"path": path})
	} else {
		request, _ = json.Marshal(s.RequestBody("file://"+path, ctxText, inputSHA))
	}
	start := time.Now()
	raw, err := ask(ctx, request)
	if err != nil {
		return p.fail(name, sha, err)
	}
	rec := records.Record{Key: key, Model: name, SpecHash: s.Hash(), ImageSHA: sha, InputSHA: inputSHA,
		Context: ctxText, Raw: string(raw), Elapsed: time.Since(start).Seconds(),
		Engine: p.Engine(name), At: time.Now().UTC().Format(time.RFC3339Nano)}
	if s.Kind != "tagger" {
		if _, _, err := model.ChatAnswer(rec.Raw); err != nil {
			return p.fail(name, sha, fmt.Errorf("reply was not json: %w", err))
		}
	} else if _, ok := model.ParseTagger(path, rec.Raw); !ok {
		return p.fail(name, sha, errors.New("reply was not json"))
	} else if c, _ := model.ParseTagger(path, rec.Raw); !c.Success {
		return p.fail(name, sha, errors.New(c.Error)) // a tagger failure is retried, not kept
	}
	if err := p.Records.Put(rec); err != nil {
		return err
	}
	return p.finish(s, sha, &rec)
}

// finish writes a record's rows into the store and clears its job rows.
func (p *Pipeline) finish(s *spec.Spec, sha string, rec *records.Record) error {
	c, ok := p.parse(s, rec)
	if !ok {
		return fmt.Errorf("record %s does not parse", rec.Key)
	}
	im, err := imageFor(p.Images, sha)
	if err != nil {
		return err
	}
	prov := store.Provenance{Model: s.Name, Mode: s.Kind, ModelPath: s.Weights, RunnerPath: "imgrec",
		Engine: rec.Engine, Closure: s.Closure, Params: s.Question()}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.Store.Write(im, prov, s.Kind, c); err != nil {
		return err
	}
	_, err = p.Store.DB.Exec("DELETE FROM jobs WHERE model = ? AND image_sha = ?", s.Name, sha)
	return err
}

func (p *Pipeline) fail(model, sha string, why error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.Store.DB.Exec("UPDATE jobs SET attempts = attempts + 1, last_error = ? "+
		"WHERE model = ? AND image_sha = ?", why.Error(), model, sha)
	if err != nil {
		return err
	}
	return why
}

func imageFor(im *imagestore.Store, sha string) (store.Image, error) {
	path := im.Path(sha)
	if path == "" {
		return store.Image{}, fmt.Errorf("no picture %s", sha)
	}
	return images.Describe(path)
}

// Materialize writes an existing record's rows into the store (idempotent):
// the store is derived from records, so a record that arrived another way
// -- merged from another machine's jsonl -- shows up in the booru too.
func (p *Pipeline) Materialize(model string, sha string, rec *records.Record) error {
	s, ok := p.Specs[model]
	if !ok {
		return fmt.Errorf("no model %q", model)
	}
	return p.finish(s, sha, rec)
}

// Status is a job's state without creating anything: exists is false when
// nobody has asked for it and it has no answer.
func (p *Pipeline) Status(name, sha string) (Result, bool, error) {
	s, ok := p.Specs[name]
	if !ok {
		return Result{}, false, fmt.Errorf("no model %q", name)
	}
	key, _, _, ready, err := p.key(s, sha)
	if err != nil {
		return Result{}, false, err
	}
	if ready {
		rec, err := p.Records.Get(key)
		if err != nil {
			return Result{}, false, err
		}
		if rec != nil {
			return Result{Status: Done, Record: rec}, true, nil
		}
	}
	var n, attempts int
	var lastErr sql.NullString
	if err := p.Store.DB.QueryRow("SELECT COUNT(*), COALESCE(MAX(attempts), 0), MAX(last_error) FROM jobs "+
		"WHERE model = ? AND image_sha = ?", name, sha).Scan(&n, &attempts, &lastErr); err != nil {
		return Result{}, false, err
	}
	if n == 0 {
		return Result{}, false, nil
	}
	if attempts >= p.MaxAttempts {
		return Result{Status: Failed, Error: lastErr.String}, true, nil
	}
	return Result{Status: Queued}, true, nil
}

// Cancel withdraws one client's interest in a job; it still runs if
// another client wants it.
func (p *Pipeline) Cancel(name, sha, client string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.Store.DB.Exec("DELETE FROM jobs WHERE model = ? AND image_sha = ? AND client = ?", name, sha, client)
	return err
}

// Contribution is what a record means: the parsed answer.
func (p *Pipeline) Contribution(name string, rec *records.Record) (model.Contribution, bool) {
	s, ok := p.Specs[name]
	if !ok {
		return model.Contribution{}, false
	}
	return p.parse(s, rec)
}
