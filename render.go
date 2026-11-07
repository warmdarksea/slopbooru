package main

// Every page, built the way the Python booru built it -- string pieces in
// the same order -- so its output can be diffed against this one's.

import (
	_ "embed"
	"fmt"
	"math"
	"sort"
	"strings"
)

//go:embed style.css
var styleCSS string

// css is the stylesheet as the Python triple-quoted string held it.
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
	return `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + e(title) + `</title><style>` + css + `</style></head><body>
<header>
  <span class="brand"><a href="/">` + e(brand) + `</a></span>
  <form action="/" method="get">
    <input type="search" name="q" value="` + e(q) + `"
      placeholder="marisa &middot; rating:safe &middot; model:pixai tag:marisa &middot; &quot;trying hard&quot;">
    <button type="submit">search</button>
  </form>
  <span class="muted"><a href="/stats">stats</a></span>
</header>
<main>` + body + `</main></body></html>`
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

func (s *Store) renderCards(rows []Row) (string, error) {
	out := []string{`<div class="grid">`}
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
		chips := ""
		if subj != nil {
			chips += `<span class="chip">` + e(subj["value"]) + `</span>`
		}
		if len(ratings) > 0 {
			top := ratings[0]["value"]
			chips += `<span class="chip r-` + e(top) + `">` + e(top) + `</span>`
		}
		id := pystr(r["id"])
		out = append(out,
			`<div class="card"><a href="/image/`+id+`">`+
				`<img loading="lazy" src="/thumb/`+id+`" alt="`+e(name)+`"></a>`+
				`<div class="meta">`+chips+`<div class="name">`+e(name)+`</div>`+
				`<div class="muted">`+pystr(r["n_ann"])+` values · `+pystr(models)+` models</div>`+
				`</div></div>`)
	}
	out = append(out, "</div>")
	return strings.Join(out, ""), nil
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

func (s *Store) renderSidebar() (string, error) {
	out := []string{`<div class="side">`}
	for _, f := range []struct {
		title, field string
		limit        int
	}{{"characters", "subject", 18}, {"tags", "tag", 30}, {"sources", "source", 12}} {
		rows, err := s.query(
			"SELECT value, COUNT(DISTINCT image_id) n FROM annotations "+
				"WHERE field = ? GROUP BY value ORDER BY n DESC, value LIMIT ?",
			f.field, f.limit)
		if err != nil {
			return "", err
		}
		if len(rows) == 0 {
			continue
		}
		out = append(out, "<h3>"+f.title+"</h3>")
		for _, r := range rows {
			out = append(out, `<a href="/value/`+f.field+`/`+quoteAll(pystr(r["value"]))+`">`+
				e(r["value"])+` <span class="n">`+pystr(r["n"])+`</span></a>`)
		}
	}
	rows, err := s.query(
		"SELECT p.model, COUNT(*) n FROM annotations a " +
			"JOIN provenances p ON p.id = a.provenance_id GROUP BY p.model ORDER BY n DESC")
	if err != nil {
		return "", err
	}
	if len(rows) > 0 {
		out = append(out, "<h3>models</h3>")
		for _, r := range rows {
			out = append(out, `<a href="/?q=model:`+quote(pystr(r["model"]))+`">`+
				e(r["model"])+` <span class="n">`+pystr(r["n"])+`</span></a>`)
		}
	}
	out = append(out, "</div>")
	return strings.Join(out, ""), nil
}

// gridPage is the shared shape of the index and a value listing.
func (s *Store) gridPage(heading string, rows []Row, more bool, pageNo int,
	empty, prev, next string) (string, error) {
	body := []string{heading, `<div class="layout"><div>`}
	if len(rows) > 0 {
		cards, err := s.renderCards(rows)
		if err != nil {
			return "", err
		}
		body = append(body, cards)
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
	body = append(body, "</div>")
	side, err := s.renderSidebar()
	if err != nil {
		return "", err
	}
	body = append(body, side, "</div>")
	return strings.Join(body, ""), nil
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
		fmt.Sprintf(`<a href="/?q=%s&page=%d">older →</a>`, quote(qs), pageNo+1))
	if err != nil {
		return "", err
	}
	return page(brand, body, qs), nil
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
			"       p.id AS pid FROM annotations a "+
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
		"SELECT COUNT(*) FROM annotations WHERE image_id = ? AND confidence = 0", imageID)
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
	var boxes []box
	for _, r := range rows {
		if d, ok := r["detail"].(string); ok && r["field"] == "text" && d != "" {
			boxes = append(boxes, ocrBoxes(d)...)
		}
	}

	shot := []string{fmt.Sprintf(`<div class="shot show" id="shot"><img src="/file/%d" alt="%s">`,
		imageID, e(name))}
	for _, b := range boxes {
		shot = append(shot, fmt.Sprintf(
			`<div class="box" style="left:%.2f%%;top:%.2f%%;width:%.2f%%;height:%.2f%%">`+
				`<span>%s</span></div>`,
			b.x1, b.y1, math.Max(0, b.x2-b.x1), math.Max(0, b.y2-b.y1), e(b.text)))
	}
	shot = append(shot, "</div>")

	left := []string{strings.Join(shot, "")}
	if len(boxes) > 0 {
		left = append(left, `<p><label><input type="checkbox" checked `+
			`onchange="document.getElementById('shot').classList.toggle('show')"> `+
			fmt.Sprintf("show %d ocr boxes</label></p>", len(boxes)))
	}
	mime := img["mime"]
	if !truthy(mime) {
		mime = ""
	}
	left = append(left, `<p class="muted mono">`+e(name)+`<br>`+pystr(img["width"])+`×`+
		pystr(img["height"])+` `+e(mime)+` · `+commas(i64(img["bytes"]))+` bytes<br>`+
		`sha256 `+e(img["sha256"])+`</p>`)
	if len(paths) > 1 {
		esc := make([]string, len(paths))
		for i, p := range paths {
			esc[i] = e(p)
		}
		left = append(left, fmt.Sprintf(
			`<details><summary>%d paths</summary><p class="mono">%s</p></details>`,
			len(paths), strings.Join(esc, "<br>")))
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
	order2 := append([]string{}, fieldOrder...)
	for _, f := range seen {
		if !contains(fieldOrder, f) {
			order2 = append(order2, f)
		}
	}
	var right []string
	for _, field := range order2 {
		rs := grouped[field]
		if len(rs) == 0 {
			continue
		}
		right = append(right, fmt.Sprintf("<h2>%s <span class='muted'>(%d)</span></h2>", e(field), len(rs)))
		right = append(right, "<table><tr>"+
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
			if field == "verdict" && truthy(r["detail"]) {
				val += verdictDetail(r["detail"])
			}
			if truthy(r["key"]) {
				val = `<span class="muted">` + e(r["key"]) + `:</span> ` + val
			}
			right = append(right, "<tr><td>"+val+"</td><td>"+confBar(r["confidence"])+"</td>"+
				`<td><a class="badge" href="/provenance/`+pystr(r["pid"])+`">`+
				e(r["model"])+"</a></td></tr>")
		}
		right = append(right, "</table>")
	}
	if zeros != 0 {
		right = append(right, fmt.Sprintf(`<p class="muted">%d value(s) scored 0 and are not shown.</p>`, zeros))
	}
	if len(fails) > 0 {
		right = append(right, "<h2>failures</h2><table>")
		for _, f := range fails {
			right = append(right, `<tr><td><span class="badge">`+e(f["model"])+`</span></td>`+
				`<td class="mono">`+e(f["error"])+`</td></tr>`)
		}
		right = append(right, "</table>")
	}
	body := `<div class="detail"><div>` + strings.Join(left, "") + `</div>` +
		`<div>` + strings.Join(right, "") + `</div></div>`
	return page(name, body, ""), nil
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
			"FROM annotations a JOIN provenances p ON p.id = a.provenance_id "+
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
		fmt.Sprintf(`<a href="%s?page=%d">older →</a>`, base, pageNo+1))
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
