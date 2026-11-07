package main

// Editing by hand: add and remove values, upload and delete images.
//
// Manual edits are a source like any model: rows under one provenance,
// model "manual". Adding a value writes a manual row (confidence 1).
// Removing one writes a manual row with confidence 0 -- a person saying
// "not this" -- which hides that value on that image whoever said it:
// every browsing query reads `visible` instead of the table. The models'
// rows stay in the db, so the provenance and stats pages still show what
// each model said, and "restore" deletes the manual zero.
//
// Off unless started with --edit: a booru served on 0.0.0.0 is reachable
// by the whole network. Every edit is a POST that must come from this
// booru's own pages (Origin/Referer), so another site can't drive it.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// visible is annotations minus everything removed by hand (and minus the
// removal markers themselves, which match their own condition). With no
// manual rows it is exactly the table, which keeps the pages identical to
// the python booru's.
const visible = "(SELECT * FROM annotations WHERE NOT EXISTS (" +
	"SELECT 1 FROM annotations m WHERE m.provenance_id IN " +
	"(SELECT id FROM provenances WHERE model = 'manual') " +
	"AND m.confidence = 0 AND m.image_id = annotations.image_id " +
	"AND m.field = annotations.field AND m.key = annotations.key " +
	"AND m.value = annotations.value COLLATE NOCASE))"

// editMode is --edit: it adds the edit controls to the pages.
var editMode bool

const editCSS = `
.ed { display: inline; }
.ed button { padding: 0 6px; margin-left: 6px; font-size: 12px; line-height: 1.4; }
.editbox { margin-top: 24px; padding: 12px 14px; border: 1px solid var(--line); border-radius: 10px; }
.editbox h2 { margin-top: 0; }
.editbox form.row { display: flex; gap: 6px; flex-wrap: wrap; margin: 6px 0; }
.editbox input[type=text], .editbox select { font: inherit; padding: 6px 9px; color: var(--fg);
  background: var(--card); border: 1px solid var(--line); border-radius: 7px; }
.editbox input[type=text] { flex: 1 1 180px; min-width: 0; }
button.danger { color: var(--warn); }
`

// fields offered in the add form; any field can be removed
var addableFields = []string{"tag", "subject", "source", "rating", "dimensionality", "description", "text"}

var errEditing = errors.New("editing is off: start the booru with --edit")

// ---- the store side ------------------------------------------------------

func (s *Store) manualProvenance() (int64, error) {
	var id int64
	err := s.db.QueryRow("SELECT id FROM provenances WHERE model = 'manual' AND mode = 'manual' " +
		"ORDER BY id LIMIT 1").Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	r, err := s.db.Exec("INSERT INTO provenances(model, mode, engine, params) " +
		"VALUES ('manual', 'manual', 'by hand', '{}')")
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

func cleanValue(field, value string) string {
	value = strings.TrimSpace(value)
	if field == "tag" { // booru vocabulary: lowercase_with_underscores
		value = strings.Join(strings.Fields(value), "_")
	}
	return value
}

// AddValue says, by hand, that this image has field=value (and undoes a
// removal of it, if there was one).
func (s *Store) AddValue(imageID int64, field, key, value string) error {
	pid, err := s.manualProvenance()
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM annotations WHERE image_id = ? AND provenance_id = ? AND field = ? "+
		"AND key = ? AND value = ? COLLATE NOCASE AND confidence = 0", imageID, pid, field, key, value); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO annotations(image_id, provenance_id, field, key, value, confidence) "+
		"VALUES (?, ?, ?, ?, ?, 1.0) ON CONFLICT(image_id, provenance_id, field, key, value) "+
		"DO UPDATE SET confidence = 1.0", imageID, pid, field, key, value); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveValue hides field=value on this image, whoever said it.
