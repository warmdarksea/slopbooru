// imgrec-stub stands in for the model runtimes, so pipelines test without
// a GPU. It is two programs, chosen by its arguments:
//
//	imgrec-stub serve WEIGHTS --served-model-name NAME --port N ...   # vllm serve
//	imgrec-stub SCRIPT --model PATH ... IMAGE...                       # the pixai tagger
//	imgrec-stub pixai-worker                                           # the pixai worker
//
// Replies are canned per kind of model and picked by the image's sha256,
// so a run is deterministic, and they cover what the parsers must handle:
// json that isn't, an empty caption, both OCR box formats, a guard whose
// verdict token isn't first (or is split, or has no probability at all),
// a request that fails outright, a tagger failure.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

var kinds = map[string]string{
	"qwen": "fields", "gemma": "fields", "joycaption": "caption", "toriigate": "caption",
	"deepseek-ocr": "ocr", "llamaguard": "guard",
}

func pick(sha [32]byte, n int) int { return int(sha[0]) % n }

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		serve(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "pixai-worker" {
		worker()
		return
	}
	tagger(os.Args[1:])
}

// ---- vllm serve ----------------------------------------------------------

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func serve(args []string) {
	name, port := flagValue(args, "--served-model-name"), flagValue(args, "--port")
	kind := kinds[name]
	if log := os.Getenv("IMGREC_STUB_LOG"); log != "" {
		f, _ := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		fmt.Fprintf(f, "serve %s %s\n", name, strings.Join(args, " "))
		f.Close()
	}
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	http.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		sha, ok := imageSHA(req.Messages)
		if !ok {
			http.Error(w, "no image", 400)
			return
		}
		content, logprobs, status := reply(kind, sha)
		if status != 200 {
			http.Error(w, `{"error": "stub failure"}`, status)
			return
		}
		choice := map[string]any{"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": content}}
		if logprobs != nil {
			choice["logprobs"] = logprobs
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "stub", "object": "chat.completion",
			"model": name, "choices": []any{choice}})
	})
	if err := http.ListenAndServe("127.0.0.1:"+port, nil); err != nil {
		fmt.Fprintln(os.Stderr, "imgrec-stub:", err)
		os.Exit(1)
	}
}

