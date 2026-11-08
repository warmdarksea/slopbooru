// Package fixture builds a small, deterministic recognition store and the
// pictures it describes, shaped to hit every path of the booru: more than
// a page of images, duplicates, EXIF rotation, alpha, values that need
// quoting, malformed details, failures, odd params, and a db without FTS.
//
// schema.sql and schema_fts.sql are copies of the llm-qwen pipeline's
// (scripts/ there); the booru reads that schema and nothing else.
package fixture

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

//go:embed schema_fts.sql
var schemaFTS string

// Recorded is where the classifying machine had the pictures. Nothing is
// ever there: the booru has to find them by content.
const Recorded = "/home/library/pictures/"

// File is one picture on disk, as the fixture wrote it.
type File struct {
	Name string // under Corpus
	Data []byte
	Sha  string
}

type Fixture struct {
	Corpus string // the pictures, flat, under their recorded names
	DB     string // with FTS5
	DBLike string // the same rows, no FTS5: the LIKE fallback
	Files  []File
	// Rotated is the name of the JPEG shot sideways (EXIF orientation 6):
	// stored left half red, right half blue; displayed, red on top.
	Rotated string
	// Missing is an image the db knows but no file holds.
	Missing string
	// Dup is a name whose bytes are identical to Files[0]'s.
	Dup string
}

func solid(w, h int, c color.Color) image.Image {
	im := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			im.Set(x, y, c)
		}
	}
	return im
}

func encPNG(im image.Image) []byte {
	var b bytes.Buffer
	png.Encode(&b, im)
	return b.Bytes()
}

