package index

// The image directory, indexed by content.
//
// The db names pictures by sha256 and records the paths they had on the
// machine that classified them, which mean nothing here. So every start
// walks the whole directory, and every file is identified by hashing it --
// unless a file with the same (directory, name, mtime, size) was hashed on
// an earlier start, in which case the hash is taken from the cache. A
// slow start on a big directory the first time, and always correct: a
// renamed, moved or replaced file changes one of the four and is hashed
// again.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"slopbooru/internal/cli"
	"slopbooru/internal/sqliteuri"

	_ "modernc.org/sqlite" // the driver, for the hash cache
)

type fileKey struct {
	dir, name   string
	mtime, size int64
}

type Index struct {
	mu     sync.RWMutex        // uploads add while requests read
	byHash map[string][]string // sha256 -> absolute paths, sorted
	Files  int
	Cached int
	Hashed int
}

// Path is the file to serve for a sha256, or "".
func (ix *Index) Path(sha string) string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if ps := ix.byHash[sha]; len(ps) > 0 {
		return ps[0]
	}
	return ""
}

// Add records a file written while serving (an upload).
func (ix *Index) Add(sha, path string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.byHash[sha] = append(ix.byHash[sha], path)
	sort.Strings(ix.byHash[sha])
}

const hashCacheSchema = `
CREATE TABLE IF NOT EXISTS files (
  dir      TEXT    NOT NULL,  -- absolute directory, without the name
  name     TEXT    NOT NULL,
  mtime_ns INTEGER NOT NULL,
  size     INTEGER NOT NULL,
  sha256   TEXT    NOT NULL,
  PRIMARY KEY (dir, name, mtime_ns, size)
) WITHOUT ROWID;`

// BuildIndex walks root (recursively) and returns sha256 -> paths. cache is
// a sqlite file for the hashes ("" to hash everything, every time); skip
// holds absolute paths -- files or directories -- that are not images: the
// db, its thumbnail dir, the cache itself.
func BuildIndex(root, cache string, skip []string, jobs int) (*Index, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	skipSet := map[string]bool{}
	for _, s := range skip {
		if a, err := filepath.Abs(s); err == nil {
			skipSet[filepath.Clean(a)] = true
		}
	}

	// 1. the walk: every regular file, stat only
	type entry struct {
		key  fileKey
		path string
	}
	var files []entry
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			cli.Logf("index: %v", err) // an unreadable subdirectory is not fatal
			if d != nil && d.IsDir() && p != root {
				return fs.SkipDir
			}
			return nil
		}
		if skipSet[filepath.Clean(p)] {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := os.Stat(p) // follows a symlink to a file
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		files = append(files, entry{fileKey{filepath.Dir(p), filepath.Base(p),
			info.ModTime().UnixNano(), info.Size()}, p})
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 2. what the cache already knows
	var db *sql.DB
	known := map[fileKey]string{}
	if cache != "" {
		db, err = openHashCache(cache)
		if err != nil {
			cli.Logf("index: no hash cache (%v); hashing everything", err)
			db = nil
		} else {
			defer db.Close()
			if known, err = loadHashCache(db); err != nil {
				return nil, err
			}
		}
	}

	ix := &Index{byHash: map[string][]string{}, Files: len(files)}
	var todo []entry
	for _, f := range files {
		if sha, ok := known[f.key]; ok {
			ix.byHash[sha] = append(ix.byHash[sha], f.path)
			ix.Cached++
		} else {
			todo = append(todo, f)
		}
	}

	// 3. hash the rest, writing each batch to the cache as it completes so
	// that an interrupted start keeps what it did.
	type result struct {
		e   entry
		sha string
	}
	in := make(chan entry)
	out := make(chan result)
	var wg sync.WaitGroup
	for range max(1, jobs) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range in {
				sha, err := hashFile(f.path)
				if err != nil {
					cli.Logf("index: %v", err)
					continue
				}
				out <- result{f, sha}
			}
		}()
	}
	go func() {
		for _, f := range todo {
			in <- f
		}
		close(in)
		wg.Wait()
		close(out)
	}()

	var batch []result
	var bytesDone int64
	start, lastSave, lastSay := time.Now(), time.Now(), time.Now()
	flush := func() {
		if db != nil && len(batch) > 0 {
			m := make(map[fileKey]string, len(batch))
			for _, r := range batch {
				m[r.e.key] = r.sha
			}
			if err := saveHashes(db, m); err != nil {
				cli.Logf("index: cache write failed: %v", err)
			}
		}
		batch = batch[:0]
		lastSave = time.Now()
	}
	if len(todo) > 0 {
		cli.Logf("index: %d file(s) under %s, %d known, hashing %d", len(files), root, ix.Cached, len(todo))
	}
	for r := range out {
		ix.byHash[r.sha] = append(ix.byHash[r.sha], r.e.path)
		ix.Hashed++
		bytesDone += r.e.key.size
		batch = append(batch, r)
		if len(batch) >= 500 || time.Since(lastSave) > 10*time.Second {
			flush()
		}
		if time.Since(lastSay) > 5*time.Second {
			el := time.Since(start).Seconds()
			cli.Logf("index: hashed %d/%d (%.0f MB/s)", ix.Hashed, len(todo), float64(bytesDone)/1e6/el)
			lastSay = time.Now()
		}
	}
	flush()

	// 4. forget files under this root that are gone or changed
	if db != nil {
		seen := map[fileKey]bool{}
		for _, f := range files {
			seen[f.key] = true
		}
		var stale []fileKey
		for k := range known {
			if !seen[k] && under(k.dir, root) {
				stale = append(stale, k)
			}
		}
		if err := dropHashes(db, stale); err != nil {
			cli.Logf("index: cache prune failed: %v", err)
		}
	}

	for _, ps := range ix.byHash {
		sort.Strings(ps)
	}
	return ix, nil
}

func under(dir, root string) bool {
	if dir == root {
		return true
	}
	return strings.HasPrefix(dir, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func openHashCache(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", sqliteuri.For(path, "mode=rwc&_pragma=busy_timeout(5000)"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(hashCacheSchema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func loadHashCache(db *sql.DB) (map[fileKey]string, error) {
	rows, err := db.Query("SELECT dir, name, mtime_ns, size, sha256 FROM files")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[fileKey]string{}
	for rows.Next() {
		var k fileKey
		var sha string
		if err := rows.Scan(&k.dir, &k.name, &k.mtime, &k.size, &sha); err != nil {
			return nil, err
		}
		m[k] = sha
	}
	return m, rows.Err()
}

func saveHashes(db *sql.DB, m map[fileKey]string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare("INSERT OR REPLACE INTO files VALUES (?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	for k, sha := range m {
		if _, err := st.Exec(k.dir, k.name, k.mtime, k.size, sha); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func dropHashes(db *sql.DB, keys []fileKey) error {
	if len(keys) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, k := range keys {
		if _, err := tx.Exec("DELETE FROM files WHERE dir = ? AND name = ? AND mtime_ns = ? AND size = ?",
			k.dir, k.name, k.mtime, k.size); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Hashes are every content in the index, sorted.
func (ix *Index) Hashes() []string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]string, 0, len(ix.byHash))
	for h := range ix.byHash {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}
