package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	_ "modernc.org/sqlite" // pure go: no cgo, no libsqlite3 on the host
)

const perPage = 48

var fieldOrder = []string{
	"description", "text", "tag", "subject", "emotion", "rating",
	"verdict", "hazard", "dimensionality", "source",
}

// which filter words map onto a field lookup: `rating:safe`, `tag:solo`
var valueFilters = []string{ // sorted: the order the subqueries are built in
	"dimensionality", "emotion", "hazard", "rating", "source", "subject",
	"tag", "verdict",
}

var linkedFields = map[string]bool{
	"tag": true, "source": true, "subject": true, "rating": true,
	"dimensionality": true, "hazard": true, "verdict": true,
}

// how the values of one field are ordered on an image page. Always within
// the field (the page is grouped by it); the clicked column decides the
// rest. Fixed fragments, picked by key -- never built from the request.
var sorts = map[string]string{
	"conf":  "COALESCE(a.confidence, -1) DESC, a.value COLLATE NOCASE, p.model",
	"value": "a.value COLLATE NOCASE, COALESCE(a.confidence, -1) DESC, p.model",
	"model": "p.model, COALESCE(a.confidence, -1) DESC, a.value COLLATE NOCASE",
}

// ---- query parsing -------------------------------------------------------

// Python's \w and \S are unicode-aware where Go's are ascii; these classes
// are what the original regex meant.
const (
	pyW  = `[\p{L}\p{N}_]`
	pyWS = `\s\x0b\x1c-\x1f\x85\p{Z}`
)

// token forms: name:"two words", name:word, "a phrase", bare
var tokenRE = regexp.MustCompile(
	`(` + pyW + `+):"([^"]*)"|(` + pyW + `+):([^` + pyWS + `]+)|("[^"]*")|([^` + pyWS + `]+)`)

var ftsTokenRE = regexp.MustCompile(`"[^"]*"|[^` + pyWS + `]+`)

// str.split(): runs of anything but whitespace
var wordRE = regexp.MustCompile(`[^` + pyWS + `]+`)

func isFilter(name string) bool {
	if name == "model" || name == "field" {
		return true
	}
	for _, f := range valueFilters {
		if f == name {
			return true
		}
	}
	return false
}

// parseQuery: 'marisa rating:safe model:pixai' -> ('marisa', {'rating':
// ['safe'], ...}). Filter values may be quoted, so subject:"maid girl"
// survives the split; everything that is not a known filter stays free text.
func parseQuery(qs string) (string, map[string][]string) {
	var terms []string
	filters := map[string][]string{}
	for _, m := range tokenRE.FindAllStringSubmatchIndex(qs, -1) {
		g := func(i int) (string, bool) {
			if m[2*i] < 0 {
				return "", false
			}
			return qs[m[2*i]:m[2*i+1]], true
		}
		qname, isQ := g(1)
		qval, _ := g(2)
		name, isN := g(3)
		val, _ := g(4)
		phrase, isP := g(5)
		bare, _ := g(6)
		if isQ {
			name, val, isN = qname, qval, true
		}
		switch {
		case isN && isFilter(name):
			filters[name] = append(filters[name], val)
		case isN:
			terms = append(terms, name+":"+val)
		case isP:
			terms = append(terms, phrase)
		default:
			terms = append(terms, bare)
		}
	}
	return strings.Join(terms, " "), filters
}

// ftsExpr turns free text into an FTS5 MATCH expression: every word,
// prefix-matched. Quoted phrases stay phrases; everything else gets a
// trailing * so 'maris' finds marisa. Double quotes inside a token are
// stripped, so the expression can never break out of its own quoting.
func ftsExpr(text string) string {
	var parts []string
	for _, raw := range ftsTokenRE.FindAllString(text, -1) {
		if strings.HasPrefix(raw, `"`) && strings.HasSuffix(raw, `"`) && len([]rune(raw)) > 2 {
			parts = append(parts, `"`+strings.ReplaceAll(raw[1:len(raw)-1], `"`, "")+`"`)
		} else {
			word := strings.NewReplacer(`"`, "", "*", "").Replace(raw)
			if word != "" {
				parts = append(parts, `"`+word+`"*`)
			}
		}
	}
	return strings.Join(parts, " AND ")
}