// withOrientation splices an EXIF APP1 segment carrying tag 0x0112 into a
// JPEG, right after SOI.
func withOrientation(jpg []byte, o uint16) []byte {
	var t bytes.Buffer
	t.WriteString("MM\x00\x2a")
	binary.Write(&t, binary.BigEndian, uint32(8)) // IFD0 offset
	binary.Write(&t, binary.BigEndian, uint16(1)) // one entry
	binary.Write(&t, binary.BigEndian, uint16(0x0112))
	binary.Write(&t, binary.BigEndian, uint16(3)) // SHORT
	binary.Write(&t, binary.BigEndian, uint32(1))
	binary.Write(&t, binary.BigEndian, o)
	binary.Write(&t, binary.BigEndian, uint16(0))
	binary.Write(&t, binary.BigEndian, uint32(0)) // no next IFD
	seg := append([]byte("Exif\x00\x00"), t.Bytes()...)
	var out bytes.Buffer
	out.Write(jpg[:2])
	out.Write([]byte{0xFF, 0xE1})
	binary.Write(&out, binary.BigEndian, uint16(len(seg)+2))
	out.Write(seg)
	out.Write(jpg[2:])
	return out.Bytes()
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Build writes the fixture under dir.
func Build(dir string) (*Fixture, error) {
	fx := &Fixture{
		Corpus: filepath.Join(dir, "corpus"),
		DB:     filepath.Join(dir, "results.db"),
		DBLike: filepath.Join(dir, "results-nofts.db"),
	}
	if err := os.MkdirAll(fx.Corpus, 0o755); err != nil {
		return nil, err
	}

	// 60 small pngs: more than one page of 48
	for i := 0; i < 60; i++ {
		c := color.NRGBA{uint8(i * 4), uint8(255 - i*4), uint8(i * 37 % 256), 255}
		fx.Files = append(fx.Files, File{Name: fmt.Sprintf("pic%02d.png", i),
			Data: encPNG(solid(8+i%5, 6+i%3, c))})
	}
	// sideways jpeg: left red, right blue
	side := image.NewNRGBA(image.Rect(0, 0, 40, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 40; x++ {
			if x < 20 {
				side.Set(x, y, color.NRGBA{255, 0, 0, 255})
			} else {
				side.Set(x, y, color.NRGBA{0, 0, 255, 255})
			}
		}
	}
	var jb bytes.Buffer
	jpeg.Encode(&jb, side, &jpeg.Options{Quality: 95})
	fx.Rotated = "sideways phone shot.jpg"
	fx.Files = append(fx.Files, File{Name: fx.Rotated, Data: withOrientation(jb.Bytes(), 6)})
	// transparency, a gif, a unicode name
	fx.Files = append(fx.Files, File{Name: "alpha.png", Data: encPNG(solid(12, 12, color.NRGBA{0, 0, 0, 0}))})
	var gb bytes.Buffer
	gif.Encode(&gb, solid(10, 10, color.NRGBA{0, 200, 0, 255}), nil)
	fx.Files = append(fx.Files, File{Name: "anim.gif", Data: gb.Bytes()})
	fx.Files = append(fx.Files, File{Name: "まりさ #1.png", Data: encPNG(solid(9, 9, color.NRGBA{250, 250, 0, 255}))})
	// the same bytes as pic00 under another name: one image, two paths
	fx.Dup = "copy of pic00.png"
	fx.Files = append(fx.Files, File{Name: fx.Dup, Data: fx.Files[0].Data})
	for i := range fx.Files {
		fx.Files[i].Sha = sum(fx.Files[i].Data)
		if err := os.WriteFile(filepath.Join(fx.Corpus, fx.Files[i].Name), fx.Files[i].Data, 0o644); err != nil {
			return nil, err
		}
	}
	// a file the db never heard of
	os.WriteFile(filepath.Join(fx.Corpus, "notes.txt"), []byte("not a picture\n"), 0o644)
	fx.Missing = "gone.png"

	for _, db := range []struct {
		path string
		fts  bool
	}{{fx.DB, true}, {fx.DBLike, false}} {
		if err := fx.write(db.path, db.fts); err != nil {
			return nil, fmt.Errorf("%s: %w", db.path, err)
		}
	}
	return fx, nil
}

// Lumpy deterministic choices, so the facets have a shape.
func pick(i, n int) int { return (i*7919 + 13) % n }

func (fx *Fixture) write(path string, fts bool) error {
	os.Remove(path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	if fts {
		if _, err := db.Exec(schemaFTS); err != nil {
			return err
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ex := func(q string, args ...any) int64 {
		if err != nil {
			return 0
		}
		var r sql.Result
		r, err = tx.Exec(q, args...)
		if err != nil {
			err = fmt.Errorf("%s: %w", q, err)
			return 0
		}
		id, _ := r.LastInsertId()
		return id
	}

	// provenances, with closures
	sp := func(p string) int64 {
		return ex("INSERT INTO store_paths(path) VALUES (?)", p)
	}
	a, b, c := sp("/nix/store/aaaa-weights"), sp("/nix/store/bbbb-vllm-env"), sp("/nix/store/cccc-python3")
	cl := ex("INSERT INTO closures(sha256, n_paths) VALUES ('c1', 3)")
	for _, s := range []int64{a, b, c} {
		ex("INSERT INTO closure_paths VALUES (?, ?)", cl, s)
	}
	prov := func(model string, mode any, params string, closure any) int64 {
		return ex("INSERT INTO provenances(model, mode, model_path, runner_path, engine, closure_id, params, created) "+
			"VALUES (?, ?, ?, '/nix/store/rrrr-classify.py', 'vllm 0.33.0', ?, ?, '2026-11-01 12:00:00')",
			model, mode, "/nix/store/"+model+"-weights", closure, params)
	}
	pixai := prov("pixai", "tagger", `{"threshold": 0.35, "batch": 16}`, cl)
	qwen := prov("qwen", "fields", `{"prompt": "Describe the picture. \"Quotes\" & <angles>", `+
		`"max_pixels": 1003520, "temperature": 0.0, "top_p": 1e-05, "big": 12345678901234567890, `+
		`"exp": 1e20, "neg": -0.0, "one": 1.0, "nested": {"b": [1, 2.5, {"z": null, "a": true}], `+
		`"a": "ü 😀 \u007f"}, "empty": {}, "emptylist": []}`, cl)
	qwen2 := prov("qwen", "fields", `{"prompt": "a different prompt"}`, nil)
	ocr := prov("deepseek-ocr", "ocr", `not json at all`, cl)
	guard := prov("llamaguard", "guard", `{"describer": null}`, nil)
	weird := prov("odd model/name", nil, `{}`, nil)

	tags := []string{"solo", "1girl", "kero_(cardcaptor_sakura)", "a/b", `say "hi"`, "naïve café",
		"<script>alert(1)</script>", "100%", "smile", "outdoors", "hat", "witch_hat", "Marisa", "marisa"}
	subjects := []string{"kirisame marisa", "maid girl", "hakurei reimu"}
	ratings := []string{"safe", "questionable", "explicit"}

	ann := func(img, p int64, field, key, value string, conf any, detail any) {
		ex("INSERT OR IGNORE INTO annotations(image_id, provenance_id, field, key, value, confidence, detail) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?)", img, p, field, key, value, conf, detail)
	}

	var ids []int64
	byName := map[string]int64{}
	for i, f := range fx.Files {
		if f.Name == fx.Dup {
			continue
		}
		var w, h, mime any = nil, nil, "image/png"
		if im, _, e := image.DecodeConfig(bytes.NewReader(f.Data)); e == nil && i%11 != 5 {
			w, h = im.Width, im.Height
		}
		if i%13 == 7 {
			mime = nil
		}
		id := ex("INSERT INTO images(sha256, bytes, mime, width, height, first_seen) VALUES (?, ?, ?, ?, ?, '2026-11-01')",
			f.Sha, len(f.Data)*1000+i, mime, w, h)
		ex("INSERT INTO image_paths(image_id, path, last_seen) VALUES (?, ?, '2026-11-01')", id, Recorded+f.Name)
		ids = append(ids, id)
		byName[f.Name] = id
	}
	// the duplicate is the same image under a second path, and a third
	// from an older run
	ex("INSERT INTO image_paths(image_id, path, last_seen) VALUES (?, ?, '2026-11-02')", byName[fx.Files[0].Name], Recorded+fx.Dup)
	ex("INSERT INTO image_paths(image_id, path, last_seen) VALUES (?, ?, '2026-10-01')", byName[fx.Files[0].Name], "/mnt/old/pic00.png")
	missing := ex("INSERT INTO images(sha256, bytes, mime, width, height) VALUES (?, 1234567, 'image/png', 1, 1)",
		strings.Repeat("ab", 32))
	ex("INSERT INTO image_paths(image_id, path) VALUES (?, ?)", missing, Recorded+fx.Missing)
	ids = append(ids, missing)
	// an image with no path at all and no annotations: named by its hash
	ex("INSERT INTO images(sha256, bytes) VALUES (?, 1)", strings.Repeat("cd", 32))

	for i, id := range ids {
		if i == len(ids)-1 {
			break // the missing one gets only a failure
		}
		for k := 0; k < 1+pick(i, 6); k++ {
			t := tags[pick(i+k*3, len(tags))]
			conf := float64(pick(i*5+k, 1000)) / 1000
			if k == 3 {
				conf = 0 // a tagger's "not this"
			}
			ann(id, pixai, "tag", "", t, conf, `{"scores": {"x": 1}}`)
		}
		if i%3 == 0 {
			ann(id, pixai, "subject", "", subjects[pick(i, 3)], 0.5+float64(i%5)/10, nil)
			ann(id, qwen, "emotion", subjects[pick(i, 3)], "trying hard", 0.25, nil)
		}
		ann(id, pixai, "rating", "", ratings[pick(i, 3)], 1.0, nil)
		if i%4 == 0 {
			ann(id, qwen, "rating", "", ratings[pick(i+1, 3)], 0.7, nil)
		}
		if i%2 == 0 {
			ann(id, qwen, "description", "", fmt.Sprintf("A picture, number %d.\nIt has two lines & an <ampersand>.", i), 0.9, nil)
			ann(id, qwen2, "description", "", "the other prompt's view", nil, nil)
		}
		if i%5 == 0 {
			ann(id, pixai, "source", "", "touhou project", 0.88, nil)
			ann(id, qwen, "dimensionality", "", "2d", 0.6, nil)
		}
		if i%7 == 0 {
			ann(id, weird, "palette", "", "warm", 0.3333333333333333, nil)
		}
	}
	first, second, third := ids[0], ids[1], ids[2]
	// ocr: boxes, a malformed list that stops half way, odd coordinates
	ann(first, ocr, "text", "", "HELLO WORLD", 0.95,
		`{"boxes": [{"text": "HELLO", "box": [100, 200, 300, 250]}, {"text": "WORLD", "box": [1.5, 2, 999.99, 1000]}]}`)
	ann(second, ocr, "text", "", "half", nil,
		`{"boxes": [{"text": "ok", "box": [0, 0, 10, 10]}, {"box": [1, 2, 3]}, {"text": "never", "box": [5, 5, 6, 6]}]}`)
	ann(third, ocr, "text", "", "odd", nil,
		`{"boxes": [{"text": 5, "box": [true, 0, 500, 2.25]}, {"box": [900, 900, 100, 100]}]}`)
	ann(ids[3], ocr, "text", "", "not json", nil, `{broken`)
	// verdicts and hazards
	ann(first, guard, "verdict", "", "unsafe", 0.91234,
		`{"raw": "\nunsafe\nS1,S10", "logprobs": {"unsafe": 0.91234, "safe": 0.0876, "weird": "x", "flag": true}, `+
			`"context": "A description <b>with</b> markup"}`)
	ann(first, guard, "hazard", "", "violent crimes", nil, nil)
	ann(first, guard, "hazard", "", "hate", nil, nil)
	ann(second, guard, "verdict", "", "safe", 0.99, `null`)
	ann(third, guard, "verdict", "", "safe", 0.5, `[1, 2]`)
	ann(ids[3], guard, "verdict", "", "safe", nil, `{"raw": "", "logprobs": {}}`)
	ann(ids[4], guard, "verdict", "", "safe", 0.75, `{"raw": "safe", "logprobs": {"safe": 1}, "context": ""}`)
	ann(ids[5], guard, "verdict", "", "safe", 0.75, `not json`)

	// failures
	ex("INSERT INTO failures(image_id, provenance_id, error, at) VALUES (?, ?, ?, '2026-11-01')",
		missing, qwen, "HTTP 400: <too many tokens> & more")
	ex("INSERT INTO failures(image_id, provenance_id, error, at) VALUES (?, ?, NULL, '2026-11-01')", missing, ocr)
	ex("INSERT INTO failures(image_id, provenance_id, error, at) VALUES (?, ?, 'timeout', '2026-11-01')", first, qwen2)
	if err != nil {
		return err
	}
	return tx.Commit()
}
