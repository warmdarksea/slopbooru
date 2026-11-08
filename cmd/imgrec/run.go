package main

// imgrec run: batch mode. Every picture in DIR, on every model (or
// --models), until nothing is left to run.

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"imgrec/internal/imagestore"
	"imgrec/internal/pipeline"
	"imgrec/internal/records"
	"imgrec/internal/runner"
	"imgrec/internal/sched"
	"imgrec/internal/spec"
	"imgrec/internal/store"
)

// fleet is everything a run needs, from flags shared by run and serve.
type fleet struct {
	db, recordsDir, writer, imagesDir, specsDir, manifest, vllm, pixai, logs string
	models                                                                   string
	rotation                                                                 time.Duration
	maxAttempts                                                              int
}

// env is a flag's default from IMGREC_<NAME>: how the flake's wrapper
// hands over store paths (specs, manifest, vllm, the pixai worker).
func env(name, def string) string {
	if v := os.Getenv("IMGREC_" + name); v != "" {
		return v
	}
	return def
}

func (f *fleet) flags(fs *flag.FlagSet) {
	fs.StringVar(&f.db, "db", env("DB", "results.db"), "the store, the jobs and (by default) the records")
	fs.StringVar(&f.recordsDir, "records", env("RECORDS", ""), "keep records as jsonl in this directory instead of the db")
	host, _ := os.Hostname()
	fs.StringVar(&f.writer, "writer", env("WRITER", host), "this machine's jsonl file name (with --records)")
	fs.StringVar(&f.imagesDir, "images-store", env("IMAGES_STORE", ""), "where pictures are kept by content (default: <db>.images)")
	fs.StringVar(&f.specsDir, "specs", env("SPECS", ""), "the specs directory")
	fs.StringVar(&f.manifest, "manifest", env("MANIFEST", ""), "models.json: weights and closures, from the flake")
	fs.StringVar(&f.vllm, "vllm", env("VLLM", "vllm"), "the vllm executable")
	fs.StringVar(&f.pixai, "pixai", env("PIXAI", ""), "the pixai worker command, e.g. 'python3 pixai_worker.py'")
	fs.StringVar(&f.logs, "logs", env("LOGS", ""), "runner logs (default: <db>.logs)")
	fs.StringVar(&f.models, "models", env("MODELS", ""), "comma-separated models (default: every spec)")
	rot, _ := time.ParseDuration(env("ROTATION", "2h"))
	fs.DurationVar(&f.rotation, "rotation", rot, "a busy model gives way after this when others wait")
	fs.IntVar(&f.maxAttempts, "max-attempts", 3, "tries per job before it counts as failed")
}