// ---- the store -----------------------------------------------------------

type Row map[string]any

type Store struct {
	path     string
	db       *sql.DB
	readonly bool
	fts      bool
}

// sqliteURI is a file: uri for path, which has to survive a windows drive
// letter and backslashes as well as a '?' or '#' in a name.
func sqliteURI(path string, query string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // C:/x -> /C:/x, which sqlite's windows vfs expects
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '%' || c == '?' || c == '#' || c == ' ' || c < 0x20 {
			fmt.Fprintf(&b, "%%%02X", c)
		} else {
			b.WriteByte(c)
		}
	}
	return "file://" + b.String() + "?" + query
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, readonly: true}
	// read-only is the right default -- the browser never writes -- but
	// opening a WAL database that way needs a usable -shm, which is not
	// always there. probe once and remember rather than failing per request.
	if err := s.open("mode=ro"); err == nil {
		if _, err = s.queryOne("SELECT COUNT(*) FROM images"); err != nil {
			logf("read-only open failed (%v); opening read-write", err)
			s.db.Close()
			s.readonly = false
		}
	} else {
		s.readonly = false
	}
	if !s.readonly {
		if err := s.open("mode=rw"); err != nil {
			return nil, err
		}
		if _, err := s.queryOne("SELECT COUNT(*) FROM images"); err != nil {
			s.db.Close()
			return nil, err
		}
	}
	if _, err := s.queryOne("SELECT 1 FROM annotations_fts LIMIT 1"); err == nil {
		s.fts = true
	} else {
		logf("no FTS5 index; searching with LIKE")
	}
	return s, nil
}

func (s *Store) open(mode string) error {
	db, err := sql.Open("sqlite", sqliteURI(s.path, mode+"&_pragma=busy_timeout(5000)"))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(2 * runtime.NumCPU())
	db.SetMaxIdleConns(2 * runtime.NumCPU())
	s.db = db
	return db.Ping()
}