func (s *Store) RemoveValue(imageID int64, field, key, value string) error {
	pid, err := s.manualProvenance()
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM annotations WHERE image_id = ? AND provenance_id = ? AND field = ? "+
		"AND key = ? AND value = ? COLLATE NOCASE AND confidence > 0", imageID, pid, field, key, value); err != nil {
		return err
	}
	// only a model's value needs hiding; a manual one is simply gone now
	var others int
	if err := tx.QueryRow("SELECT COUNT(*) FROM annotations WHERE image_id = ? AND provenance_id != ? "+
		"AND field = ? AND key = ? AND value = ? COLLATE NOCASE", imageID, pid, field, key, value).Scan(&others); err != nil {
		return err
	}
	if others > 0 {
		if _, err := tx.Exec("INSERT INTO annotations(image_id, provenance_id, field, key, value, confidence) "+
			"VALUES (?, ?, ?, ?, ?, 0) ON CONFLICT(image_id, provenance_id, field, key, value) "+
			"DO UPDATE SET confidence = 0", imageID, pid, field, key, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RestoreValue undoes a removal.
func (s *Store) RestoreValue(imageID int64, field, key, value string) error {
	_, err := s.db.Exec("DELETE FROM annotations WHERE image_id = ? AND field = ? AND key = ? "+
		"AND value = ? COLLATE NOCASE AND confidence = 0 AND provenance_id IN "+
		"(SELECT id FROM provenances WHERE model = 'manual')", imageID, field, key, value)
	return err
}

// DeleteImage removes an image and everything said about it from the db
// (foreign keys cascade). The file on disk is left alone.
func (s *Store) DeleteImage(imageID int64) error {
	_, err := s.db.Exec("DELETE FROM images WHERE id = ?", imageID)
	return err
}

// Removed lists what was removed by hand from this image.
func (s *Store) Removed(imageID any) ([]Row, error) {
	return s.query("SELECT a.field, a.key, a.value FROM annotations a JOIN provenances p "+
		"ON p.id = a.provenance_id WHERE p.model = 'manual' AND a.confidence = 0 AND a.image_id = ? "+
		"ORDER BY a.field, a.value", imageID)
}

var imageMimes = map[string]string{"jpeg": "image/jpeg", "png": "image/png", "gif": "image/gif",
	"webp": "image/webp", "bmp": "image/bmp", "tiff": "image/tiff"}

// AddImage records a file that is already in place on disk.
func (s *Store) AddImage(path, sha string, size int64, format string, w, h int) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO images(sha256, bytes, mime, width, height) VALUES (?, ?, ?, ?, ?) "+
		"ON CONFLICT(sha256) DO NOTHING", sha, size, imageMimes[format], w, h); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRow("SELECT id FROM images WHERE sha256 = ?", sha).Scan(&id); err != nil {
		return 0, err
	}
	// recorded with '/' separators, like every other path in the db
	if _, err := tx.Exec("INSERT OR IGNORE INTO image_paths(image_id, path) VALUES (?, ?)",
		id, filepath.ToSlash(path)); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// ---- pages ---------------------------------------------------------------

func hidden(name, value string) string {
	return `<input type="hidden" name="` + name + `" value="` + e(value) + `">`
}

func removeButton(imageID int64, field, key, value string) string {
	return fmt.Sprintf(`<form class="ed" method="post" action="/image/%d/remove">`, imageID) +
		hidden("field", field) + hidden("key", key) + hidden("value", value) +
		`<button type="submit" title="remove from this image">×</button></form>`
}

// removedNote is shown in every mode: hidden values should not vanish
// without a word. The restore buttons only with --edit.
func (s *Store) removedNote(imageID int64) (string, error) {
	removed, err := s.Removed(imageID)
	if err != nil || len(removed) == 0 {
		return "", err
	}
	if !editMode {
		return fmt.Sprintf(`<p class="muted">%d value(s) removed by hand are not shown.</p>`, len(removed)), nil
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("<h2>removed by hand <span class='muted'>(%d)</span></h2><table>", len(removed)))
	for _, r := range removed {
		f, k, v := pystr(r["field"]), pystr(r["key"]), pystr(r["value"])
		label := e(v)
		if k != "" {
			label = `<span class="muted">` + e(k) + `:</span> ` + label
		}
		b.WriteString(`<tr><td><span class="badge">` + e(f) + `</span></td><td>` + label + `</td><td>` +
			fmt.Sprintf(`<form class="ed" method="post" action="/image/%d/restore">`, imageID) +
			hidden("field", f) + hidden("key", k) + hidden("value", v) +
			`<button type="submit">restore</button></form></td></tr>`)
	}
	b.WriteString("</table>")
	return b.String(), nil
}

func editPanel(imageID int64) string {
	var opts strings.Builder
	for _, f := range addableFields {
		opts.WriteString(`<option>` + f + `</option>`)
	}
	return `<div class="editbox"><h2>edit</h2>` +
		fmt.Sprintf(`<form class="row" method="post" action="/image/%d/add">`, imageID) +
		`<select name="field" aria-label="field">` + opts.String() + `</select>` +
		`<input type="text" name="value" placeholder="value, e.g. witch_hat" required aria-label="value">` +
		`<button type="submit">add</button></form>` +
		fmt.Sprintf(`<form class="row" method="post" action="/image/%d/delete" `, imageID) +
		`onsubmit="return confirm('Delete this image and everything said about it from the db? The file stays on disk.')">` +
		`<button type="submit" class="danger">delete image</button></form></div>`
}

func uploadPage(canUpload bool) string {
	if !canUpload {
		return page("upload", "<h1>upload</h1><p class='muted'>uploads need a picture folder: "+
			"start the booru with --images DIR as well as --edit.</p>", "")
	}
	return page("upload", `<h1>upload</h1><div class="editbox">`+
		`<form class="row" method="post" action="/upload" enctype="multipart/form-data">`+
		`<input type="file" name="files" accept="image/*" multiple required>`+
		`<button type="submit">upload</button></form>`+
		`<p class="muted">saved into the picture folder and added to the db, untagged. `+
		`A picture already in the db is not added twice.</p></div>`, "")
}

// ---- requests ------------------------------------------------------------

// sameOrigin: the POST came from a page of this booru.
func sameOrigin(r *http.Request) bool {
	for _, h := range []string{r.Header.Get("Origin"), r.Header.Get("Referer")} {
		if h == "" || h == "null" {
			continue
		}
		u, err := url.Parse(h)
		return err == nil && u.Host == r.Host
	}
	return false
}

func redirect(w http.ResponseWriter, to string) {
	w.Header().Set("Location", to)
	w.WriteHeader(http.StatusSeeOther)
}

func (s *Server) post(w http.ResponseWriter, r *http.Request) error {
	if !editMode {
		sendHTML(w, page("403", "<h1>editing is off</h1><p>start the booru with --edit.</p>", ""), 403)
		return nil
	}
	if !sameOrigin(r) {
		sendHTML(w, page("403", "<h1>refused</h1><p>edits must come from this booru's own pages.</p>", ""), 403)
		return nil
	}
	parts := pathParts(r)
	if len(parts) == 1 && parts[0] == "upload" {
		return s.upload(w, r)
	}
	if len(parts) != 3 || parts[0] != "image" {
		sendHTML(w, page("404", "<h1>not found</h1>", ""), 404)
		return nil
	}
	id, err := pyint(parts[1])
	if err != nil {
		return err
	}
	if err := r.ParseForm(); err != nil {
		return errBadRequest
	}
	field := strings.TrimSpace(r.PostForm.Get("field"))
	key := r.PostForm.Get("key")
	value := cleanValue(field, r.PostForm.Get("value"))
	switch parts[2] {
	case "delete":
		if err := s.store.DeleteImage(id); err != nil {
			return err
		}
		redirect(w, "/")
		return nil
	case "add", "remove", "restore":
		if field == "" || value == "" {
			return errBadRequest
		}
		ops := map[string]func(int64, string, string, string) error{
			"add": s.store.AddValue, "remove": s.store.RemoveValue, "restore": s.store.RestoreValue}
		if err := ops[parts[2]](id, field, key, value); err != nil {
			return err
		}
		redirect(w, "/image/"+strconv.FormatInt(id, 10))
		return nil
	}
	sendHTML(w, page("404", "<h1>not found</h1>", ""), 404)
	return nil
}

const maxUpload = 1 << 30 // one request, all files together

func (s *Server) upload(w http.ResponseWriter, r *http.Request) error {
	if s.imagesDir == "" {
		sendHTML(w, uploadPage(false), 400)
		return nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	mr, err := r.MultipartReader()
	if err != nil {
		return errBadRequest
	}
	var ids []int64
	var problems []string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errBadRequest
		}
		if part.FormName() != "files" || part.FileName() == "" {
			continue
		}
		id, err := s.saveUpload(part.FileName(), part)
		if err != nil {
			problems = append(problems, e(part.FileName())+": "+e(err.Error()))
			continue
		}
		ids = append(ids, id)
	}
	if len(problems) > 0 {
		sendHTML(w, page("upload", fmt.Sprintf("<h1>uploaded %d, refused %d</h1><p class='mono'>%s</p>"+
			"<p><a href='/'>back</a></p>", len(ids), len(problems), strings.Join(problems, "<br>")), ""), 400)
		return nil
	}
	if len(ids) == 1 {
		redirect(w, "/image/"+strconv.FormatInt(ids[0], 10))
	} else {
		redirect(w, "/")
	}
	return nil
}