// imageSHA reads the image a request points at (file:// or a data url).
func imageSHA(msgs []struct {
	Content json.RawMessage `json:"content"`
}) ([32]byte, bool) {
	for _, m := range msgs {
		var parts []struct {
			Type     string `json:"type"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		if json.Unmarshal(m.Content, &parts) != nil {
			continue
		}
		for _, p := range parts {
			if p.Type != "image_url" {
				continue
			}
			u := p.ImageURL.URL
			if strings.HasPrefix(u, "file://") {
				path, _ := url.PathUnescape(strings.TrimPrefix(u, "file://"))
				data, err := os.ReadFile(path)
				if err != nil {
					return [32]byte{}, false
				}
				return sha256.Sum256(data), true
			}
			return sha256.Sum256([]byte(u)), true
		}
	}
	return [32]byte{}, false
}

func top(tokens ...any) []any { // pairs: token, logprob
	var out []any
	for i := 0; i+1 < len(tokens); i += 2 {
		out = append(out, map[string]any{"token": tokens[i], "logprob": tokens[i+1]})
	}
	return out
}

func reply(kind string, sha [32]byte) (string, any, int) {
	switch kind {
	case "fields":
		variants := []string{
			`{"description": "A witch on a broom over a moonlit forest.", "text": null, "tags": ["1girl", "witch_hat", "broom", "night", "witch_hat"], "emotion": {"overall": "cheerful", "subjects": [{"subject": "kirisame marisa", "emotion": "confident"}]}, "subjects": [{"name": "kirisame marisa", "probability": 0.72}, {"name": "blonde witch girl", "probability": 0.2}], "rating": "safe", "dimensionality": "2d", "confidence": {"description": 0.9, "text": 0.99, "tags": 0.8, "emotion": 0.6, "rating": 0.95, "dimensionality": 0.99}}`,
			`{"description": "  A street sign reading « Café » at dusk.  ", "text": "CAFÉ\nOPEN 24H", "tags": ["no_humans", "sign", "dusk", "café"], "emotion": {"overall": "calm", "subjects": []}, "subjects": [], "rating": "safe", "dimensionality": "3d", "confidence": {"description": 0.85, "text": 0.7, "tags": 0.75, "emotion": 0.5, "rating": 0.9, "dimensionality": 1.2}, "palette": ["warm", "orange"], "photo": true}`,
			`{"description": "Two figures, one partially obscured.", "text": "", "tags": [], "emotion": {"overall": "tense", "subjects": [{"subject": "", "emotion": "afraid"}, {"subject": "figure on the left", "emotion": "angry"}]}, "subjects": [{"name": "maid girl", "probability": 1.5}, {"name": "maid girl", "probability": 0.4}, {"name": "unknown", "probability": -0.1}], "rating": "questionable", "dimensionality": "2d", "confidence": {"description": 0.4, "text": 0.0, "tags": 0.3, "emotion": 0.5, "rating": 0.45, "dimensionality": 0.8}}`,
			`I'm sorry, I can't describe this image.`,
		}
		return variants[pick(sha, len(variants))], nil, 200
	case "caption":
		variants := []string{
			"A long, detailed description: a girl in a black-and-white dress stands in a library,\nreaching for a book on a high shelf. Dust floats in the light.",
			"  Photograph of a cat asleep on a windowsill — soft focus, morning light.  ",
			"",
			"FAIL",
		}
		v := variants[pick(sha, len(variants))]
		if v == "FAIL" {
			return "", nil, 500
		}
		return v, nil, 200
	case "ocr":
		variants := []string{
			"HELLO[[100, 200, 300, 250]]\nWORLD[[310, 200, 520, 251]]",
			"<|ref|>OPEN<|/ref|><|det|>[[12, 40, 330, 120]]<|/det|>\n<|ref|>24H<|/ref|><|det|>[[340, 40, 520, 120]]<|/det|>",
			"just some text, no boxes\r\nsecond line",
			"Café au lait[[5, 6, 7, 8]]\r\nnot a box line\r\nété[[900, 900, 999, 999]]",
		}
		return variants[pick(sha, len(variants))], nil, 200
	case "guard":
		// llama-guard opens with a newline; the verdict token is second
		nl := map[string]any{"token": "\n\n", "logprob": -0.0001, "top_logprobs": top("\n\n", -0.0001, "\n", -9.5)}
		switch pick(sha, 4) {
		case 0:
			return "\n\nunsafe\nS1,S10", map[string]any{"content": []any{nl,
				map[string]any{"token": "unsafe", "logprob": -0.105,
					"top_logprobs": top("unsafe", -0.105, "safe", -2.3, "un", -5.1, "under", -9.0)},
				map[string]any{"token": "\n", "logprob": -0.01, "top_logprobs": top("\n", -0.01)}}}, 200
		case 1:
			return "\n\nsafe", map[string]any{"content": []any{nl,
				map[string]any{"token": "safe", "logprob": -0.0204,
					"top_logprobs": top("safe", -0.0204, "unsafe", -3.9, " safe", -7.25)}}}, 200
		case 2:
			// the verdict split into "un" + "safe": both prefixes count
			return "\n\nunsafe\nS3", map[string]any{"content": []any{nl,
				map[string]any{"token": "un", "logprob": -0.6,
					"top_logprobs": top("un", -0.6, "safe", -0.8, "S", -6.0)}}}, 200
		default:
			// no candidate is a verdict: the probability is missing, not zero
			return "\n\nsafe", map[string]any{"content": []any{nl,
				map[string]any{"token": "\n", "logprob": -0.3, "top_logprobs": top("\n", -0.3, "\t", -2.0)}}}, 200
		}
	}
	return "", nil, 400
}

// ---- the tagger ----------------------------------------------------------

// tag is the canned contribution for one image.
func tag(img string, data []byte) map[string]any {
	sha := sha256.Sum256(data)
	switch pick(sha, 3) {
	case 0:
		return map[string]any{"image": img, "model": "pixai", "success": true,
			"fields": map[string]any{"tags": []any{"1girl", "solo", "witch_hat"},
				"subjects": []any{map[string]any{"name": "kirisame_marisa", "probability": 0.912}},
				"source":   []any{"touhou"}, "rating": "safe"},
			"detail": map[string]any{
				"rating": map[string]any{"safe": 0.81, "sensitive": 0.15, "questionable": 0.03, "explicit": 0.01},
				"scores": map[string]any{
					"tag":     map[string]any{"1girl": 0.9871, "solo": 0.9502, "witch_hat": 0.8812},
					"subject": map[string]any{"kirisame_marisa": 0.9123},
					"source":  map[string]any{"touhou": 0.9655},
					"rating":  map[string]any{"safe": 0.81, "sensitive": 0.15, "questionable": 0.03, "explicit": 0.01}}}}
	case 1:
		return map[string]any{"image": img, "model": "pixai", "success": true,
			"fields": map[string]any{"tags": []any{"no_humans", "scenery"}, "subjects": []any{},
				"source": []any{}, "rating": "sensitive"},
			"detail": map[string]any{
				"rating": map[string]any{"safe": 0.3, "sensitive": 0.6, "questionable": 0.07, "explicit": 0.03},
				"scores": map[string]any{"tag": map[string]any{"no_humans": 0.99, "scenery": 0.7},
					"subject": map[string]any{}, "source": map[string]any{},
					"rating": map[string]any{"safe": 0.3, "sensitive": 0.6, "questionable": 0.07, "explicit": 0.03}}}}
	default:
		return map[string]any{"image": img, "model": "pixai", "success": false,
			"error": "cannot identify image file"}
	}
}

// tagger is pixai_tag.py: every image named on the command line, one json
// line each, then exit.
func tagger(args []string) {
	var images []string
	for i := 1; i < len(args); i++ { // args[0] is the script
		if strings.HasPrefix(args[i], "--") {
			i++
			continue
		}
		images = append(images, args[i])
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	for _, img := range images {
		data, err := os.ReadFile(img)
		if err != nil {
			enc.Encode(map[string]any{"image": img, "model": "pixai", "success": false, "error": err.Error()})
			continue
		}
		enc.Encode(tag(img, data))
	}
}

// worker is runners/pixai_worker.py: {"ready": true}, then one answer per
// {"id", "path"} request line, until stdin closes.
func worker() {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.Encode(map[string]any{"ready": true})
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req struct {
			ID   int64  `json:"id"`
			Path string `json:"path"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		var out map[string]any
		if data, err := os.ReadFile(req.Path); err != nil {
			out = map[string]any{"image": req.Path, "model": "pixai", "success": false, "error": err.Error()}
		} else {
			out = tag(req.Path, data)
		}
		out["id"] = req.ID
		enc.Encode(out)
	}
}
