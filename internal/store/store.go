// Package store writes the recognition store (schema.sql): content-
// addressed images, provenances with their nix closures, and one
// annotation row per value a model said. The booru reads it.
//
// A port of src/py-pipeline/store.py. schema.sql and schema_fts.sql are
// copies of src/py-pipeline's (go:embed can't reach outside the module);
// change them together.
package store

import (
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"

	"imgrec/internal/grounding"
	"imgrec/internal/model"
	"imgrec/internal/pyjson"
)

//go:embed schema.sql
var schema string

//go:embed schema_fts.sql
var schemaFTS string

type Store struct {
	DB    *sql.DB
	FTS   bool
	provs map[string]int64 // provenance identity -> id
}

// Open creates or opens a store, applying the schema (idempotent).
func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(abs)+
		"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(30000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; sqlite serializes writes anyway
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master").Scan(&n); err != nil {
		db.Close()
		return nil, err
	}
	first := n == 0
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{DB: db, provs: map[string]int64{}}
	if _, err := db.Exec(schemaFTS); err == nil {
		s.FTS = true
		if first {
			if _, err := db.Exec("INSERT INTO annotations_fts(annotations_fts) VALUES('rebuild')"); err != nil {
				db.Close()
				return nil, err
			}
		}
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

// ---- provenance ------------------------------------------------------------

// Provenance is what produced a set of values.
type Provenance struct {
	Model, Mode, ModelPath, RunnerPath, Engine string
	Closure                                    []string // store paths
	Params                                     any      // the question, as json
}

// internClosure: an empty closure is interned like any other, never left
// NULL -- sqlite treats NULLs as distinct in the provenances UNIQUE.
func internClosure(tx *sql.Tx, paths []string) (int64, error) {
	uniq := map[string]bool{}
	var ps []string
	for _, p := range paths {
		if p != "" && !uniq[p] {
			uniq[p] = true
			ps = append(ps, p)
		}
	}
	sort.Strings(ps)
	sum := sha256.Sum256([]byte(strings.Join(ps, "\n")))
	digest := hex.EncodeToString(sum[:])
	var id int64
	err := tx.QueryRow("SELECT id FROM closures WHERE sha256 = ?", digest).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	r, err := tx.Exec("INSERT INTO closures(sha256, n_paths) VALUES (?, ?)", digest, len(ps))
	if err != nil {
		return 0, err
	}
	id, _ = r.LastInsertId()
	for _, p := range ps {
		if _, err := tx.Exec("INSERT OR IGNORE INTO store_paths(path) VALUES (?)", p); err != nil {
			return 0, err
		}
		if _, err := tx.Exec("INSERT OR IGNORE INTO closure_paths(closure_id, store_path_id) "+
			"SELECT ?, id FROM store_paths WHERE path = ?", id, p); err != nil {
			return 0, err
		}
	}
	return id, nil
}

func (s *Store) provenanceID(tx *sql.Tx, p Provenance) (int64, error) {
	params := pyjson.Dumps(p.Params, 0, true)
	if p.Params == nil {
		params = "{}"
	}
	ident := strings.Join([]string{p.Model, p.Mode, p.ModelPath, p.RunnerPath, p.Engine,
		strings.Join(p.Closure, "\x00"), params}, "\x01")
	if id, ok := s.provs[ident]; ok {
		return id, nil
	}
	cid, err := internClosure(tx, p.Closure)
	if err != nil {
		return 0, err
	}
	row := []any{p.Model, p.Mode, p.ModelPath, p.RunnerPath, p.Engine, cid, params}
	if _, err := tx.Exec("INSERT OR IGNORE INTO provenances"+
		"(model, mode, model_path, runner_path, engine, closure_id, params) VALUES (?, ?, ?, ?, ?, ?, ?)",
		row...); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRow("SELECT id FROM provenances WHERE model=? AND mode=? AND model_path=? "+
		"AND runner_path=? AND engine=? AND closure_id IS ? AND params=?", row...).Scan(&id); err != nil {
		return 0, err
	}
	s.provs[ident] = id
	return id, nil
}

// ---- images ----------------------------------------------------------------

// Image is one picture's identity and metadata.
type Image struct {
	SHA256        string
	Path          string // absolute
	Bytes         int64
	Mime          string // "" if unknown
	Width, Height int    // 0 if unknown
}

func nullable[T comparable](v T) any {
	var zero T
	if v == zero {
		return nil
	}
	return v
}

func (s *Store) imageID(tx *sql.Tx, im Image) (int64, error) {
	var id int64
	err := tx.QueryRow("SELECT id FROM images WHERE sha256 = ?", im.SHA256).Scan(&id)
	if err == sql.ErrNoRows && im.Path == "" && im.Bytes == 0 {
		return 0, fmt.Errorf("picture %s: not in the store and not on disk", im.SHA256)
	}
	if err == sql.ErrNoRows {
		r, err := tx.Exec("INSERT INTO images(sha256, bytes, mime, width, height) VALUES (?, ?, ?, ?, ?)",
			im.SHA256, im.Bytes, nullable(im.Mime), nullable(im.Width), nullable(im.Height))
		if err != nil {
			return 0, err
		}
		id, _ = r.LastInsertId()
	} else if err != nil {
		return 0, err
	}
	if im.Path != "" {
		if _, err := tx.Exec("INSERT INTO image_paths(image_id, path) VALUES (?, ?) "+
			"ON CONFLICT(image_id, path) DO UPDATE SET last_seen = datetime('now')", id, im.Path); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// ---- writing one contribution ---------------------------------------------

// Write folds one contribution into the store, in one transaction.
// kind is the model's kind (fields, caption, ocr, guard, tagger).
func (s *Store) Write(im Image, prov Provenance, kind string, c model.Contribution) (int, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	iid, err := s.imageID(tx, im)
	if err != nil {
		return 0, err
	}
	pid, err := s.provenanceID(tx, prov)
	if err != nil {
		return 0, err
	}
	if c.Error != "" || !c.Success {
		why := c.Error
		if why == "" {
			why = "no result"
		}
		if _, err := tx.Exec("INSERT INTO failures(image_id, provenance_id, error) VALUES (?, ?, ?) "+
			"ON CONFLICT(image_id, provenance_id) DO UPDATE SET error = excluded.error, at = datetime('now')",
			iid, pid, why); err != nil {
			return 0, err
		}
	}
	rows := RowsFor(c.Fields, c.Detail, kind)
	for _, r := range rows {
		var det any
		if r.Detail != nil {
			det = pyjson.Dumps(r.Detail, 0, true)
		}
		if _, err := tx.Exec("INSERT INTO annotations(image_id, provenance_id, field, key, value, confidence, detail) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(image_id, provenance_id, field, key, value) "+
			"DO UPDATE SET confidence = excluded.confidence, detail = excluded.detail",
			iid, pid, r.Field, r.Key, r.Value, r.Confidence, det); err != nil {
			return 0, err
		}
	}
	return len(rows), tx.Commit()
}

// ---- flattening (store.py rows_for) ----------------------------------------

// fields whose value is a list of scalars -> one row each, under the
// singular name; fields whose value is a scalar -> one row.
var listFields = [][2]string{{"tags", "tag"}, {"source", "source"}, {"hazards", "hazard"}}
var scalarFields = []string{"description", "text", "rating", "verdict", "dimensionality"}

// Row is one annotation.
type Row struct {
	Field, Key, Value string
	Confidence        any // float64 or nil
	Detail            any // json-able or nil
}

// clamp is float(x) clamped to [0, 1], nil where python's float() raises.
func clamp(x any) any {
	var f float64
	switch v := x.(type) {
	case nil:
		return nil
	case bool:
		if v {
			f = 1
		}
	case float64:
		f = v
	case string:
		p, err := strconv.ParseFloat(grounding.PyStrip(v), 64)
		if err != nil {
			return nil
		}
		f = p
	default:
		n, ok := pyjson.Numeric(v)
		if !ok {
			return nil
		}
		f = n
	}
	if math.IsNaN(f) { // python: max(0.0, nan) is 0.0
		return 0.0
	}
	return math.Min(1, math.Max(0, f))
}

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func confidenceFor(field, value string, fields, detail map[string]any) any {
	if scores, ok := asMap(detail["scores"]); ok {
		if per, ok := asMap(scores[field]); ok {
			if v, ok := per[value]; ok {
				return clamp(v)
			}
		}
	}
	if legacy, ok := asMap(detail[field]); ok && field == "rating" {
		if v, ok := legacy[value]; ok {
			return clamp(v)
		}
	}
	if self, ok := asMap(fields["confidence"]); ok {
		if v, ok := self[field]; ok {
			return clamp(v)
		}
		for _, lf := range listFields {
			if lf[1] == field {
				if v, ok := self[lf[0]]; ok {
					return clamp(v)
				}
			}
		}
	}
	return nil
}

// iterate is python's `for v in (x or [])`: a list's items, a string's
// characters, a dict's keys; nothing for a falsy or unsized value.
func iterate(x any) []any {
	if !pyjson.Truthy(x) {
		return nil
	}
	switch v := x.(type) {
	case []any:
		return v
	case string:
		var out []any
		for _, r := range v {
			out = append(out, string(r))
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]any, len(keys))
		for i, k := range keys {
			out[i] = k
		}
		return out
	}
	return nil
}

// isScalar is isinstance(v, (str, int, float)) -- bool included.
func isScalar(v any) bool {
	switch v.(type) {
	case string, bool, float64:
		return true
	}
	_, isNum := pyjson.Numeric(v)
	return isNum
}

// RowsFor flattens a contribution's fields into rows.
func RowsFor(fields, detail map[string]any, kind string) []Row {
	if fields == nil {
		fields = map[string]any{}
	}
	if detail == nil {
		detail = map[string]any{}
	}
	var out []Row
	emit := func(field string, value any, key string, conf any, det any, _ bool) {
		if value == nil {
			return
		}
		v := grounding.PyStrip(pyjson.Str(value))
		if v == "" {
			return
		}
		if conf == nil { // python: emit(..., conf=None) looks it up
			conf = confidenceFor(field, v, fields, detail)
		}
		out = append(out, Row{field, key, v, conf, det})
	}

	for _, name := range scalarFields {
		value, present := fields[name]
		if !present {
			continue
		}
		var det any
		if name == "text" {
			// deepseek-ocr's grounding markup: the plain transcription as
			// the value, the regions in detail for the booru's boxes
			if str, ok := value.(string); ok && kind == "ocr" {
				if plain, boxes := grounding.Parse(str); boxes != nil {
					value = plain
					bs := make([]any, len(boxes))
					for i, b := range boxes {
						bs[i] = b.Value()
					}
					det = map[string]any{"boxes": bs}
				}
			}
			if det == nil && pyjson.Truthy(detail["boxes"]) {
				det = map[string]any{"boxes": detail["boxes"]}
			}
		}
		if name == "rating" {
			var scores any
			if sc, ok := asMap(detail["scores"]); ok {
				scores = sc["rating"]
			}
			if !pyjson.Truthy(scores) {
				scores = detail["rating"]
			}
			if m, ok := asMap(scores); ok {
				det = map[string]any{"scores": m}
			}
		}
		if name == "verdict" {
			d := map[string]any{}
			for _, k := range []string{"raw", "logprobs", "context"} {
				if pyjson.Truthy(detail[k]) {
					d[k] = detail[k]
				}
			}
			if len(d) > 0 {
				det = d
			}
		}
		emit(name, value, "", nil, det, false)
	}

	for _, lf := range listFields {
		for _, v := range iterate(fields[lf[0]]) {
			emit(lf[1], v, "", nil, nil, false)
		}
	}

	// subjects: {name, probability} -- the probability is the confidence
	for _, s := range iterate(fields["subjects"]) {
		if m, ok := asMap(s); ok {
			emit("subject", m["name"], "", clamp(m["probability"]), nil, true)
		} else {
			emit("subject", s, "", nil, nil, false)
		}
	}

	// emotion: overall, plus one row per subject keyed by the subject
	if emo, ok := asMap(fields["emotion"]); ok {
		emit("emotion", emo["overall"], "", nil, nil, false)
		for _, e := range iterate(emo["subjects"]) {
			if m, ok := asMap(e); ok {
				key := ""
				if pyjson.Truthy(m["subject"]) {
					key = pyjson.Str(m["subject"])
				}
				emit("emotion", m["emotion"], key, nil, nil, false)
			}
		}
	} else if e, present := fields["emotion"]; present && e != nil {
		emit("emotion", e, "", nil, nil, false)
	}

	// anything a future model invents: scalars and lists of scalars
	known := map[string]bool{"subjects": true, "emotion": true, "confidence": true}
	for _, n := range scalarFields {
		known[n] = true
	}
	for _, lf := range listFields {
		known[lf[0]] = true
	}
	names := make([]string, 0, len(fields))
	for n := range fields {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if known[name] {
			continue
		}
		switch val := fields[name].(type) {
		case []any:
			for _, v := range val {
				if isScalar(v) {
					emit(name, v, "", nil, nil, false)
				}
			}
		default:
			if isScalar(val) {
				emit(name, val, "", nil, nil, false)
			}
		}
	}
	return out
}
