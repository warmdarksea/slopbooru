package main

// import-py-cache: every answer in a python pipeline db's cache_* tables,
// parsed by the Go parsers and written into a store.
//
// The cache keys an answer by the bytes the model was shown. For a
// downscaled stand-in that is not the original's sha256, and Go can't
// reproduce Pillow's bytes to find out which original it was: those
// answers are reported and skipped (--standins maps them, from a file
// the python side writes).

import (
	"bufio"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	"slopbooru/imgrec/internal/images"
	"slopbooru/imgrec/internal/model"
	"slopbooru/imgrec/internal/pyjson"
	"slopbooru/imgrec/internal/store"
)

type manifestModel struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	Closure string `json:"closure"`
}

func readManifest(path string) (map[string]manifestModel, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	v, err := pyjson.Decode(string(raw))
	if err != nil {
		return nil, err
	}
	top, _ := v.(map[string]any)
	models, _ := top["models"].(map[string]any)
	out := map[string]manifestModel{}
	for name, m := range models {
		mm, _ := m.(map[string]any)
		s := func(k string) string { x, _ := mm[k].(string); return x }
		out[name] = manifestModel{Path: s("path"), Mode: s("mode"), Closure: s("closure")}
	}
	return out, nil
}

func readClosure(file string) []string {
	if file == "" {
		return nil
	}
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if p := strings.TrimSpace(sc.Text()); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// guardContext is the text part of a guard request, from the cached question.
func guardContext(material any) string {
	m, _ := material.(map[string]any)
	req, _ := m["request"].(map[string]any)
	msgs, _ := req["messages"].([]any)
	for _, msg := range msgs {
		mm, _ := msg.(map[string]any)
		parts, _ := mm["content"].([]any)
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			if pm["type"] == "text" {
				t, _ := pm["text"].(string)
				return t
			}
		}
	}
	return ""
}

func importPyCache(args []string) error {
	fs := flag.NewFlagSet("import-py-cache", flag.ExitOnError)
	pyDB := fs.String("py-db", "", "the python pipeline's db (with cache_* tables)")
	dir := fs.String("images", "", "the directory the answers are about")
	manifest := fs.String("manifest", "", "models.json: each model's kind, weights and closure")
	out := fs.String("db", "", "the store to write")
	fs.Parse(args)
	if *pyDB == "" || *dir == "" || *manifest == "" || *out == "" {
		fs.Usage()
		os.Exit(2)
	}
	models, err := readManifest(*manifest)
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	bySha, _, err := images.Index(*dir)
	if err != nil {
		return err
	}
	src, err := sql.Open("sqlite", "file:"+*pyDB+"?mode=ro")
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := store.Open(*out)
	if err != nil {
		return err
	}
	defer dst.Close()

	rows, err := src.Query("SELECT a.model, a.path, a.image, a.output, q.material " +
		"FROM cache_answers a JOIN cache_questions q ON q.id = a.question ORDER BY a.id")
	if err != nil {
		return err
	}
	defer rows.Close()
	var answers, written, skipped int
	for rows.Next() {
		var name, weights, imageSha, output, material string
		if err := rows.Scan(&name, &weights, &imageSha, &output, &material); err != nil {
			return err
		}
		answers++
		mm, ok := models[name]
		if !ok {
			fmt.Fprintf(os.Stderr, "imgrec: %s is not in the manifest; skipped\n", name)
			skipped++
			continue
		}
		im, ok := bySha[imageSha]
		if !ok {
			skipped++ // a stand-in, or a file no longer in the directory
			continue
		}
		question, _ := pyjson.Decode(material)
		var c model.Contribution
		if mm.Mode == "tagger" {
			if c, ok = model.ParseTagger(im.Path, output); !ok {
				skipped++
				continue
			}
		} else {
			content, logprobs, err := model.ChatAnswer(output)
			if err != nil {
				skipped++
				continue
			}
			ctx := ""
			if mm.Mode == "guard" {
				ctx = guardContext(question)
			}
			c = model.Parse(mm.Mode, name, im.Path, content, logprobs, ctx)
		}
		prov := store.Provenance{Model: name, Mode: mm.Mode, ModelPath: weights,
			RunnerPath: "imgrec import-py-cache", Closure: readClosure(mm.Closure), Params: question}
		n, err := dst.Write(im, prov, mm.Mode, c)
		if err != nil {
			return err
		}
		written += n
	}
	if err := rows.Err(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "imgrec: %d cached answer(s), %d annotation row(s) written, %d skipped -> %s\n",
		answers, written, skipped, *out)
	return nil
}
