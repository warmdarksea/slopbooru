package booru

// Every page. A booru's shape: the values on a sidebar to the left, the
// pictures beside it; on an image page the picture fitted to the screen with
// its descriptions underneath, and every value a model gave below that.

import (
	_ "embed"
	"fmt"
	"math"
	"slopbooru/internal/cli"
	"sort"
	"strings"
)

//go:embed style.css
var styleCSS string

var css = "\n" + styleCSS

func confBar(c any) string {
	f, ok := numeric(c)
	if !ok || c == nil {
		return `<span class="muted">—</span>`
	}
	pct := math.Max(0, math.Min(1, f)) * 100
	return fmt.Sprintf(`<span class="conf" title="confidence %.3f">`+
		`<i style="width:%.0f%%"></i></span> `+
		`<span class="muted">%.2f</span>`, f, pct, f)
}

func page(title, body, q string) string {
	style, upload := css, ""
	if editMode { // the edit controls exist only with --edit
		style += editCSS
		upload = "\n  <span class=\"muted\"><a href=\"/upload\">upload</a></span>"
	}
	return `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + e(title) + `</title><style>` + style + `</style></head><body>
<header>
  <span class="brand"><a href="/">` + e(cli.Brand) + `</a></span>
  <form action="/" method="get">
    <input type="search" name="q" value="` + e(q) + `"
      placeholder="marisa &middot; rating:safe &middot; model:pixai tag:marisa &middot; &quot;trying hard&quot;">
    <button type="submit">search</button>
  </form>` + upload + `
  <span class="muted"><a href="/stats">stats</a></span>
</header>
<main>` + body + `</main></body></html>`
}

// layout is a booru page: the sidebar, then the content beside it (on a
// narrow screen, under it).
func layout(side, content string) string {
	return `<div class="layout"><aside class="side">` + side + `</aside>` +
		`<section class="content">` + content + `</section></div>`
}

// the sidebar's groups, in order: field, heading, how many to list
var sideGroups = []struct {
	field, title string
	limit        int
}{
	{"subject", "characters", 25}, {"source", "sources", 15}, {"tag", "tags", 60},
	{"emotion", "moods", 15}, {"hazard", "hazards", 15}, {"rating", "rating", 6},
	{"verdict", "verdict", 4}, {"dimensionality", "dimensionality", 4},
}

// narrow is the search q with one more value required; "" when the value
// can't be written as a filter (a double quote in it).
func narrow(q, field, value string) string {
	if strings.Contains(value, `"`) {
		return ""
	}
	return "/?q=" + quote(strings.TrimSpace(q+" "+field+`:"`+value+`"`))
}

func i64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	}
	return 0
}

// renderThumbs is the grid of pictures, each the whole picture fitted to
// its cell; what used to be a card's text is its tooltip.
func (s *Store) renderThumbs(rows []Row) (string, error) {
	out := []string{`<div class="thumbs">`}
	for _, r := range rows {
		subj, ratings, models, err := s.Summary(r["id"])
		if err != nil {
			return "", err
		}
		paths, err := s.Paths(r["id"])
		if err != nil {
			return "", err
		}
		var name string
		if len(paths) > 0 {
			name = basename(paths[0])
		} else {
			name = pystr(r["sha256"])[:12]
		}
		tip := []string{name}
		if subj != nil {
			tip = append(tip, pystr(subj["value"]))
		}
		cls := "thumb"
		if len(ratings) > 0 {
			top := pystr(ratings[0]["value"])
			tip = append(tip, "rating:"+top)
			cls += " r-" + safeClass(top)
		}
		tip = append(tip, pystr(r["n_ann"])+" values · "+pystr(models)+" models")
		id := pystr(r["id"])
		out = append(out, `<a class="`+cls+`" href="/image/`+id+`" title="`+e(strings.Join(tip, " · "))+`">`+
			`<img loading="lazy" src="/thumb/`+id+`" alt="`+e(name)+`"></a>`)
	}
	out = append(out, "</div>")
	return strings.Join(out, ""), nil
}

