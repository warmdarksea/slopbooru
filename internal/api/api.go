// Package api is imgrec's HTTP interface. A job is one picture on one
// model; every call is idempotent, so a client's "queue" is just its own
// list of inputs, and re-running a client is how it polls.
//
//	GET    /v1/models                       what this server can run
//	POST   /v1/jobs {image, model}          by hash: 200 + answer | 202 queued | 404 need_image
//	POST   /v1/jobs multipart               image (sha), models, file: the picture itself
//	GET    /v1/jobs/{model}/{sha}?wait=N    200 answer | 202 pending | 404
//	DELETE /v1/jobs/{model}/{sha}           withdraw your interest
//	GET    /v1/queue                        per model: state, in flight, your pending jobs
//
// Pictures travel only when needed: ask by hash first, and send the bytes
// on need_image. The server deletes an uploaded picture once no job needs
// it; the answers stay.
//
// Every call carries Authorization: Bearer <token>. Answers are shared --
// the same picture is answered once, whoever asks -- but a client gets
// one by hash only for pictures it has itself sent at least once.
// Otherwise it is need_image, exactly as if nothing were cached, so a hash
// alone tells nobody whether anyone else ever submitted that picture. (A
// client holding the bytes can still tell, from the instant answer, that
// someone ran them: the price of sharing answers at all.) An admin sees
// everything.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"imgrec/internal/auth"
	"imgrec/internal/pipeline"
	"imgrec/internal/records"
	"imgrec/internal/sched"
)

type Server struct {
	P         *pipeline.Pipeline
	Auth      *auth.Auth
	Sched     *sched.Scheduler
	MaxUpload int64         // bytes per picture
	MaxWait   time.Duration // cap on ?wait=
	Logf      func(format string, a ...any)
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func fail(w http.ResponseWriter, code int, format string, a ...any) {
	writeJSON(w, code, map[string]any{"error": fmt.Sprintf(format, a...)})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="imgrec"`)
		fail(w, 401, "send Authorization: Bearer <token> (imgrec client add NAME makes one)")
		return
	}
	c, err := s.Auth.Authenticate(strings.TrimSpace(token))
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="imgrec", error="invalid_token"`)
		fail(w, 401, "unknown token")
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/models":
		s.models(w)
	case r.Method == "POST" && r.URL.Path == "/v1/jobs":
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			s.postJobWithImage(w, r, c)
		} else {
			s.postJob(w, r, c)
		}
	case len(parts) == 4 && parts[0] == "v1" && parts[1] == "jobs" && (r.Method == "GET" || r.Method == "DELETE"):
		s.job(w, r, c, parts[2], parts[3])
	case r.Method == "GET" && r.URL.Path == "/v1/queue":
		s.queue(w, c)
	default:
		fail(w, 404, "no such endpoint: %s %s", r.Method, r.URL.Path)
	}
}

