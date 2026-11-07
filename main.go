// slopbooru: a booru over the recognition store's sqlite db.
//
//	slopbooru --db results.db --images DIR [--host 127.0.0.1] [--port 8800]
//
// One static binary with no dependencies on the host: pure Go sqlite
// (modernc.org/sqlite, FTS5 included), stdlib http, Go image decoders for
// the thumbnails. Pages: / grid (search + facets), /image/<id> everything
// said about one image with per-value confidence and provenance,
// /value/<field>/<v> the booru-style listing, /provenance/<id> the nix
// closure that produced a set of answers, /stats coverage per model,
// /api/image/<id> the rows as json.
//
// Files are only ever served by image id, found by content in --images
// (see index.go), never by a path from the request.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

// set at link time: -X main.brand=llm-booru inside the llm-qwen project
var (
	brand   = "slopbooru"
	version = "dev"
)

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, brand+": "+format+"\n", a...)
}

func die(format string, a ...any) {
	logf(format, a...)
	os.Exit(1)
}

func main() {
	fs := flag.NewFlagSet(brand, flag.ExitOnError)
	db := fs.String("db", "", "the results.db to browse (required)")
	images := fs.String("images", "", "the directory holding the pictures; found by content, so names and layout\n"+
		"need not match the machine that made the db (default: the paths the db recorded)")
	host := fs.String("host", "127.0.0.1", "address to listen on (0.0.0.0: every interface)")
	port := fs.Int("port", 8800, "port to listen on")
	thumbs := fs.String("thumbs", "", "cache directory for thumbnails and file hashes (default: <db>.thumbs)")
	jobs := fs.Int("hash-jobs", min(4, runtime.NumCPU()), "files hashed in parallel while indexing --images")
	edit := fs.Bool("edit", false, "allow editing from the pages: add/remove values, upload/delete images\n(anyone who can reach --host:--port can edit; the db must be writable)")
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "%s %s: browse an image recognition db\n\n"+
			"usage: %s --db results.db --images DIR [--host 127.0.0.1] [--port 8800]\n\n",
			brand, version, filepath.Base(os.Args[0]))
		fs.PrintDefaults()
	}
	if len(os.Args) == 1 {
		fs.Usage()
		if runtime.GOOS == "windows" {
			// double-clicked from explorer: keep the console open long
			// enough to read the usage
			fmt.Fprint(os.Stderr, "\nrun it from a terminal with the options above. press enter to close.")
			bufio.NewReader(os.Stdin).ReadString('\n')
		}
		os.Exit(2)
	}
	fs.Parse(os.Args[1:])
	if *showVersion {
		fmt.Println(brand, version)
		return
	}
	if *db == "" || fs.NArg() > 0 {
		fs.Usage()
		os.Exit(2)
	}

	dbPath, err := filepath.Abs(*db)
	if err != nil {
		die("%v", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		die("no such db: %s", *db)
	}
	editMode = *edit
	store, err := OpenStore(dbPath, *edit)
	if err != nil {
		die("cannot open %s: %v", *db, err)
	}
	cacheDir := *thumbs
	if cacheDir == "" {
		cacheDir = dbPath + ".thumbs"
	}
	if cacheDir, err = filepath.Abs(cacheDir); err == nil {
		if err = os.MkdirAll(cacheDir, 0o755); err != nil {
			logf("no thumbnail cache (%v): serving originals", err)
			cacheDir = ""
		}
	}

	var counts []string
	for _, t := range []string{"images", "annotations", "provenances"} {
		n, err := store.count("SELECT COUNT(*) FROM " + t)
		if err != nil {
			die("%s does not look like a recognition db: %v", *db, err)
		}
		counts = append(counts, fmt.Sprintf("%d %s(s)", n, t[:len(t)-1]))
	}
	extra := ""
	if !store.fts {
		extra = "  [no FTS5: LIKE search]"
	}
	logf("%s, %s, %s%s", counts[0], counts[1], counts[2], extra)

	// bind before indexing: a port that is taken should fail now, not
	// after ten minutes of hashing
	addr := net.JoinHostPort(*host, strconv.Itoa(*port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		die("cannot listen on %s: %v", addr, err)
	}

	srv := &Server{store: store, thumbs: cacheDir, imagesDir: imagesAbs(*images),
		verbose: os.Getenv("SLOPBOORU_VERBOSE") != "" || os.Getenv("LLM_BOORU_VERBOSE") != ""}
	if *images != "" {
		if st, err := os.Stat(*images); err != nil || !st.IsDir() {
			die("no such directory: %s", *images)
		}
		start := time.Now()
		hashCache := ""
		if cacheDir != "" {
			hashCache = filepath.Join(cacheDir, "files.db")
		}
		skip := []string{dbPath, dbPath + "-wal", dbPath + "-shm", dbPath + "-journal", cacheDir}
		srv.index, err = BuildIndex(*images, hashCache, skip, *jobs)
		if err != nil {
			die("cannot index %s: %v", *images, err)
		}
		found, total := matched(store, srv.index)
		logf("index: %d file(s), %d hashed, %d from cache, in %.1fs; %d/%d of the db's images found",
			srv.index.Files, srv.index.Hashed, srv.index.Cached, time.Since(start).Seconds(), found, total)
	}

	logf("http://%s  (db %s)", addr, *db)
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 30 * time.Second}
	if err := hs.Serve(ln); err != nil {
		die("%v", err)
	}
}

// matched counts the db's images that the index has a file for.
func matched(s *Store, ix *Index) (int, int) {
	rows, err := s.query("SELECT sha256 FROM images")
	if err != nil {
		return 0, 0
	}
	n := 0
	for _, r := range rows {
		if ix.Path(pystr(r["sha256"])) != "" {
			n++
		}
	}
	return n, len(rows)
}

func imagesAbs(dir string) string {
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}