// safeClass keeps a value usable as a css class name
func safeClass(v string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(v) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sortHeader is a clickable column head: the same page, ordered by that column.
func sortHeader(imageID int64, key, label, current string) string {
	mark, cls := "", ""
	if key == current {
		mark, cls = " ▾", ` class="on"`
	}
	return fmt.Sprintf(`<th%s><a href="/image/%d?sort=%s">%s%s</a></th>`,
		cls, imageID, key, e(label), mark)
}

// verdictDetail is llama-guard's raw answer, its token probabilities and
// the context it was given -- all three live in the verdict row's detail.
func verdictDetail(blob any) string {
	str, ok := blob.(string)
	if !ok {
		return ""
	}
	v, err := jsonDecode(str)
	if err != nil {
		return ""
	}
	if !truthy(v) {
		v = map[string]any{}
	}
	d, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	var bits []string
	if truthy(d["raw"]) {
		bits = append(bits, `<p class="mono">`+e(strings.TrimSpace(pystr(d["raw"])))+`</p>`)
	}
	if probs, ok := d["logprobs"].(map[string]any); ok {
		keys := make([]string, 0, len(probs))
		for k := range probs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var shown []string
		for _, k := range keys {
			if f, ok := numeric(probs[k]); ok {
				shown = append(shown, fmt.Sprintf("p(%s)=%.3f", k, f))
			}
		}
		if len(shown) > 0 {
			bits = append(bits, `<p class="mono">`+e(strings.Join(shown, ", "))+`</p>`)
		}
	}
	if truthy(d["context"]) {
		bits = append(bits, `<pre class="mono" style="white-space:pre-wrap">`+
			e(pystr(d["context"]))+`</pre>`)
	}
	if len(bits) == 0 {
		return ""
	}
	return "<details><summary>what it judged</summary>" + strings.Join(bits, "") + "</details>"
}

// listingSide is a listing's sidebar: the values of the pictures on this
// page, each with the number of pictures in the store that have it, and a
// + that narrows the current search to it. Values a model scored 0 ("not
// this") don't count.
func (s *Store) listingSide(rows []Row, q string) (string, error) {
	var out []string
	if len(rows) > 0 {
		var args []any
		for _, r := range rows {
			args = append(args, r["id"])
		}
		for _, g := range sideGroups {
			args = append(args, g.field)
		}
		vals, err := s.query(
			"WITH here AS (SELECT DISTINCT field, value FROM "+visible+" "+
				"WHERE image_id IN ("+placeholders(len(rows))+") "+
				"AND field IN ("+placeholders(len(sideGroups))+") "+
				"AND (confidence IS NULL OR confidence > 0)) "+
				"SELECT h.field, h.value, COUNT(DISTINCT v.image_id) n FROM here h "+
				"JOIN "+visible+" v ON v.field = h.field AND v.value = h.value "+
				"AND (v.confidence IS NULL OR v.confidence > 0) "+
				"GROUP BY h.field, h.value ORDER BY n DESC, h.value", args...)
		if err != nil {
			return "", err
		}
		byField := map[string][]Row{}
		for _, v := range vals {
			f := pystr(v["field"])
			byField[f] = append(byField[f], v)
		}
		for _, g := range sideGroups {
			vs := byField[g.field]
			if len(vs) == 0 {
				continue
			}
			out = append(out, "<h3>"+g.title+"</h3><ul>")
			for i, v := range vs {
				if i == g.limit {
					out = append(out, fmt.Sprintf(`<li class="muted">and %d more</li>`, len(vs)-g.limit))
					break
				}
				val := pystr(v["value"])
				add := ""
				if href := narrow(q, g.field, val); href != "" {
					add = `<a class="add" href="` + href + `" title="narrow the search to this">+</a>`
				}
				out = append(out, `<li>`+add+`<a href="/value/`+g.field+`/`+quoteAll(val)+`">`+
					e(val)+`</a> <span class="n">`+pystr(v["n"])+`</span></li>`)
			}
			out = append(out, "</ul>")
		}
	}
	rows2, err := s.query(
		"SELECT p.model, COUNT(*) n FROM " + visible + " a " +
			"JOIN provenances p ON p.id = a.provenance_id GROUP BY p.model ORDER BY n DESC")
	if err != nil {
		return "", err
	}
	if len(rows2) > 0 {
		out = append(out, "<h3>models</h3><ul>")
		for _, r := range rows2 {
			out = append(out, `<li><a href="/?q=model:`+quote(pystr(r["model"]))+`">`+
				e(r["model"])+`</a> <span class="n">`+pystr(r["n"])+`</span></li>`)
		}
		out = append(out, "</ul>")
	}
	return strings.Join(out, ""), nil
}

// gridPage is the shared shape of the index and a value listing.
func (s *Store) gridPage(heading string, rows []Row, more bool, pageNo int,
	empty, prev, next, q string) (string, error) {
	body := []string{heading}
	if len(rows) > 0 {
		thumbs, err := s.renderThumbs(rows)
		if err != nil {
			return "", err
		}
		body = append(body, thumbs)
	} else {
		body = append(body, empty)
	}
	var nav []string
	if pageNo != 0 {
		nav = append(nav, prev)
	}
	if more {
		nav = append(nav, next)
	}
	if len(nav) > 0 {
		body = append(body, `<div class="pager">`+strings.Join(nav, " ")+"</div>")
	}
	side, err := s.listingSide(rows, q)
	if err != nil {
		return "", err
	}
	return layout(side, strings.Join(body, "")), nil
}

func (s *Store) RenderIndex(qs string, pageNo int) (string, error) {
	rows, more, total, err := s.Search(qs, pageNo)
	if err != nil {
		return "", err
	}
	h := fmt.Sprintf("<h1>%d image", total)
	if total != 1 {
		h += "s"
	}
	if qs != "" {
		h += " matching <em>" + e(qs) + "</em>"
	}
	h += "</h1>"
	body, err := s.gridPage(h, rows, more, pageNo, "<p class='muted'>nothing here.</p>",
		fmt.Sprintf(`<a href="/?q=%s&page=%d">← newer</a>`, quote(qs), pageNo-1),
		fmt.Sprintf(`<a href="/?q=%s&page=%d">older →</a>`, quote(qs), pageNo+1), qs)
	if err != nil {
		return "", err
	}
	return page(cli.Brand, body, qs), nil
}

type box struct {
	text           any
	x1, y1, x2, y2 float64
}

// ocrBoxes reads deepseek-ocr's grounding boxes (0..1000 coordinates) out
// of a text row's detail, as percentages.
func ocrBoxes(detail string) []box {
	v, err := jsonDecode(detail)
	if err != nil {
		return nil
	}
	d, _ := v.(map[string]any)
	list, _ := d["boxes"].([]any)
	var out []box
	for _, it := range list {
		b, ok := it.(map[string]any)
		if !ok {
			break
		}
		coords, ok := b["box"].([]any)
		if !ok || len(coords) != 4 {
			break // the python raised here and kept what it had
		}
		var c [4]float64
		for i := range coords {
			if _, isBool := coords[i].(bool); isBool {
				c[i], _ = numeric(coords[i])
				continue
			}
			f, ok := coords[i].(interface{ Float64() (float64, error) })
			if !ok {
				return out
			}
			c[i], err = f.Float64()
			if err != nil {
				return out
			}
		}
		text, has := b["text"]
		if !has {
			text = ""
		}
		out = append(out, box{text, c[0] / 10, c[1] / 10, c[2] / 10, c[3] / 10})
	}
	return out
}

func (s *Store) RenderImage(imageID int64, sortKey string) (string, error) {
	order, ok := sorts[sortKey]
	if !ok {
		order = sorts["conf"]
	}
	img, err := s.queryRow("SELECT * FROM images WHERE id = ?", imageID)
	if err != nil || img == nil {
		return "", err
	}
	paths, err := s.Paths(imageID)
	if err != nil {
		return "", err
	}
	rows, err := s.query(
		"SELECT a.field, a.key, a.value, a.confidence, a.detail, p.model, "+
			"       p.id AS pid FROM "+visible+" a "+
			"JOIN provenances p ON p.id = a.provenance_id WHERE a.image_id = ? "+
			// a model that scored a value at zero said "not this"; it is not
			// an answer worth a row of the page. A NULL confidence is
			// different -- the model never offered a number -- and stays.
			"AND (a.confidence IS NULL OR a.confidence > 0) "+
			"ORDER BY a.field, "+order, imageID)
	if err != nil {
		return "", err
	}
	zeros, err := s.count(
		"SELECT COUNT(*) FROM "+visible+" WHERE image_id = ? AND confidence = 0", imageID)
	if err != nil {
		return "", err
	}
	fails, err := s.query(
		"SELECT p.model, f.error FROM failures f "+
			"JOIN provenances p ON p.id = f.provenance_id WHERE f.image_id = ?", imageID)
	if err != nil {
		return "", err
	}

	name := pystr(img["sha256"])[:12]
	if len(paths) > 0 {
		name = basename(paths[0])
	}
	grouped := map[string][]Row{}
	var seen []string
	for _, r := range rows {
		f := pystr(r["field"])
		if _, ok := grouped[f]; !ok {
			seen = append(seen, f)
		}
		grouped[f] = append(grouped[f], r)
	}

	// ---- the sidebar: the values, then the facts --------------------------
	side := imageSide(grouped)
	mime := img["mime"]
	if !truthy(mime) {
		mime = ""
	}
	side = append(side, `<h3>information</h3><ul class="info">`+
		`<li>`+pystr(img["width"])+`×`+pystr(img["height"])+` `+e(mime)+`</li>`+
		`<li>`+commas(i64(img["bytes"]))+` bytes</li>`+
		`<li class="mono" title="sha256">`+e(img["sha256"])+`</li>`+
		`<li><a href="/file/`+pystr(imageID)+`">original file</a></li>`+
		`<li><a href="/api/image/`+pystr(imageID)+`">json</a></li></ul>`)
	if len(paths) > 0 {
		esc := make([]string, len(paths))
		for i, p := range paths {
			esc[i] = e(p)
		}
		side = append(side, fmt.Sprintf(
			`<details><summary>%d path(s)</summary><p class="mono">%s</p></details>`,
			len(paths), strings.Join(esc, "<br>")))
	}
	var who []string
	whoSeen := map[string]bool{}
	for _, r := range rows {
		k := pystr(r["pid"])
		if !whoSeen[k] {
			whoSeen[k] = true
			who = append(who, `<a class="badge" href="/provenance/`+k+`">`+e(r["model"])+`</a>`)
		}
	}
	if len(who) > 0 {
		side = append(side, `<h3>models</h3><p>`+strings.Join(who, " ")+`</p>`)
	}

	// ---- the picture, fitted to the screen; a click shows it full size ---
	var boxes []box
	for _, r := range grouped["text"] {
		if d, ok := r["detail"].(string); ok && d != "" {
			boxes = append(boxes, ocrBoxes(d)...)
		}
	}
	shot := []string{fmt.Sprintf(`<div class="shot show fit" id="shot">`+
		`<img src="/file/%d" alt="%s" title="click: full size / fit to screen" `+
		`onclick="document.getElementById('shot').classList.toggle('fit')">`, imageID, e(name))}
	for _, b := range boxes {
		shot = append(shot, fmt.Sprintf(
			`<div class="box" style="left:%.2f%%;top:%.2f%%;width:%.2f%%;height:%.2f%%">`+
				`<span>%s</span></div>`,
			b.x1, b.y1, math.Max(0, b.x2-b.x1), math.Max(0, b.y2-b.y1), e(b.text)))
	}
	shot = append(shot, "</div>")
	main := []string{`<h1 class="name">` + e(name) + `</h1>`, strings.Join(shot, "")}
	if len(boxes) > 0 {
		main = append(main, `<p class="muted"><label><input type="checkbox" checked `+
			`onchange="document.getElementById('shot').classList.toggle('show')"> `+
			fmt.Sprintf("show %d ocr boxes</label></p>", len(boxes)))
	}

	// ---- under it: what the models said about it in words ----------------
	for _, f := range []struct{ field, title string }{
		{"description", "descriptions"}, {"text", "text in the picture"},
	} {
		rs := grouped[f.field]
		if len(rs) == 0 {
			continue
		}
		main = append(main, `<h2>`+f.title+`</h2>`)
		for _, r := range rs {
			main = append(main, `<div class="said"><p class="prose">`+e(r["value"])+`</p>`+
				`<p class="by"><a class="badge" href="/provenance/`+pystr(r["pid"])+`">`+e(r["model"])+
				`</a> `+confBar(r["confidence"])+`</p></div>`)
		}
	}
	for _, r := range grouped["verdict"] {
		if truthy(r["detail"]) {
			if d := verdictDetail(r["detail"]); d != "" {
				main = append(main, `<h2>safety: `+e(r["value"])+` <span class="muted">(`+
					e(r["model"])+`)</span></h2>`+d)
			}
		}
	}

	// ---- and everything, value by value, with who said it -----------------
	order2 := append([]string{}, fieldOrder...)
	for _, f := range seen {
		if !contains(fieldOrder, f) {
			order2 = append(order2, f)
		}
	}
	var all []string
	for _, field := range order2 {
		rs := grouped[field]
		if len(rs) == 0 {
			continue
		}
		all = append(all, fmt.Sprintf("<h2>%s <span class='muted'>(%d)</span></h2>", e(field), len(rs)))
		all = append(all, "<table><tr>"+
			sortHeader(imageID, "value", "value", sortKey)+
			sortHeader(imageID, "conf", "conf", sortKey)+
			sortHeader(imageID, "model", "model", sortKey)+"</tr>")
		for _, r := range rs {
			var val string
			if linkedFields[field] {
				href := "/value/" + field + "/" + quoteAll(pystr(r["value"]))
				val = `<a href="` + href + `">` + e(r["value"]) + `</a>`
			} else {
				val = `<span class="prose">` + e(r["value"]) + `</span>`
			}
			if truthy(r["key"]) {
				val = `<span class="muted">` + e(r["key"]) + `:</span> ` + val
			}
			if editMode {
				val += removeButton(imageID, field, pystr(r["key"]), pystr(r["value"]))
			}
			all = append(all, "<tr><td>"+val+"</td><td>"+confBar(r["confidence"])+"</td>"+
				`<td><a class="badge" href="/provenance/`+pystr(r["pid"])+`">`+
				e(r["model"])+"</a></td></tr>")
		}
		all = append(all, "</table>")
	}
	if len(all) > 0 {
		open := ""
		if sortKey != "" || editMode {
			open = " open"
		}
		main = append(main, fmt.Sprintf(`<details class="values" id="values"%s>`+
			`<summary>every value, by model (%d)</summary>`, open, len(rows))+
			strings.Join(all, "")+`</details>`)
	}
	removed, err := s.removedNote(imageID)
	if err != nil {
		return "", err
	}
	if removed != "" {
		main = append(main, removed)
	}
	if zeros != 0 {
		main = append(main, fmt.Sprintf(`<p class="muted">%d value(s) scored 0 and are not shown.</p>`, zeros))
	}
	if len(fails) > 0 {
		main = append(main, "<h2>failures</h2><table>")
		for _, f := range fails {
			main = append(main, `<tr><td><span class="badge">`+e(f["model"])+`</span></td>`+
				`<td class="mono">`+e(f["error"])+`</td></tr>`)
		}
		main = append(main, "</table>")
	}
	if editMode {
		main = append(main, editPanel(imageID))
	}
	return page(name, layout(strings.Join(side, ""), strings.Join(main, "")), ""), nil
}

// imageSide is an image page's sidebar: each value once, however many
// models gave it, with the best confidence any of them had (the tooltip
// says who said what).
func imageSide(grouped map[string][]Row) []string {
	var out []string
	for _, g := range sideGroups {
		type agg struct {
			key, value string
			best       float64
			has        bool
			who        []string
		}
		var vals []*agg
		index := map[string]*agg{}
		for _, r := range grouped[g.field] {
			k, v := pystr(r["key"]), pystr(r["value"])
			a := index[k+"\x00"+v]
			if a == nil {
				a = &agg{key: k, value: v}
				index[k+"\x00"+v] = a
				vals = append(vals, a)
			}
			w := pystr(r["model"])
			if f, ok := numeric(r["confidence"]); ok && r["confidence"] != nil {
				w += fmt.Sprintf(" %.2f", f)
				if !a.has || f > a.best {
					a.best, a.has = f, true
				}
			}
			a.who = append(a.who, w)
		}
		if len(vals) == 0 {
			continue
		}
		sort.SliceStable(vals, func(i, j int) bool {
			if vals[i].has != vals[j].has {
				return vals[i].has
			}
			if vals[i].best != vals[j].best {
				return vals[i].best > vals[j].best
			}
			return strings.ToLower(vals[i].value) < strings.ToLower(vals[j].value)
		})
		out = append(out, "<h3>"+g.title+"</h3><ul>")
		for _, a := range vals {
			key := ""
			if a.key != "" {
				key = `<span class="muted">` + e(a.key) + `:</span> `
			}
			score := ""
			if a.has {
				score = fmt.Sprintf("%.2f", a.best)
			}
			if len(a.who) > 1 {
				score += fmt.Sprintf(" ×%d", len(a.who))
			}
			out = append(out, `<li>`+key+`<a href="/value/`+g.field+`/`+quoteAll(a.value)+`">`+e(a.value)+`</a>`+
				` <span class="n" title="`+e(strings.Join(a.who, ", "))+`">`+strings.TrimSpace(score)+`</span></li>`)
		}
		out = append(out, "</ul>")
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if x == y {
			return true
		}
	}
	return false
}

func (s *Store) RenderValue(field, value string, pageNo int) (string, error) {
	qs := field + `:"` + value + `"`
	rows, more, total, err := s.Search(qs, pageNo)
	if err != nil {
		return "", err
	}
	models, err := s.query(
		"SELECT p.model, COUNT(*) n, ROUND(AVG(a.confidence), 3) c "+
			"FROM "+visible+" a JOIN provenances p ON p.id = a.provenance_id "+
			"WHERE a.field = ? AND a.value = ? COLLATE NOCASE GROUP BY p.model "+
			"ORDER BY n DESC", field, value)
	if err != nil {
		return "", err
	}
	var who []string
	for _, m := range models {
		w := `<span class="chip">` + e(m["model"]) + ` ×` + pystr(m["n"])
		if c, ok := numeric(m["c"]); ok && m["c"] != nil {
			w += fmt.Sprintf(" · %.2f", c)
		}
		who = append(who, w+"</span>")
	}
	head := fmt.Sprintf("<h1>%s: %s <span class='muted'>(%d)</span></h1>", e(field), e(value), total) +
		"<p>" + strings.Join(who, " ") + "</p>"
	base := "/value/" + field + "/" + quoteAll(value)
	body, err := s.gridPage(head, rows, more, pageNo, "<p class='muted'>nothing.</p>",
		fmt.Sprintf(`<a href="%s?page=%d">← newer</a>`, base, pageNo-1),
		fmt.Sprintf(`<a href="%s?page=%d">older →</a>`, base, pageNo+1), qs)
	if err != nil {
		return "", err
	}
	return page(field+": "+value, body, ""), nil
}

func (s *Store) RenderProvenance(pid int64) (string, error) {
	p, err := s.queryRow("SELECT * FROM provenances WHERE id = ?", pid)
	if err != nil || p == nil {
		return "", err
	}
	paths, err := s.query(
		"SELECT s.path FROM closure_paths c JOIN store_paths s "+
			"ON s.id = c.store_path_id WHERE c.closure_id = ? ORDER BY s.path", p["closure_id"])
	if err != nil {
		return "", err
	}
	stats, err := s.query(
		"SELECT field, COUNT(*) n, ROUND(AVG(confidence), 3) c FROM annotations "+
			"WHERE provenance_id = ? GROUP BY field ORDER BY n DESC", pid)
	if err != nil {
		return "", err
	}
	nImg, err := s.count(
		"SELECT COUNT(DISTINCT image_id) FROM annotations WHERE provenance_id = ?", pid)
	if err != nil {
		return "", err
	}

	var rows strings.Builder
	for _, k := range []string{"model", "mode", "model_path", "runner_path", "engine", "created"} {
		rows.WriteString("<tr><td>" + e(k) + "</td><td class='mono'>" + e(p[k]) + "</td></tr>")
	}
	params := pystr(p["params"])
	if s, ok := p["params"].(string); ok {
		if v, err := jsonDecode(s); err == nil {
			params = pyjson(v, 2, true)
		}
	}
	model := pystr(p["model"])
	body := []string{
		fmt.Sprintf(`<h1>provenance %d: <span class="badge">%s</span></h1>`, pid, e(model)),
		fmt.Sprintf("<p class='muted'>%d image(s) annotated by this build.</p>", nImg),
		"<table>" + rows.String() + "</table>",
		"<h2>params</h2><pre class='mono'>" + e(params) + "</pre>",
		"<h2>fields</h2><table><tr><th>field</th><th>n</th><th>mean conf</th></tr>",
	}
	for _, st := range stats {
		mean := ""
		if c, ok := numeric(st["c"]); ok && st["c"] != nil {
			mean = fmt.Sprintf("%.3f", c)
		}
		href := "/?q=" + quote("model:"+model+" field:"+pystr(st["field"]))
		body = append(body, `<tr><td><a href="`+href+`">`+e(st["field"])+`</a></td>`+
			`<td>`+pystr(st["n"])+`</td><td>`+mean+`</td></tr>`)
	}
	body = append(body, "</table>",
		fmt.Sprintf("<h2>nix store path tree <span class='muted'>(%d paths)</span></h2>", len(paths)))
	if len(paths) > 0 {
		esc := make([]string, len(paths))
		for i, r := range paths {
			esc[i] = e(r["path"])
		}
		body = append(body, "<details open><summary>closure</summary><p class='mono'>"+
			strings.Join(esc, "<br>")+"</p></details>")
	} else {
		body = append(body, "<p class='muted'>no closure recorded for this provenance.</p>")
	}
	return page(fmt.Sprintf("provenance %d", pid), strings.Join(body, ""), ""), nil
}

func (s *Store) RenderStats() (string, error) {
	var totals []string
	for _, t := range [][2]string{
		{"images", "images"}, {"annotations", "annotations"}, {"provenances", "provenances"},
		{"failures", "failures"}, {"store paths", "store_paths"},
	} {
		n, err := s.count("SELECT COUNT(*) FROM " + t[1])
		if err != nil {
			return "", err
		}
		totals = append(totals, "<b>"+commas(n)+"</b> "+t[0])
	}
	rows, err := s.query("SELECT * FROM model_field_stats ORDER BY model, field")
	if err != nil {
		return "", err
	}
	fields, err := s.query(
		"SELECT field, COUNT(*) n, COUNT(DISTINCT image_id) imgs FROM annotations " +
			"GROUP BY field ORDER BY n DESC")
	if err != nil {
		return "", err
	}
	body := []string{
		"<h1>store</h1><p>" + strings.Join(totals, " · ") + "</p>",
		"<h2>per model and field</h2>",
		"<table><tr><th>model</th><th>field</th><th>n</th>" +
			"<th>with confidence</th><th>mean</th></tr>",
	}
	for _, r := range rows {
		mean := ""
		if r["mean_confidence"] != nil {
			mean = pystr(r["mean_confidence"])
		}
		body = append(body, `<tr><td><a href="/?q=model:`+quote(pystr(r["model"]))+`">`+
			e(r["model"])+`</a></td><td>`+e(r["field"])+`</td><td>`+pystr(r["n"])+`</td>`+
			`<td>`+pystr(r["n_with_confidence"])+`</td>`+
			`<td>`+mean+`</td></tr>`)
	}
	body = append(body, "</table><h2>per field</h2><table><tr><th>field</th><th>values</th>"+
		"<th>images</th></tr>")
	for _, r := range fields {
		body = append(body, `<tr><td>`+e(r["field"])+`</td><td>`+pystr(r["n"])+`</td><td>`+
			pystr(r["imgs"])+`</td></tr>`)
	}
	body = append(body, "</table>")
	return page("stats", strings.Join(body, ""), ""), nil
}
