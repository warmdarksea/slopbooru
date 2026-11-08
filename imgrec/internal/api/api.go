// Package api is imgrec's HTTP interface: two calls, and everything else
// follows from their types.
//
//	ask    :: [(ImageHash, Model)] -> [Maybe Answer]     POST /v1/ask[?wait=N]
//	submit :: Image -> [Model] -> WorkingOnIt            POST /v1/submit (multipart)
//
// ask is a pure query: it never creates work. An Answer is what a model
// said -- or, once a job has run out of attempts, why it couldn't (success
// false), so a broken picture is answered rather than pending for ever.
// Nothing means no answer *for you* yet: not asked, still running, or a
// picture you have never sent -- the same Nothing either way, so a hash
// alone tells a client nothing about anyone else's pictures. With ?wait=N
// the call holds until every answer is Just, or N seconds pass.
//
// submit is the only call that creates work: the picture (verified
// against its hash) and the models to run it on; one upload covers them
// all. It answers 202 and nothing more -- ask again for the answers.
// Answers are shared: a picture two clients submit runs once. The server
// deletes an uploaded picture once no job needs it; the answers stay.
//
// So a client is: ask everything; submit each picture whose answers came
// back Nothing; ask again, waiting, until every answer is Just.
//
// Also: GET /v1/models (what can be asked), GET /v1/queue (what's running).
// Every call carries Authorization: Bearer <token>; an admin sees all.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"slopbooru/imgrec/internal/auth"
	"slopbooru/imgrec/internal/pipeline"
	"slopbooru/imgrec/internal/records"
	"slopbooru/imgrec/internal/sched"
)

type Server struct {
	P            *pipeline.Pipeline
	Auth         *auth.Auth
	Sched        *sched.Scheduler
	MaxUpload    int64         // bytes per picture
	MaxWait      time.Duration // cap on ?wait=
	MaxQuestions int           // per ask (default 1024)
	Logf         func(format string, a ...any)
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
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/ask":
		s.ask(w, r, c)
	case r.Method == "POST" && r.URL.Path == "/v1/submit":
		s.submit(w, r, c)
	case r.Method == "GET" && r.URL.Path == "/v1/models":
		s.models(w)
	case r.Method == "GET" && r.URL.Path == "/v1/queue":
		s.queue(w, c)
	default:
		fail(w, 404, "no such endpoint: %s %s (POST /v1/ask, POST /v1/submit)", r.Method, r.URL.Path)
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

// Question is one (picture, model) pair.
type Question struct {
	Image string `json:"image"`
	Model string `json:"model"`
}

// answer is Just an answer, or nil (Nothing). No side effects.
func (s *Server) answer(c auth.Client, q Question) (map[string]any, error) {
	sees, err := s.Auth.Sees(c, q.Image)
	if err != nil || !sees {
		return nil, err // not your picture: Nothing, exactly as if unanswered
	}
	res, exists, err := s.P.Status(q.Model, q.Image)
	if err != nil || !exists {
		return nil, err
	}
	switch res.Status {
	case pipeline.Done:
		out := map[string]any{"image": q.Image, "model": q.Model, "record": recordMeta(res.Record)}
		if c, ok := s.P.Contribution(q.Model, res.Record); ok {
			out["success"], out["fields"], out["detail"], out["error"] = c.Success, c.Fields, c.Detail, c.Error
		} else {
			out["success"], out["error"] = false, "the answer does not parse"
		}
		return out, nil
	case pipeline.Failed:
		return map[string]any{"image": q.Image, "model": q.Model, "success": false, "error": res.Error}, nil
	}
	return nil, nil
}

func recordMeta(r *records.Record) map[string]any {
	return map[string]any{"key": r.Key, "spec_hash": r.SpecHash, "input_sha": r.InputSHA,
		"elapsed": r.Elapsed, "engine": r.Engine, "at": r.At}
}

func (s *Server) ask(w http.ResponseWriter, r *http.Request, c auth.Client) {
	limit := s.MaxQuestions
	if limit == 0 {
		limit = 1024
	}
	var req struct {
		Questions []Question `json:"questions"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, int64(limit)*256)).Decode(&req); err != nil {
		fail(w, 400, "body: %v", err)
		return
	}
	if len(req.Questions) > limit {
		fail(w, 400, "at most %d questions per ask", limit)
		return
	}
	for _, q := range req.Questions {
		if _, ok := s.P.Specs[q.Model]; !ok {
			fail(w, 400, "no model %q (GET /v1/models)", q.Model)
			return
		}
		if !shaRE.MatchString(q.Image) {
			fail(w, 400, "not a sha256: %q", q.Image)
			return
		}
	}
	wait, _ := strconv.ParseFloat(r.URL.Query().Get("wait"), 64)
	deadline := time.Now().Add(min(time.Duration(wait*float64(time.Second)), s.MaxWait))
	answers := make([]map[string]any, len(req.Questions))
	for {
		pending := 0
		for i, q := range req.Questions {
			if answers[i] != nil {
				continue
			}
			a, err := s.answer(c, q)
			if err != nil {
				fail(w, 500, "%v", err)
				return
			}
			if a == nil {
				pending++
			}
			answers[i] = a
		}
		if pending == 0 || !time.Now().Before(deadline) {
			out := make([]any, len(answers))
			for i, a := range answers {
				if a != nil {
					out[i] = a
				} // else null: Nothing
			}
			writeJSON(w, 200, map[string]any{"answers": out})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// submit takes a picture and the models to run it on (multipart: field
// image = its sha256, field models = names, comma-separated or repeated,
// then file = the bytes). 202: working on it.
func (s *Server) submit(w http.ResponseWriter, r *http.Request, c auth.Client) {
	r.Body = http.MaxBytesReader(w, r.Body, s.MaxUpload+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		fail(w, 400, "submit is multipart: image, models, file (%v)", err)
		return
	}
	var sha string
	var models []string
	uploaded := false
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.uploadError(w, err)
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
			// the fields come first: the hash must be known to check the bytes
			if !shaRE.MatchString(sha) || len(models) == 0 {
				fail(w, 400, "send the image (sha256) and models fields before the file")
				return
			}
			for _, m := range models {
				if _, ok := s.P.Specs[m]; !ok {
					fail(w, 400, "no model %q (GET /v1/models)", m)
					return
				}
			}
			if _, err := s.P.Upload(part, sha); err != nil {
				s.uploadError(w, err)
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
	for _, m := range models {
		res, err := s.P.Intake(m, sha, c.Name)
		if err != nil {
			fail(w, 500, "%v", err)
			return
		}
		if res.Status == pipeline.Done {
			// an answer that arrived another way (another machine's
			// records) belongs in the store too; idempotent
			if err := s.P.Materialize(m, sha, res.Record); err != nil && s.Logf != nil {
				s.Logf("%s %s: %v", m, sha[:12], err)
			}
		}
	}
	writeJSON(w, 202, map[string]any{"image": sha, "models": models, "working": true})
}

func (s *Server) uploadError(w http.ResponseWriter, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		fail(w, 413, "a picture is at most %d bytes", s.MaxUpload)
	} else {
		fail(w, 400, "%v", err)
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
