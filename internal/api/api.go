// Package api is imgrec's HTTP interface. A job is one picture on one
// model; every call is idempotent, so a client's "queue" is just its own
// list of inputs, and re-running a client is how it polls.
//
//	GET    /v1/models                  what this server can run
//	PUT    /v1/images/{sha256}         upload (hash verified): 201 new to you, 200 had it
//	POST   /v1/images                  upload without knowing the hash: 201/200 + the sha
//	POST   /v1/jobs {image, model}     200 + result (answered), 202 + Location (queued)
//	GET    /v1/jobs/{model}/{sha}      200 result | 202 pending | 404; ?wait=N holds up to N s
//	DELETE /v1/jobs/{model}/{sha}      withdraw your interest (it still runs if others want it)
//	GET    /v1/queue                   per model: state, in flight, your pending jobs
//
// Every call carries Authorization: Bearer <token>. A client sees only the
// pictures it submitted (an admin sees all); one it can't see is a 404,
// never a 403, so clients can't probe for each other's pictures.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
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
	case r.Method == "PUT" && len(parts) == 3 && parts[0] == "v1" && parts[1] == "images":
		s.upload(w, r, c, parts[2])
	case r.Method == "POST" && r.URL.Path == "/v1/images":
		s.upload(w, r, c, "")
	case r.Method == "POST" && r.URL.Path == "/v1/jobs":
		s.postJob(w, r, c)
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

func (s *Server) upload(w http.ResponseWriter, r *http.Request, c auth.Client, want string) {
	if want != "" && !shaRE.MatchString(want) {
		fail(w, 400, "not a sha256: %q", want)
		return
	}
	body := http.MaxBytesReader(w, r.Body, s.MaxUpload)
	im, err := s.P.Images.Put(body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail(w, 413, "a picture is at most %d bytes", s.MaxUpload)
		} else {
			fail(w, 400, "%v", err)
		}
		return
	}
	if want != "" && im.SHA256 != want {
		fail(w, 400, "the bytes hash to %s, not %s", im.SHA256, want)
		return
	}
	isNew, err := s.Auth.Own(c.Name, im.SHA256)
	if err != nil {
		fail(w, 500, "%v", err)
		return
	}
	code := 200
	if isNew {
		code = 201 // new to *you*: whether anyone else had it is not your business
	}
	writeJSON(w, code, map[string]any{"image": im.SHA256, "bytes": im.Bytes, "mime": im.Mime,
		"width": im.Width, "height": im.Height})
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

func (s *Server) visible(w http.ResponseWriter, c auth.Client, model, sha string) bool {
	if _, ok := s.P.Specs[model]; !ok {
		fail(w, 404, "no model %q (GET /v1/models)", model)
		return false
	}
	if !shaRE.MatchString(sha) {
		fail(w, 400, "not a sha256: %q", sha)
		return false
	}
	ok, err := s.Auth.Sees(c, sha)
	if err != nil {
		fail(w, 500, "%v", err)
		return false
	}
	if !ok {
		fail(w, 404, "no picture %s (upload it first)", sha)
	}
	return ok
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
	if !s.visible(w, c, req.Model, req.Image) {
		return
	}
	res, err := s.P.Intake(req.Model, req.Image, c.Name)
	if err != nil {
		fail(w, 500, "%v", err)
		return
	}
	if res.Status == pipeline.Done {
		// an answer that arrived another way (another machine's records)
		// belongs in the store too; idempotent
		if err := s.P.Materialize(req.Model, req.Image, res.Record); err != nil && s.Logf != nil {
			s.Logf("%s %s: %v", req.Model, req.Image[:12], err)
		}
	}
	code, out := s.view(req.Model, req.Image, res)
	if code == 202 {
		w.Header().Set("Location", location(req.Model, req.Image))
	}
	writeJSON(w, code, out)
}

func (s *Server) job(w http.ResponseWriter, r *http.Request, c auth.Client, model, sha string) {
	model, _ = url.PathUnescape(model)
	if !s.visible(w, c, model, sha) {
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
