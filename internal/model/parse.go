// Package model turns a model's reply into a contribution: what it said,
// as fields, before the store flattens it into rows.
//
// One parser per kind of model (fields, caption, ocr, guard, tagger). A
// port of src/py-pipeline/infer.py's contribution(); the two must produce
// the same contribution for the same reply, which the replay parity test
// checks.
package model

import (
	"encoding/json"
	"math"
	"regexp"
	"strings"

	"imgrec/internal/grounding"
	"imgrec/internal/pyjson"
)

// Contribution is one model's answer about one image.
type Contribution struct {
	Image   string // the image, as the caller names it
	Model   string // the model's short name
	Success bool
	Fields  map[string]any // what it said
	Detail  map[string]any // what backs it (scores, raw answer, boxes...)
	Error   string
}

// GuardHazards is llama-guard's hazard taxonomy (its own chat template).
var GuardHazards = map[string]string{
	"S1": "violent crimes", "S2": "non-violent crimes", "S3": "sex crimes",
	"S4": "child exploitation", "S5": "defamation", "S6": "specialized advice",
	"S7": "privacy", "S8": "intellectual property", "S9": "indiscriminate weapons",
	"S10": "hate", "S11": "self-harm", "S12": "sexual content", "S13": "elections",
	"S14": "code interpreter abuse",
}

var guardCode = regexp.MustCompile(`\bS(1[0-4]|[1-9])\b`)

// GuardLookahead is how many generated tokens to scan for the verdict:
// llama-guard opens with a newline, so it is rarely the first.
const GuardLookahead = 4

// ChatAnswer reads an OpenAI chat-completions reply: (content, logprobs).
// An error means the reply is not json at all, so it is no answer.
func ChatAnswer(raw string) (string, any, error) {
	v, err := pyjson.Decode(raw)
	if err != nil {
		return "", nil, err
	}
	resp, _ := v.(map[string]any)
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return "", nil, nil
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	content, _ := msg["content"].(string)
	return content, choice["logprobs"], nil
}

// Parse is a reply from a chat model of this kind -> its contribution.
// context is the text a guard request carried ("" otherwise).
func Parse(kind, name, image, content string, logprobs any, context string) Contribution {
	ok := Contribution{Image: image, Model: name, Success: true}
	fail := func(why string) Contribution {
		return Contribution{Image: image, Model: name, Error: why}
	}
	if kind == "fields" {
		v, err := pyjson.Decode(content)
		fields, isMap := v.(map[string]any)
		if err != nil || !isMap {
			c := fail("model output did not parse as json")
			c.Detail = map[string]any{"raw": content}
			return c
		}
		ok.Fields = fields
		return ok
	}
	if content == "" {
		return fail("empty response")
	}
	switch kind {
	case "caption":
		ok.Fields = map[string]any{"description": content}
	case "ocr":
		ok.Fields = map[string]any{"text": content}
	case "guard":
		call := Verdict(content)
		hz := Hazards(content)
		hazards := make([]any, len(hz))
		for i, h := range hz {
			hazards[i] = h
		}
		ok.Fields = map[string]any{"verdict": call, "hazards": hazards}
		ok.Detail = map[string]any{"raw": grounding.PyStrip(content)}
		if context != "" {
			ok.Detail["context"] = context
		}
		if probs := GuardProbability(logprobs); probs != nil {
			// the probability of the verdict it gave, and only the verdict's
			ok.Fields["confidence"] = map[string]any{"verdict": probs[call]}
			ok.Detail["logprobs"] = map[string]any{"safe": probs["safe"], "unsafe": probs["unsafe"]}
		} else if logprobs != nil && pyjson.Truthy(logprobs) {
			ok.Detail["logprobs_unparsed"] = guardTokens(logprobs)
		}
	default:
		return fail("no such kind: " + kind)
	}
	return ok
}

// ParseTagger is one json line from the tagger -> its contribution, with
// the image renamed to the caller's (a cached line may name another path).
func ParseTagger(image, line string) (Contribution, bool) {
	v, err := pyjson.Decode(line)
	m, isMap := v.(map[string]any)
	if err != nil || !isMap {
		return Contribution{}, false
	}
	c := Contribution{Image: image}
	c.Model, _ = m["model"].(string)
	c.Success = pyjson.Truthy(m["success"])
	c.Fields, _ = m["fields"].(map[string]any)
	c.Detail, _ = m["detail"].(map[string]any)
	if e, ok := m["error"]; ok && e != nil {
		c.Error = pyjson.Str(e)
	}
	return c, true
}

// Verdict: llama-guard answers "safe" or "unsafe\nS<n>[,S<m>...]".
func Verdict(content string) string {
	if strings.Contains(content, "unsafe") {
		return "unsafe"
	}
	return "safe"
}

// Hazards: the violated categories, as labels, in order, without repeats.
func Hazards(content string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range guardCode.FindAllStringSubmatch(content, -1) {
		key := "S" + m[1]
		label, ok := GuardHazards[key]
		if !ok {
			label = key
		}
		if !seen[label] {
			seen[label] = true
			out = append(out, label)
		}
	}
	return out
}

func guardBucket(word string) string {
	w := strings.ToLower(grounding.PyStrip(word))
	switch {
	case w == "":
		return ""
	case strings.HasPrefix("unsafe", w) || strings.HasPrefix(w, "unsafe"):
		return "unsafe"
	case strings.HasPrefix("safe", w) || strings.HasPrefix(w, "safe"):
		return "safe"
	}
	return ""
}

func steps(logprobs any) []any {
	m, _ := logprobs.(map[string]any)
	s, _ := m["content"].([]any)
	if len(s) > GuardLookahead {
		s = s[:GuardLookahead]
	}
	return s
}

// GuardProbability: {"safe": p, "unsafe": p} from the first token that
// carries the verdict, or nil -- never zeros -- when none does.
func GuardProbability(logprobs any) map[string]float64 {
	for _, st := range steps(logprobs) {
		step, _ := st.(map[string]any)
		p := map[string]float64{"safe": 0, "unsafe": 0}
		cands, _ := step["top_logprobs"].([]any)
		for _, c := range cands {
			cand, _ := c.(map[string]any)
			tok, _ := cand["token"].(string)
			key := guardBucket(tok)
			if key == "" {
				continue
			}
			lp, isNum := cand["logprob"].(json.Number)
			if !isNum {
				continue
			}
			f, err := lp.Float64()
			if err != nil {
				continue
			}
			p[key] += math.Exp(f)
		}
		if p["safe"] != 0 || p["unsafe"] != 0 {
			return p
		}
	}
	return nil
}

// guardTokens: the first few tokens, for a verdict whose probability we
// missed, so the failure is diagnosable from the store.
func guardTokens(logprobs any) any {
	var out []any
	for _, st := range steps(logprobs) {
		step, _ := st.(map[string]any)
		if t, ok := step["token"]; ok && t != nil {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
