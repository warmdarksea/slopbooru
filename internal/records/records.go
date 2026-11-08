// Package records keeps answers: one record per (question, input), the
// reply verbatim, keyed by spec.Key. Records are the pipeline's output;
// the store (annotations) is built from them.
//
// Two backends, one interface:
//   - sqlite: a `records` table, normally inside results.db
//   - jsonl: a directory of append-only files, one per writer (machine);
//     merging machines is copying their files into one directory --
//     keys are content hashes, so two writers can never disagree about one
package records

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"

	_ "modernc.org/sqlite"
)

// Record is one answer.
type Record struct {
	Key      string  `json:"key"`
	Model    string  `json:"model"`
	SpecHash string  `json:"spec_hash"`
	ImageSHA string  `json:"image_sha"` // the original picture
	InputSHA string  `json:"input_sha"` // what the model was shown (a stand-in, maybe)
	Context  string  `json:"context,omitempty"`
	Raw      string  `json:"raw"` // the reply, unmodified
	Elapsed  float64 `json:"elapsed"`
	Engine   string  `json:"engine"`
	At       string  `json:"at"` // RFC 3339
}

// Backend is where records live.
type Backend interface {
	Get(key string) (*Record, error) // nil, nil when absent
	Put(r Record) error              // idempotent: an existing key is kept
	Scan(fn func(Record) error) error
	Close() error
}

// ---- sqlite ----------------------------------------------------------------

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS records (
  key       TEXT PRIMARY KEY,
  model     TEXT NOT NULL,
  spec_hash TEXT NOT NULL,
  image_sha TEXT NOT NULL,
  input_sha TEXT NOT NULL,
  context   TEXT NOT NULL DEFAULT '',
  raw       TEXT NOT NULL,
  elapsed   REAL,
  engine    TEXT NOT NULL DEFAULT '',
  at        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS records_image ON records(image_sha, model);`

type Sqlite struct{ db *sql.DB }

// OpenSqlite uses an open db (results.db: the records sit beside the store).
func OpenSqlite(db *sql.DB) (*Sqlite, error) {
	if _, err := db.Exec(sqliteSchema); err != nil {
		return nil, err
	}
	return &Sqlite{db}, nil
}

func (s *Sqlite) Get(key string) (*Record, error) {
	var r Record
	var elapsed sql.NullFloat64
	err := s.db.QueryRow("SELECT key, model, spec_hash, image_sha, input_sha, context, raw, elapsed, engine, at "+
		"FROM records WHERE key = ?", key).Scan(&r.Key, &r.Model, &r.SpecHash, &r.ImageSHA, &r.InputSHA,
		&r.Context, &r.Raw, &elapsed, &r.Engine, &r.At)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	r.Elapsed = elapsed.Float64
	return &r, err
}

func (s *Sqlite) Put(r Record) error {
	_, err := s.db.Exec("INSERT OR IGNORE INTO records(key, model, spec_hash, image_sha, input_sha, context, raw, elapsed, engine, at) "+
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", r.Key, r.Model, r.SpecHash, r.ImageSHA, r.InputSHA,
		r.Context, r.Raw, r.Elapsed, r.Engine, r.At)
	return err
}

func (s *Sqlite) Scan(fn func(Record) error) error {
	rows, err := s.db.Query("SELECT key, model, spec_hash, image_sha, input_sha, context, raw, elapsed, engine, at " +
		"FROM records ORDER BY rowid")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r Record
		var elapsed sql.NullFloat64
		if err := rows.Scan(&r.Key, &r.Model, &r.SpecHash, &r.ImageSHA, &r.InputSHA,
			&r.Context, &r.Raw, &elapsed, &r.Engine, &r.At); err != nil {
			return err
		}
		r.Elapsed = elapsed.Float64
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Sqlite) Close() error { return nil } // the db belongs to the caller

// ---- jsonl -----------------------------------------------------------------

// Jsonl is a directory of *.jsonl files; this process appends to one of
// them (its own) and reads all of them. One writer per file.
type Jsonl struct {
	mu    sync.Mutex
	dir   string
	out   *os.File
	index map[string]Record
}

// OpenJsonl reads every *.jsonl in dir and appends to dir/<writer>.jsonl.
// A torn last line (a crash mid-write) is ignored, not fatal.
func OpenJsonl(dir, writer string) (*Jsonl, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	j := &Jsonl{dir: dir, index: map[string]Record{}}
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	for _, f := range files {
		if err := j.load(f); err != nil {
			return nil, err
		}
	}
	j.out, err = os.OpenFile(filepath.Join(dir, writer+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return j, nil
}

func (j *Jsonl) load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<30)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.Key == "" {
			continue
		}
		if _, seen := j.index[r.Key]; !seen {
			j.index[r.Key] = r
		}
	}
	return sc.Err()
}

func (j *Jsonl) Get(key string) (*Record, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if r, ok := j.index[key]; ok {
		return &r, nil
	}
	return nil, nil
}

func (j *Jsonl) Put(r Record) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, ok := j.index[r.Key]; ok {
		return nil
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	// one write per line, then sync: a record is durable before it counts
	if _, err := j.out.Write(append(line, '\n')); err != nil {
		return err
	}
	if err := j.out.Sync(); err != nil {
		return err
	}
	j.index[r.Key] = r
	return nil
}

func (j *Jsonl) Scan(fn func(Record) error) error {
	j.mu.Lock()
	keys := make([]string, 0, len(j.index))
	for k := range j.index {
		keys = append(keys, k)
	}
	j.mu.Unlock()
	sort.Strings(keys)
	for _, k := range keys {
		j.mu.Lock()
		r := j.index[k]
		j.mu.Unlock()
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

func (j *Jsonl) Close() error {
	if j.out == nil {
		return nil
	}
	return j.out.Close()
}
