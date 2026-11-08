// Package pyjson formats values the way Python's json and str() do.
//
// The store's detail column is written by python as
// json.dumps(detail, sort_keys=True), and annotation values as str(v);
// the Go importer must produce the same bytes for the same answer, or a
// store rebuilt in Go would differ from one built in python. Copied from
// slopbooru's internal/booru/pycompat.go, minus the html parts.
package pyjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

// Str is str(v) for the values a sqlite row or a json document holds.
func Str(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case []byte:
		return string(x)
	case bool:
		if x {
			return "True"
		}
		return "False"
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return Float(x)
	case json.Number:
		return numberStr(x)
	default:
		return fmt.Sprint(x)
	}
}

// Float is repr(float): the shortest digits that round-trip, fixed
// notation for exponents in [-4, 16), always with a decimal point.
func Float(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	exp := int(math.Floor(math.Log10(math.Abs(f))))
	// Log10 can be off by one at exact powers; ask strconv for the truth.
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	if i := strings.LastIndexByte(sci, 'e'); i >= 0 {
		exp, _ = strconv.Atoi(sci[i+1:])
	}
	if exp < -4 || exp >= 16 {
		return sci // Go and Python agree here: 1e-05, 1.5e+16
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".") {
		s += ".0"
	}
	return s
}

// numberStr renders a json number the way json.loads + str would: an
// integer literal stays an integer (of any size), anything else is a float.
func numberStr(n json.Number) string {
	s := string(n)
	if strings.ContainsAny(s, ".eE") {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return s
		}
		return Float(f)
	}
	if i, ok := new(big.Int).SetString(s, 10); ok {
		return i.String()
	}
	return s
}

// Numeric is isinstance(v, (int, float)) -- bool included, as in Python.
func Numeric(v any) (float64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		return f, err == nil
	}
	return 0, false
}

// Truthy is Python's bool(v) for json values.
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		f, _ := Numeric(x)
		return f != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// ---- json, as Python's json module writes it -----------------------------

// Decode is json.loads, keeping numbers as their literal.
func Decode(s string) (any, error) {
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if d.More() {
		return nil, fmt.Errorf("extra data")
	}
	return v, nil
}

// KV is one member of an object whose order matters (the api rows).
type KV struct {
	K string
	V any
}

// Dumps is json.dumps(v, indent=indent, sort_keys=sortKeys) with Python's
// defaults: ensure_ascii, ", " / ": " separators.
func Dumps(v any, indent int, sortKeys bool) string {
	var b bytes.Buffer
	dumpJSON(&b, v, indent, 0, sortKeys)
	return b.String()
}

func dumpJSON(b *bytes.Buffer, v any, indent, level int, sortKeys bool) {
	nl := func(l int) {
		if indent > 0 {
			b.WriteByte('\n')
			b.WriteString(strings.Repeat(" ", indent*l))
		}
	}
	itemSep := ", "
	if indent > 0 {
		itemSep = ","
	}
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		b.WriteString(pyjsonString(x))
	case []byte:
		b.WriteString(pyjsonString(string(x)))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case float64:
		switch {
		case math.IsNaN(x):
			b.WriteString("NaN")
		case math.IsInf(x, 1):
			b.WriteString("Infinity")
		case math.IsInf(x, -1):
			b.WriteString("-Infinity")
		default:
			b.WriteString(Float(x))
		}
	case json.Number:
		b.WriteString(numberStr(x))
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		for i, it := range x {
			if i > 0 {
				b.WriteString(itemSep)
			}
			nl(level + 1)
			dumpJSON(b, it, indent, level+1, sortKeys)
		}
		nl(level)
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		// Go maps are unordered, so the original order is gone either way;
		// sorted is the only stable choice (and what sort_keys asks for).
		sort.Strings(keys)
		ordered := make([]KV, len(keys))
		for i, k := range keys {
			ordered[i] = KV{k, x[k]}
		}
		dumpJSON(b, ordered, indent, level, sortKeys)
	case []KV:
		if len(x) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteByte('{')
		for i, it := range x {
			if i > 0 {
				b.WriteString(itemSep)
			}
			nl(level + 1)
			b.WriteString(pyjsonString(it.K))
			b.WriteString(": ")
			dumpJSON(b, it.V, indent, level+1, sortKeys)
		}
		nl(level)
		b.WriteByte('}')
	case [][]KV:
		items := make([]any, len(x))
		for i := range x {
			items[i] = x[i]
		}
		dumpJSON(b, items, indent, level, sortKeys)
	default:
		b.WriteString(pyjsonString(fmt.Sprint(x)))
	}
}

func pyjsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20 || r > 0x7e:
				if r > 0xffff {
					r1, r2 := surrogates(r)
					fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
				} else {
					fmt.Fprintf(&b, `\u%04x`, r)
				}
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func surrogates(r rune) (rune, rune) {
	r -= 0x10000
	return 0xd800 + (r>>10)&0x3ff, 0xdc00 + r&0x3ff
}