func readLines(file string) []string {
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// open builds the pipeline and the scheduler from the flags.
func (f *fleet) open() (*pipeline.Pipeline, *sched.Scheduler, func(), error) {
	if f.specsDir == "" {
		return nil, nil, nil, fmt.Errorf("--specs is required")
	}
	specs, err := spec.LoadDir(f.specsDir)
	if err != nil {
		return nil, nil, nil, err
	}
	if f.models != "" {
		keep := map[string]bool{}
		for _, m := range strings.Split(f.models, ",") {
			keep[strings.TrimSpace(m)] = true
		}
		for name, s := range specs {
			// a needed model stays: its answers are an input
			if !keep[name] && !neededBy(specs, keep, name) {
				delete(specs, name)
			}
			_ = s
		}
	}
	pixaiArgv := strings.Fields(f.pixai)
	if f.manifest != "" {
		m, err := readManifest(f.manifest)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("manifest: %w", err)
		}
		for name, s := range specs {
			if mm, ok := m[name]; ok {
				s.Weights, s.Closure = mm.Path, readLines(mm.Closure)
			}
		}
	}
	for _, s := range specs {
		if s.Kind == "tagger" && len(pixaiArgv) > 0 {
			s.Worker = pixaiArgv[len(pixaiArgv)-1] // the script: part of the question
		}
	}
	st, err := store.Open(f.db)
	if err != nil {
		return nil, nil, nil, err
	}
	var rec records.Backend
	if f.recordsDir != "" {
		rec, err = records.OpenJsonl(f.recordsDir, f.writer)
	} else {
		rec, err = records.OpenSqlite(st.DB)
	}
	if err != nil {
		st.Close()
		return nil, nil, nil, err
	}
	imagesDir := f.imagesDir
	if imagesDir == "" {
		imagesDir = f.db + ".images"
	}
	im, err := imagestore.New(imagesDir)
	if err != nil {
		return nil, nil, nil, err
	}
	p, err := pipeline.New(specs, st, rec, im)
	if err != nil {
		return nil, nil, nil, err
	}
	p.MaxAttempts = f.maxAttempts
	p.Engine = func(model string) string {
		if specs[model].Kind == "tagger" {
			return f.pixai
		}
		return f.vllm
	}
	logs := f.logs
	if logs == "" {
		logs = f.db + ".logs"
	}
	os.MkdirAll(logs, 0o755)
	newRunner := func(s *spec.Spec) (runner.Runner, error) {
		logf, err := os.OpenFile(filepath.Join(logs, s.Name+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(logf, "\n==== %s %s\n", time.Now().Format(time.RFC3339), s.Name)
		if s.Kind == "tagger" {
			if len(pixaiArgv) == 0 {
				return nil, fmt.Errorf("no --pixai worker command")
			}
			argv := append(append([]string{}, pixaiArgv...), "--model", s.Weights)
			for _, k := range []string{"batch", "device"} {
				if v, ok := s.Runner[k]; ok {
					argv = append(argv, "--"+k, strings.Trim(spec.Canonical(v), `"`))
				}
			}
			for _, k := range []string{"threshold_general", "threshold_character"} {
				if v, ok := s.Request[k]; ok && v != nil {
					argv = append(argv, "--"+strings.ReplaceAll(k, "_", "-"), spec.Canonical(v))
				}
			}
			return &runner.Worker{Argv: argv, Log: logf, Timeout: 10 * time.Minute}, nil
		}
		port := freePort()
		argv := append([]string{f.vllm, "serve"}, s.ServeArgs(port, im.Root)...)
		return &runner.VLLM{Argv: argv, Port: port, Log: logf, Timeout: 10 * time.Minute,
			Env: []string{"VLLM_NO_USAGE_STATS=1", "DO_NOT_TRACK=1", "HF_HUB_OFFLINE=1", "TRANSFORMERS_OFFLINE=1"}}, nil
	}
	sc := &sched.Scheduler{P: p, NewRunner: newRunner, Rotation: f.rotation, Budget: 1.0,
		Logf: func(format string, a ...any) { fmt.Fprintf(os.Stderr, "imgrec: "+format+"\n", a...) }}
	closeAll := func() { rec.Close(); st.Close() }
	return p, sc, closeAll, nil
}

func neededBy(specs map[string]*spec.Spec, keep map[string]bool, name string) bool {
	for other, s := range specs {
		if keep[other] && s.Needs != nil && s.Needs.Model == name {
			return true
		}
	}
	return false
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var f fleet
	f.flags(fs)
	client := fs.String("client", "local", "who is asking")
	jobs := fs.Int("jobs", runtime.NumCPU(), "pictures prepared in parallel")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: imgrec run [flags] DIR")
	}
	p, sc, closeAll, err := f.open()
	if err != nil {
		return err
	}
	defer closeAll()

	// every picture in DIR (not recursive), stored by content
	entries, err := os.ReadDir(fs.Arg(0))
	if err != nil {
		return err
	}
	var paths []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			paths = append(paths, filepath.Join(fs.Arg(0), e.Name()))
		}
	}
	models := make([]string, 0, len(p.Specs))
	for name := range p.Specs {
		models = append(models, name)
	}
	sort.Strings(models)

	// intake on every core: storing, hashing, and the downscaled stand-ins
	// (made once per picture, kept, shared by every model) happen here
	t := time.Now()
	var mu sync.Mutex
	tally := map[string]int{}
	work := make(chan string)
	var wg sync.WaitGroup
	for range max(1, *jobs) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range work {
				im, err := p.Images.PutFile(path)
				if err != nil {
					mu.Lock()
					tally["not a picture"]++
					mu.Unlock()
					continue
				}
				for _, m := range models {
					res, err := p.Intake(m, im.SHA256, *client)
					mu.Lock()
					if err != nil {
						tally["error"]++
						fmt.Fprintf(os.Stderr, "imgrec: %s on %s: %v\n", m, path, err)
					} else {
						tally[res.Status.String()]++
					}
					mu.Unlock()
					if err == nil && res.Status == pipeline.Done {
						if err := p.Materialize(m, im.SHA256, res.Record); err != nil {
							fmt.Fprintf(os.Stderr, "imgrec: %s on %s: %v\n", m, path, err)
						}
					}
				}
			}
		}()
	}
	for _, path := range paths {
		work <- path
	}
	close(work)
	wg.Wait()
	fmt.Fprintf(os.Stderr, "imgrec: %d file(s), %d model(s): %d done, %d queued, %d failed%s (%s)\n",
		len(paths), len(models), tally["done"], tally["queued"], tally["failed"], extra(tally),
		time.Since(t).Round(time.Millisecond))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := sc.Run(ctx, true); err != nil && ctx.Err() == nil {
		return err
	}
	return summary(p.Store.DB, *client)
}

func extra(t map[string]int) string {
	s := ""
	if t["not a picture"] > 0 {
		s += fmt.Sprintf(", %d not a picture", t["not a picture"])
	}
	if t["error"] > 0 {
		s += fmt.Sprintf(", %d error(s)", t["error"])
	}
	return s
}

func summary(db *sql.DB, client string) error {
	rows, err := db.Query("SELECT model, COUNT(*), MAX(last_error) FROM jobs GROUP BY model ORDER BY model")
	if err != nil {
		return err
	}
	defer rows.Close()
	left := 0
	for rows.Next() {
		var m string
		var n int
		var e sql.NullString
		rows.Scan(&m, &n, &e)
		left += n
		fmt.Fprintf(os.Stderr, "imgrec: %s: %d job(s) not done (last error: %s)\n", m, n, e.String)
	}
	if left == 0 {
		fmt.Fprintln(os.Stderr, "imgrec: every job done")
	}
	return rows.Err()
}
