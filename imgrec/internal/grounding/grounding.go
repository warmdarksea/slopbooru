// Package grounding reads DeepSeek-OCR's grounding output: plain text plus
// bounding boxes (coordinates normalized 0-999, so they map onto the
// original image whatever size the model saw). A port of
// src/py-pipeline/grounding.py; the two must agree.
package grounding

import (
	"regexp"
	"strconv"
	"strings"

	"slopbooru/imgrec/internal/pyjson"
)

var (
	// <|ref|>text<|/ref|><|det|>[[x1, y1, x2, y2]]<|/det|>
	full = regexp.MustCompile(`(?s)<\|ref\|>(.*?)<\|/ref\|><\|det\|>\s*(\[.*?\])\s*<\|/det\|>`)
	// what vllm's chat api returns (special tokens stripped): one region a
	// line, "text[[x1, y1, x2, y2]]"
	compact = regexp.MustCompile(`^(.*?)\s*\[\[(\d+),\s*(\d+),\s*(\d+),\s*(\d+)\]\]\s*$`)
)

// Box is one region: its text and [x1, y1, x2, y2] (any json, as parsed).
type Box struct {
	Text string
	Box  any
}

// Value is the box as the store writes it into detail.
func (b Box) Value() []pyjson.KV {
	return []pyjson.KV{{K: "box", V: b.Box}, {K: "text", V: b.Text}}
}

// Parse returns the plain text and the boxes (nil when there is no markup).
func Parse(raw string) (string, []Box) {
	var boxes []Box
	for _, m := range full.FindAllStringSubmatch(raw, -1) {
		coords, err := pyjson.Decode(m[2])
		if err != nil {
			coords = nil
		}
		// usually [[x1,y1,x2,y2]]: flatten the single-box case
		if l, ok := coords.([]any); ok && len(l) == 1 {
			if inner, ok := l[0].([]any); ok {
				coords = inner
			}
		}
		boxes = append(boxes, Box{Text: PyStrip(m[1]), Box: coords})
	}
	if len(boxes) == 0 {
		for _, line := range splitlines(raw) {
			m := compact.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			c := make([]any, 4)
			for i := range 4 {
				n, _ := strconv.ParseInt(m[2+i], 10, 64)
				c[i] = n
			}
			boxes = append(boxes, Box{Text: PyStrip(m[1]), Box: c})
		}
	}
	if len(boxes) == 0 {
		return PyStrip(raw), nil
	}
	var texts []string
	for _, b := range boxes {
		if b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n"), boxes
}

// splitlines is str.splitlines() for the separators a model can emit.
func splitlines(s string) []string {
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\v", "\n", "\f", "\n",
		"\x1c", "\n", "\x1d", "\n", "\x1e", "\n", "\x85", "\n", " ", "\n", " ", "\n").Replace(s)
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// PyStrip is str.strip(): python counts \x1c-\x1f as whitespace, Go does not.
func PyStrip(s string) string {
	return strings.TrimFunc(s, IsPySpace)
}

func IsPySpace(r rune) bool {
	switch {
	case r >= '\t' && r <= '\r', r >= 0x1c && r <= 0x1f, r == ' ', r == 0x85, r == 0xa0,
		r == 0x1680, r >= 0x2000 && r <= 0x200a, r == 0x2028, r == 0x2029, r == 0x202f,
		r == 0x205f, r == 0x3000:
		return true
	}
	return false
}