func (s *Server) models(w http.ResponseWriter) {
	var out []map[string]any
	for name, sp := range s.P.Specs {
		m := map[string]any{"name": name, "kind": sp.Kind, "spec_hash": sp.Hash(), "output": sp.Output}
		if sp.Needs != nil {
			m["needs"] = sp.Needs
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	writeJSON(w, 200, map[string]any{"models": out})
}

// view is a job's answer as the API returns it.
func (s *Server) view(model, sha string, res pipeline.Result) (int, map[string]any) {
	out := map[string]any{"status": res.Status.String(), "model": model, "image": sha}
	switch res.Status {
	case pipeline.Done:
		out["record"] = recordMeta(res.Record)
		if c, ok := s.P.Contribution(model, res.Record); ok {
			out["result"] = map[string]any{"success": c.Success, "fields": c.Fields,
				"detail": c.Detail, "error": c.Error}
		}
		return 200, out
	case pipeline.Failed:
		out["error"] = res.Error
		return 200, out
	case pipeline.NeedImage:
		out["need_image"] = true
		out["error"] = "send the picture: POST /v1/jobs as multipart, with the file"
		return 404, out
	}
	return 202, out
}

func recordMeta(r *records.Record) map[string]any {
	return map[string]any{"key": r.Key, "spec_hash": r.SpecHash, "input_sha": r.InputSHA,
		"elapsed": r.Elapsed, "engine": r.Engine, "at": r.At}
}

func location(model, sha string) string {
	return "/v1/jobs/" + url.PathEscape(model) + "/" + sha
}

// check validates a model and a hash; sees is whether this client has sent
// that picture (or is admin) -- what entitles it to answers by hash.
func (s *Server) check(w http.ResponseWriter, c auth.Client, model, sha string) (sees, ok bool) {
	if _, exists := s.P.Specs[model]; !exists {
		fail(w, 404, "no model %q (GET /v1/models)", model)
		return false, false
	}
	if !shaRE.MatchString(sha) {
		fail(w, 400, "not a sha256: %q", sha)
		return false, false
	}
	sees, err := s.Auth.Sees(c, sha)
	if err != nil {
		fail(w, 500, "%v", err)
		return false, false
	}
	return sees, true
}

// answer runs intake for one job and says what came of it.
func (s *Server) answer(c auth.Client, model, sha string) (int, map[string]any, error) {
	res, err := s.P.Intake(model, sha, c.Name)
	if err != nil {
		return 0, nil, err
	}
	if res.Status == pipeline.Done {
		// an answer that arrived another way (another machine's records)
		// belongs in the store too; idempotent
		if err := s.P.Materialize(model, sha, res.Record); err != nil && s.Logf != nil {
			s.Logf("%s %s: %v", model, sha[:12], err)
		}
	}
	code, out := s.view(model, sha, res)
	if code == 202 {
		out["location"] = location(model, sha)
	}
	return code, out, nil
}

func (s *Server) postJob(w http.ResponseWriter, r *http.Request, c auth.Client) {
	var req struct {
		Image string `json:"image"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		fail(w, 400, "body: %v", err)
		return
	}
	sees, ok := s.check(w, c, req.Model, req.Image)
	if !ok {
		return
	}
	if !sees {
		// never sent by this client: need_image, whether or not anything is
		// cached -- a hash alone must not reveal that anyone else sent it
		code, out := s.view(req.Model, req.Image, pipeline.Result{Status: pipeline.NeedImage})
		writeJSON(w, code, out)
		return
	}
	code, out, err := s.answer(c, req.Model, req.Image)
	if err != nil {
		fail(w, 500, "%v", err)
		return
	}
	if code == 202 {
		w.Header().Set("Location", location(req.Model, req.Image))
	}
	writeJSON(w, code, out)
}

// postJobWithImage is a job with the picture attached (multipart: field
// image = its sha256, field models = one or more model names, file part
// file = the bytes), for after a need_image. One upload serves every model
// named. 202 if any job is queued, else 200; the body lists each.
func (s *Server) postJobWithImage(w http.ResponseWriter, r *http.Request, c auth.Client) {
	r.Body = http.MaxBytesReader(w, r.Body, s.MaxUpload+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		fail(w, 400, "%v", err)
		return
	}
	var sha string
	var models []string
	var uploaded bool
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				fail(w, 413, "a picture is at most %d bytes", s.MaxUpload)
			} else {
				fail(w, 400, "%v", err)
			}
			return
		}
		switch part.FormName() {
		case "image":
			b, _ := io.ReadAll(io.LimitReader(part, 128))
			sha = strings.TrimSpace(string(b))
		case "models", "model":
			b, _ := io.ReadAll(io.LimitReader(part, 1<<12))
			for _, m := range strings.Split(string(b), ",") {
				if m = strings.TrimSpace(m); m != "" {
					models = append(models, m)
				}
			}
		case "file":
			// the fields come first: the hash must be known to verify the bytes
			if !shaRE.MatchString(sha) || len(models) == 0 {
				fail(w, 400, "send the image (sha256) and models fields before the file")
				return
			}
			for _, m := range models {
				if _, ok := s.check(w, c, m, sha); !ok {
					return
				}
			}
			if _, err := s.P.Upload(part, sha); err != nil {
				var tooBig *http.MaxBytesError
				if errors.As(err, &tooBig) {
					fail(w, 413, "a picture is at most %d bytes", s.MaxUpload)
				} else {
					fail(w, 400, "%v", err)
				}
				return
			}
			if _, err := s.Auth.Own(c.Name, sha); err != nil {
				fail(w, 500, "%v", err)
				return
			}
			uploaded = true
		}
	}
	if !uploaded {
		fail(w, 400, "no file part")
		return
	}
	code := 200
	var jobs []map[string]any
	for _, m := range models {
		jc, out, err := s.answer(c, m, sha)
		if err != nil {
			fail(w, 500, "%v", err)
			return
		}
		if jc == 202 {
			code = 202
		}
		jobs = append(jobs, out)
	}
	writeJSON(w, code, map[string]any{"image": sha, "jobs": jobs})
}

func (s *Server) job(w http.ResponseWriter, r *http.Request, c auth.Client, model, sha string) {
	model, _ = url.PathUnescape(model)
	sees, ok := s.check(w, c, model, sha)
	if !ok {
		return
	}
	if !sees {
		fail(w, 404, "no job %s on %s", sha, model)
		return
	}
	if r.Method == "DELETE" {
		if err := s.P.Cancel(model, sha, c.Name); err != nil {
			fail(w, 500, "%v", err)
			return
		}
		w.WriteHeader(204)
		return
	}
	wait, _ := strconv.ParseFloat(r.URL.Query().Get("wait"), 64)
	deadline := time.Now().Add(min(time.Duration(wait*float64(time.Second)), s.MaxWait))
	for {
		res, exists, err := s.P.Status(model, sha)
		if err != nil {
			fail(w, 500, "%v", err)
			return
		}
		if !exists {
			fail(w, 404, "no job %s on %s (POST /v1/jobs to ask)", sha, model)
			return
		}
		if res.Status != pipeline.Queued || !time.Now().Before(deadline) {
			code, out := s.view(model, sha, res)
			if code == 202 {
				w.Header().Set("Location", location(model, sha))
				w.Header().Set("Retry-After", "5")
			}
			writeJSON(w, code, out)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (s *Server) queue(w http.ResponseWriter, c auth.Client) {
	q := "SELECT model, COUNT(DISTINCT image_sha) FROM jobs WHERE attempts < ? AND client = ? GROUP BY model"
	args := []any{s.P.MaxAttempts, c.Name}
	if c.Admin {
		q = "SELECT model, COUNT(DISTINCT image_sha) FROM jobs WHERE attempts < ? GROUP BY model"
		args = args[:1]
	}
	rows, err := s.P.Store.DB.Query(q, args...)
	if err != nil {
		fail(w, 500, "%v", err)
		return
	}
	pending := map[string]int{}
	for rows.Next() {
		var m string
		var n int
		rows.Scan(&m, &n)
		pending[m] = n
	}
	rows.Close()
	var models []map[string]any
	now := time.Now()
	if s.Sched != nil {
		for _, m := range s.Sched.Snapshot() {
			e := map[string]any{"name": m.Name, "state": m.State.String(), "inflight": m.Inflight,
				"pending": pending[m.Name]}
			if m.State == sched.Up || m.State == sched.Draining {
				e["up_for_s"] = int(now.Sub(m.UpSince).Seconds())
			}
			models = append(models, e)
		}
	}
	writeJSON(w, 200, map[string]any{"client": c.Name, "admin": c.Admin, "models": models})
}
