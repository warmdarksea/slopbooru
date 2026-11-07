package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Server struct {
	store     *Store
	index     *Index // nil: serve the paths recorded in the db
	thumbs    string
	imagesDir string // --images, where uploads go ("" if none)
	verbose   bool
}

// errBadRequest is the ValueError of the Python handler: a 400, not a 500.
var errBadRequest = errors.New("bad request")

// pyint is int(s) for the forms a url carries: surrounding space, a sign.
func pyint(s string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, errBadRequest
	}
	return n, nil
}

// arg is parse_qs(query)[name][0], which skips blank values.
func arg(q url.Values, name, def string) string {
	for _, v := range q[name] {
		if v != "" {
			return v
		}
	}
	return def
}

// pathParts splits the raw path and unquotes each piece, so an encoded '/'
// inside a value (/value/tag/a%2Fb) stays inside its piece.
func pathParts(r *http.Request) []string {
	var parts []string
	for _, p := range strings.Split(strings.Trim(r.URL.EscapedPath(), "/"), "/") {
		if p == "" {
			continue
		}
		u, err := url.PathUnescape(p)
		if err != nil {
			u = p
		}
		parts = append(parts, strings.ToValidUTF8(u, string(utf8.RuneError)))
	}
	return parts
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", brand)
	sw := &statusWriter{w, 200}
	if s.verbose {
		defer func() {
			fmt.Fprintf(os.Stderr, "%s - - [%s] \"%s %s %s\" %d -\n",
				strings.Split(r.RemoteAddr, ":")[0], time.Now().Format("02/Jan/2006 15:04:05"),
				r.Method, r.RequestURI, r.Proto, sw.code)
		}()
	}
	var err error
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		err = s.route(sw, r)
	case http.MethodPost:
		err = s.post(sw, r)
	default:
		sendHTML(sw, page("501", "<h1>not implemented</h1>", ""), 501)
		return
	}
	switch {
	case err == nil:
	case errors.Is(err, errBadRequest):
		sendHTML(sw, page("400", "<h1>bad request</h1>", ""), 400)
	default:
		sendHTML(sw, page("500", "<h1>db error</h1><p class='mono'>"+e(err.Error())+"</p>", ""), 500)
	}
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) error {
	parts := pathParts(r)
	q, _ := url.ParseQuery(r.URL.RawQuery)
	pageNo, err := pyint(arg(q, "page", "0"))
	if err != nil {
		return err
	}
	pageNo = max(0, pageNo)
	qs := arg(q, "q", "")

	if len(parts) == 0 {
		text, err := s.store.RenderIndex(qs, int(pageNo))
		return htmlOr404(w, text, err)
	}
	switch {
	case parts[0] == "upload" && len(parts) == 1 && editMode:
		sendHTML(w, uploadPage(s.imagesDir != ""), 200)
		return nil
	case parts[0] == "stats":
		text, err := s.store.RenderStats()
		return htmlOr404(w, text, err)
	case parts[0] == "image" && len(parts) == 2:
		id, err := pyint(parts[1])
		if err != nil {
			return err
		}
		text, err := s.store.RenderImage(id, arg(q, "sort", "conf"))
		if err == nil && text == "" {
			sendHTML(w, page("404", "<h1>no such image</h1>", ""), 404)
			return nil
		}
		return htmlOr404(w, text, err)
	case parts[0] == "provenance" && len(parts) == 2:
		id, err := pyint(parts[1])
		if err != nil {
			return err
		}
		text, err := s.store.RenderProvenance(id)
		if err == nil && text == "" {
			sendHTML(w, page("404", "<h1>no such provenance</h1>", ""), 404)
			return nil
		}
		return htmlOr404(w, text, err)
	case parts[0] == "value" && len(parts) == 3:
		text, err := s.store.RenderValue(parts[1], parts[2], int(pageNo))
		return htmlOr404(w, text, err)
	case (parts[0] == "thumb" || parts[0] == "file") && len(parts) == 2:
		id, err := pyint(parts[1])
		if err != nil {
			return err
		}
		path, sha, err := s.fileFor(id)
		if err != nil {
			return err
		}
		if path == "" {
			sendHTML(w, page("404", "<h1>file is gone</h1>", ""), 404)
			return nil
		}
		if parts[0] == "file" {
			sendFile(w, r, path, "")
			return nil
		}
		size, err := pyint(arg(q, "s", "256"))
		if err != nil {
			return err
		}
		size = min(1024, max(32, size))
		if thumb := thumbnail(path, sha, int(size), s.thumbs); thumb != "" {
			sendFile(w, r, thumb, "image/jpeg")
		} else {
			sendFile(w, r, path, "")
		}
		return nil
	case parts[0] == "api" && len(parts) == 3 && parts[1] == "image":
		id, err := pyint(parts[2])
		if err != nil {
			return err
		}
		rows, err := s.store.query(
			"SELECT model, field, key, value, confidence, detail "+
				"FROM annotation_rows WHERE image_id = ?", id)
		if err != nil {
			return err
		}
		out := make([][]kv, len(rows))
		for i, r := range rows {
			for _, k := range []string{"model", "field", "key", "value", "confidence", "detail"} {
				out[i] = append(out[i], kv{k, r[k]})
			}
		}
		data := []byte(pyjson(out, 2, false))
		w.Header().Set("content-type", "application/json")
		w.Header().Set("content-length", strconv.Itoa(len(data)))
		w.WriteHeader(200)
		w.Write(data)
		return nil
	}
	sendHTML(w, page("404", "<h1>not found</h1>", ""), 404)
	return nil
}

// fileFor is where the bytes of an image are on this machine: found by
// content in the image directory, or at the path the db recorded when no
// directory was given.
func (s *Server) fileFor(id int64) (string, string, error) {
	if s.index != nil {
		sha, err := s.store.Sha(id)
		if err != nil || sha == "" {
			return "", "", err
		}
		return s.index.Path(sha), sha, nil
	}
	path, sha, err := s.store.RecordedFile(id)
	if err != nil || path == "" {
		return "", "", err
	}
	if _, err := os.Stat(path); err != nil {
		return "", "", nil
	}
	return path, sha, nil
}

func htmlOr404(w http.ResponseWriter, text string, err error) error {
	if err != nil {
		return err
	}
	sendHTML(w, text, 200)
	return nil
}

func sendHTML(w http.ResponseWriter, text string, code int) {
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.Header().Set("content-length", strconv.Itoa(len(text)))
	w.WriteHeader(code)
	w.Write([]byte(text))
}

func sendFile(w http.ResponseWriter, r *http.Request, path, mime string) {
	f, err := os.Open(path)
	if err != nil {
		sendHTML(w, page("404", "<h1>file is gone</h1>", ""), 404)
		return
	}
	defer f.Close()
	if mime == "" {
		mime = guessMime(path)
	}
	w.Header().Set("content-type", mime)
	w.Header().Set("cache-control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, "", time.Time{}, f)
}