func (s *Store) query(q string, args ...any) ([]Row, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []Row
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := Row{}
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				vals[i] = string(b)
			}
			r[c] = vals[i]
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) queryRow(q string, args ...any) (Row, error) {
	rows, err := s.query(q, args...)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// queryOne is the first column of the first row.
func (s *Store) queryOne(q string, args ...any) (any, error) {
	var v any
	err := s.db.QueryRow(q, args...).Scan(&v)
	return v, err
}

func (s *Store) count(q string, args ...any) (int64, error) {
	v, err := s.queryOne(q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := v.(int64)
	return n, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func (s *Store) Search(qs string, pageNo int) ([]Row, bool, int64, error) {
	text, filters := parseQuery(qs)
	var where []string
	var args []any

	if text != "" {
		if s.fts {
			if expr := ftsExpr(text); expr != "" {
				where = append(where, "a.id IN (SELECT rowid FROM annotations_fts "+
					"WHERE annotations_fts MATCH ?)")
				args = append(args, expr)
			}
		} else {
			for _, word := range wordRE.FindAllString(text, -1) {
				where = append(where, "a.value LIKE ?")
				args = append(args, "%"+word+"%")
			}
		}
	}

	// several models mean any of them: AND-ing p.model against two values
	// asks one row to come from both, which nothing satisfies.
	models := filters["model"]
	if len(models) > 0 {
		where = append(where, "p.model IN ("+placeholders(len(models))+")")
		for _, m := range models {
			args = append(args, m)
		}
	}
	for _, f := range filters["field"] {
		where = append(where, "a.field = ?")
		args = append(args, f)
	}

	// value filters constrain the image, not the matched row -- but a query
	// that also names a model is asking about *that model's* rows:
	// `model:pixai tag:marisa` is "what did pixai tag marisa", while
	// `tag:marisa` on its own stays "every image with the tag, whoever said
	// so".
	var imgWhere []string
	var imgArgs []any
	for _, name := range valueFilters {
		for _, val := range filters[name] {
			q := []string{"SELECT 1 FROM annotations x"}
			if len(models) > 0 {
				q = append(q, "JOIN provenances q ON q.id = x.provenance_id")
			}
			q = append(q, "WHERE x.image_id = i.id AND x.field = ? "+
				"AND x.value = ? COLLATE NOCASE")
			imgArgs = append(imgArgs, name, val)
			if len(models) > 0 {
				q = append(q, "AND q.model IN ("+placeholders(len(models))+")")
				for _, m := range models {
					imgArgs = append(imgArgs, m)
				}
			}
			imgWhere = append(imgWhere, "EXISTS ("+strings.Join(q, " ")+")")
		}
	}

	// left join throughout: with no conditions this lists every image
	// (including one whose only story is a failure), and a condition on
	// a.* / p.* drops the non-matching images on its own.
	q := []string{
		"SELECT i.id, i.sha256, i.width, i.height,",
		"  (SELECT COUNT(*) FROM annotations y WHERE y.image_id = i.id) AS n_ann,",
		"  COUNT(a.id) AS hits, MAX(COALESCE(a.confidence, 0)) AS best",
		"FROM images i",
		"LEFT JOIN annotations a ON a.image_id = i.id",
		"LEFT JOIN provenances p ON p.id = a.provenance_id",
	}
	if clauses := append(append([]string{}, where...), imgWhere...); len(clauses) > 0 {
		q = append(q, "WHERE "+strings.Join(clauses, " AND "))
	}
	q = append(q, "GROUP BY i.id")
	if len(where) > 0 {
		q = append(q, "ORDER BY hits DESC, best DESC, i.id ASC")
	} else {
		q = append(q, "ORDER BY i.id ASC")
	}
	args = append(args, imgArgs...)
	sqlText := strings.Join(q, " ")

	rows, err := s.query(sqlText+" LIMIT ? OFFSET ?",
		append(append([]any{}, args...), perPage+1, pageNo*perPage)...)
	if err != nil {
		return nil, false, 0, err
	}
	total, err := s.count("SELECT COUNT(*) FROM ("+sqlText+")", args...)
	if err != nil {
		return nil, false, 0, err
	}
	more := len(rows) > perPage
	if more {
		rows = rows[:perPage]
	}
	return rows, more, total, nil
}

// Summary is the chips for a grid card: best subject, rating votes, counts.
func (s *Store) Summary(imageID any) (Row, []Row, int64, error) {
	subj, err := s.queryRow(
		"SELECT value, confidence FROM annotations WHERE image_id = ? "+
			"AND field = 'subject' ORDER BY COALESCE(confidence, 0) DESC LIMIT 1", imageID)
	if err != nil {
		return nil, nil, 0, err
	}
	ratings, err := s.query(
		"SELECT value, COUNT(*) n FROM annotations WHERE image_id = ? "+
			"AND field = 'rating' GROUP BY value ORDER BY n DESC", imageID)
	if err != nil {
		return nil, nil, 0, err
	}
	models, err := s.count(
		"SELECT COUNT(DISTINCT provenance_id) FROM annotations WHERE image_id = ?", imageID)
	return subj, ratings, models, err
}

// Paths is every name the picture was recorded under, on the machine that
// classified it.
func (s *Store) Paths(imageID any) ([]string, error) {
	rows, err := s.query(
		"SELECT path FROM image_paths WHERE image_id = ? ORDER BY path", imageID)
	var out []string
	for _, r := range rows {
		out = append(out, pystr(r["path"]))
	}
	return out, err
}

// RecordedFile is the newest recorded path and the sha256, for serving the
// file when no image directory was given.
func (s *Store) RecordedFile(imageID int64) (string, string, error) {
	r, err := s.queryRow(
		"SELECT i.sha256, p.path FROM images i JOIN image_paths p "+
			"ON p.image_id = i.id WHERE i.id = ? ORDER BY p.last_seen DESC", imageID)
	if err != nil || r == nil {
		return "", "", err
	}
	return pystr(r["path"]), pystr(r["sha256"]), nil
}

func (s *Store) Sha(imageID int64) (string, error) {
	v, err := s.queryOne("SELECT sha256 FROM images WHERE id = ?", imageID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return pystr(v), nil
}

// basename is os.path.basename on the classifying machine, which was posix:
// the recorded paths always use '/', whatever this one uses.
func basename(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// queryEscape is urllib.parse.quote(s) with the default safe='/'.
func quote(s string) string { return pyquote(s, "/") }

// quoteAll is quote(s, safe="").
func quoteAll(s string) string { return pyquote(s, "") }
