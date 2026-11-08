// Package spec: every parameter that decides a model's answer, as data.
//
// A spec is a json file in specs/ (prompt, schema, sampling, server
// settings, input limits) plus what the flake knows: the weights' store
// path, the closure behind them and, for the tagger, the worker script.
// The cache key falls out of it:
//
//	key = sha256(canonical(question) | input sha256 | context)
//
// where the question is everything that changes the answer -- weights,
// kind, input limits, server settings, the request -- and nothing that
// doesn't: the runner's gpu share and concurrency, the model's name.
package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Version changes only when the meaning of a key changes.
const Version = 1

type Spec struct {
	Name    string         `json:"name"`
	Kind    string         `json:"kind"` // fields, caption, ocr, guard, tagger
	Input   map[string]any `json:"input"`
	Server  map[string]any `json:"server"`  // output-affecting server settings
	Runner  map[string]any `json:"runner"`  // operational: not in the key
	Request map[string]any `json:"request"` // the request minus the image
	Output  map[string]any `json:"output"`  // json schema of what it produces
	Needs   *Needs         `json:"needs,omitempty"`

	// from the flake's manifest
	Weights string   `json:"weights,omitempty"`
	Closure []string `json:"closure,omitempty"`
	Worker  string   `json:"worker,omitempty"` // the tagger's script
}

// Needs is a model whose answer this one reads (llamaguard: a description).
type Needs struct {
	Model string `json:"model"`
	Field string `json:"field"`
	Limit int    `json:"limit"`
}

func decode(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber() // numbers keep their literal: 0.2 hashes as 0.2
	return d.Decode(v)
}

// LoadDir reads every specs/*.json.
func LoadDir(dir string) (map[string]*Spec, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	out := map[string]*Spec{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var s Spec
		if err := decode(data, &s); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if s.Name == "" || s.Kind == "" {
			return nil, fmt.Errorf("%s: a spec needs a name and a kind", f)
		}
		out[s.Name] = &s
	}
	return out, nil
}

// Question is the part of a spec that decides the answer.
func (s *Spec) Question() map[string]any {
	q := map[string]any{"v": Version, "kind": s.Kind, "weights": s.Weights,
		"input": s.Input, "server": s.Server, "request": s.Request}
	if s.Worker != "" {
		q["worker"] = s.Worker
	}
	return q
}

// Hash identifies the question (a provenance, in store terms).
func (s *Spec) Hash() string { return sum(Canonical(s.Question())) }

// Key is one question about one input. context is the text a guard
// request carries ("" for every other kind).
func (s *Spec) Key(inputSHA, context string) string {
	return sum(s.Hash() + "\x00" + inputSHA + "\x00" + context)
}

// Seed: per input, so a re-run reproduces the answer instead of drawing
// a new sample.
func Seed(inputSHA string) int64 {
	if len(inputSHA) < 8 {
		return 0
	}
	n, _ := strconv.ParseUint(inputSHA[:8], 16, 32)
	return int64(n & 0x7fffffff)
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// Canonical is the one serialization: sorted keys, no padding, numbers as
// their literal, no html escaping.
func Canonical(v any) string {
	var b strings.Builder
	canon(&b, v)
	return b.String()
}

func canon(b *strings.Builder, v any) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			canon(b, k)
			b.WriteByte(':')
			canon(b, x[k])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, it := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			canon(b, it)
		}
		b.WriteByte(']')
	default:
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.Encode(x)
		b.WriteString(strings.TrimSuffix(buf.String(), "\n"))
	}
}

// RequestBody is the chat-completions request for one input: the spec's
// request with the image, the context and the seed filled in.
func (s *Spec) RequestBody(imageURL, context, inputSHA string) map[string]any {
	body := deepCopy(s.Request).(map[string]any)
	body["model"] = s.Name
	body["seed"] = Seed(inputSHA)
	msgs, _ := body["messages"].([]any)
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		parts, _ := mm["content"].([]any)
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			switch pm["type"] {
			case "image_url":
				pm["image_url"] = map[string]any{"url": imageURL}
			case "text":
				if pm["text"] == "{context}" {
					pm["text"] = context
				}
			}
		}
	}
	return body
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, it := range x {
			m[k] = deepCopy(it)
		}
		return m
	case []any:
		l := make([]any, len(x))
		for i, it := range x {
			l[i] = deepCopy(it)
		}
		return l
	}
	return v
}

// ServeArgs is the vllm command line for a spec (after `vllm serve`).
func (s *Spec) ServeArgs(port int, mediaPath string) []string {
	args := []string{s.Weights, "--served-model-name", s.Name,
		"--host", "127.0.0.1", "--port", strconv.Itoa(port),
		"--allowed-local-media-path", mediaPath}
	keys := make([]string, 0, len(s.Server))
	for k := range s.Server {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		flag := "--" + strings.ReplaceAll(k, "_", "-")
		switch v := s.Server[k].(type) {
		case map[string]any, []any:
			args = append(args, flag, Canonical(v))
		case bool:
			if v {
				args = append(args, flag)
			}
		default:
			args = append(args, flag, strings.Trim(Canonical(v), `"`))
		}
	}
	if gpu, ok := s.Runner["gpu"].(json.Number); ok {
		args = append(args, "--gpu-memory-utilization", gpu.String())
	}
	if seqs, ok := s.Runner["max_num_seqs"].(json.Number); ok {
		args = append(args, "--max-num-seqs", seqs.String())
	}
	if eager, _ := s.Runner["enforce_eager"].(bool); eager {
		args = append(args, "--enforce-eager")
	}
	return args
}