// safeName is the uploaded file's own name, reduced to a plain file name.
func safeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		name = "upload"
	}
	return name
}

func (s *Server) saveUpload(name string, src io.Reader) (int64, error) {
	tmp, err := os.CreateTemp(s.imagesDir, ".upload-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name()) // a no-op once it has been renamed
	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), src)
	if err == nil {
		_, err = tmp.Seek(0, io.SeekStart)
	}
	if err != nil {
		tmp.Close()
		return 0, err
	}
	cfg, format, err := image.DecodeConfig(tmp)
	tmp.Close()
	if err != nil {
		return 0, fmt.Errorf("not a picture this booru can read (%v)", err)
	}
	sha := hex.EncodeToString(h.Sum(nil))

	// the same bytes already on disk: record nothing new on disk
	dest := ""
	if s.index != nil {
		dest = s.index.Path(sha)
	}
	if dest == "" {
		base := safeName(name)
		ext := filepath.Ext(base)
		stem := strings.TrimSuffix(base, ext)
		for i := 0; ; i++ {
			dest = filepath.Join(s.imagesDir, base)
			if i > 0 {
				dest = filepath.Join(s.imagesDir, fmt.Sprintf("%s-%d%s", stem, i, ext))
			}
			if _, err := os.Stat(dest); os.IsNotExist(err) {
				break
			}
		}
		if err := os.Rename(tmp.Name(), dest); err != nil {
			return 0, err
		}
		if s.index != nil {
			s.index.Add(sha, dest)
		}
	}
	return s.store.AddImage(dest, sha, size, format, cfg.Width, cfg.Height)
}
