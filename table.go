// WRP Table Layout Mode
// Compiles the layout Chrome already computed (via DOMSnapshot) into
// HTML 3.2 nested tables with FONT/BGCOLOR - automatic 1996 style table
// layout. Text stays text, images are downscaled, geometry is approximated
// by recursive XY-cut over the layout boxes.
package main

import (
	"context"
	"fmt"
	"html"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/chromedp/cdproto/domsnapshot"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/lithammer/shortuuid/v4"
)

const (
	tblRowGap   = 3.0  // min vertical gap that splits rows
	tblColGap   = 10.0 // min horizontal gap that splits columns
	tblMaxDepth = 12
)

// order must match sty* constants below
var tblStyles = []string{"visibility", "color", "background-color", "font-size", "font-weight", "font-style", "font-family"}

const (
	styVis = iota
	styColor
	styBg
	styFSize
	styFWeight
	styFStyle
	styFFamily
)

type tblImg struct {
	id  string
	url string
	w   int
	h   int
	alt string
	ok  bool
}

type tblAtom struct {
	x, y, w, h float64
	text       string
	href       string
	colorHex   string
	colorDark  bool
	fontSize   int
	bold       bool
	italic     bool
	mono       bool
	img        *tblImg
	ctrl       string
}

type tblBg struct {
	x, y, w, h, area float64
	color            string
}

type tblBox struct {
	x, y, w, h float64
}

type tblSnap struct {
	rq      *wrpReq
	doc     *domsnapshot.DocumentSnapshot
	strs    []string
	kids    map[int64][]int64
	tboxes  map[int64][]int
	base    *url.URL
	linkPfx string
	pageW   float64
	pageBg  string
	atoms   []tblAtom
	bgs     []tblBg
	imgs    []*tblImg
	imgExt  string
	imgOpt  int
}

func (s *tblSnap) str(i domsnapshot.StringIndex) string {
	if i < 0 || int(i) >= len(s.strs) {
		return ""
	}
	return s.strs[i]
}

func (s *tblSnap) nodeName(ni int64) string {
	nn := s.doc.Nodes.NodeName
	if ni < 0 || int(ni) >= len(nn) {
		return ""
	}
	return s.str(nn[ni])
}

func (s *tblSnap) parent(ni int64) int64 {
	pi := s.doc.Nodes.ParentIndex
	if ni < 0 || int(ni) >= len(pi) {
		return -1
	}
	return pi[ni]
}

func (s *tblSnap) attr(ni int64, name string) string {
	at := s.doc.Nodes.Attributes
	if ni < 0 || int(ni) >= len(at) {
		return ""
	}
	pairs := at[ni]
	for i := 0; i+1 < len(pairs); i += 2 {
		if s.str(domsnapshot.StringIndex(pairs[i])) == name {
			return s.str(domsnapshot.StringIndex(pairs[i+1]))
		}
	}
	return ""
}

func (s *tblSnap) rareStr(rd *domsnapshot.RareStringData, ni int64) string {
	if rd == nil {
		return ""
	}
	for i, idx := range rd.Index {
		if idx == ni && i < len(rd.Value) {
			return s.str(rd.Value[i])
		}
	}
	return ""
}

func rareBoolHas(rd *domsnapshot.RareBooleanData, ni int64) bool {
	if rd == nil {
		return false
	}
	for _, idx := range rd.Index {
		if idx == ni {
			return true
		}
	}
	return false
}

func (s *tblSnap) style(li int, idx int) string {
	st := s.doc.Layout.Styles
	if li >= len(st) || idx >= len(st[li]) {
		return ""
	}
	return s.str(domsnapshot.StringIndex(st[li][idx]))
}

func parseRGB(v string) (int, int, int, float64, bool) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "rgb") {
		return 0, 0, 0, 0, false
	}
	o := strings.Index(v, "(")
	c := strings.LastIndex(v, ")")
	if o < 0 || c <= o {
		return 0, 0, 0, 0, false
	}
	parts := strings.Split(v[o+1:c], ",")
	if len(parts) < 3 {
		return 0, 0, 0, 0, false
	}
	var n [3]int
	for i := 0; i < 3; i++ {
		f, err := strconv.ParseFloat(strings.TrimSpace(parts[i]), 64)
		if err != nil {
			return 0, 0, 0, 0, false
		}
		n[i] = int(f)
	}
	a := 1.0
	if len(parts) > 3 {
		a, _ = strconv.ParseFloat(strings.TrimSpace(parts[3]), 64)
	}
	return n[0], n[1], n[2], a, true
}

func hexOf(r, g, b int) string {
	return fmt.Sprintf("#%02X%02X%02X", r, g, b)
}

func hexLum(c string) int {
	if len(c) != 7 || c[0] != '#' {
		return 255
	}
	r, e1 := strconv.ParseInt(c[1:3], 16, 32)
	g, e2 := strconv.ParseInt(c[3:5], 16, 32)
	b, e3 := strconv.ParseInt(c[5:7], 16, 32)
	if e1 != nil || e2 != nil || e3 != nil {
		return 255
	}
	return int((299*r + 587*g + 114*b) / 1000)
}

func fontSizeClass(px float64) int {
	switch {
	case px <= 0:
		return 3
	case px < 10:
		return 1
	case px < 13:
		return 2
	case px < 16.5:
		return 3
	case px < 20:
		return 4
	case px < 26:
		return 5
	case px < 38:
		return 6
	}
	return 7
}

var tblFolder = strings.NewReplacer(
	" ", " ", "‘", "'", "’", "'", "‚", "'",
	"“", `"`, "”", `"`, "„", `"`, "«", `"`, "»", `"`,
	"–", "-", "—", "-", "−", "-", "…", "...",
	"•", "*", "·", "*", "×", "x",
	"©", "(c)", "®", "(r)", "™", "(tm)",
)

// fold typography to ascii, remaining non-ascii runes become dots
func tblASCII(t string) string {
	t = tblFolder.Replace(t)
	var b strings.Builder
	for _, r := range t {
		if r > 127 {
			b.WriteByte('.')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// icon font glyphs etc that would render as pure dots
func allNonASCII(t string) bool {
	has := false
	for _, r := range t {
		if r == ' ' || r == '\t' || r == '\n' {
			continue
		}
		if r <= 127 {
			return false
		}
		has = true
	}
	return has
}

func tblEsc(t string) string {
	return html.EscapeString(tblASCII(t))
}

func (s *tblSnap) underControl(ni int64) bool {
	for p := s.parent(ni); p >= 0; p = s.parent(p) {
		switch s.nodeName(p) {
		case "INPUT", "TEXTAREA", "SELECT", "BUTTON", "OPTION":
			return true
		case "BODY":
			return false
		}
	}
	return false
}

func (s *tblSnap) linkFor(ni int64) string {
	for p := ni; p >= 0; p = s.parent(p) {
		if s.nodeName(p) != "A" {
			continue
		}
		href := s.attr(p, "href")
		if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "javascript:") {
			return ""
		}
		abs := resolveURL(href, s.base)
		if s.rq.proxy {
			return strings.Replace(abs, "https://", "http://", 1)
		}
		return s.linkPfx + url.QueryEscape(abs)
	}
	return ""
}

func (s *tblSnap) gatherText(ni int64) string {
	var sb strings.Builder
	var rec func(int64)
	rec = func(n int64) {
		if s.nodeName(n) == "#text" {
			nv := s.doc.Nodes.NodeValue
			if int(n) < len(nv) {
				sb.WriteString(s.str(nv[n]))
				sb.WriteByte(' ')
			}
			return
		}
		for _, c := range s.kids[n] {
			rec(c)
		}
	}
	rec(ni)
	return strings.Join(strings.Fields(sb.String()), " ")
}

func (s *tblSnap) collect() {
	d := s.doc
	lay := d.Layout
	for i, li := range d.TextBoxes.LayoutIndex {
		s.tboxes[li] = append(s.tboxes[li], i)
	}
	for i, p := range d.Nodes.ParentIndex {
		if p >= 0 {
			s.kids[p] = append(s.kids[p], int64(i))
		}
	}
	for li := 0; li < len(lay.NodeIndex); li++ {
		ni := lay.NodeIndex[li]
		if li >= len(lay.Bounds) || len(lay.Bounds[li]) < 4 {
			continue
		}
		b := lay.Bounds[li]
		x, y, w, h := b[0], b[1], b[2], b[3]
		name := s.nodeName(ni)
		if name == "BODY" || name == "HTML" {
			if s.pageBg == "" {
				if r, g, bb, a, ok := parseRGB(s.style(li, styBg)); ok && a > 0.98 {
					s.pageBg = hexOf(r, g, bb)
				}
			}
			continue
		}
		if w <= 0 || h <= 0 || x+w <= 0 || y+h <= 0 || x >= s.pageW {
			continue
		}
		if v := s.style(li, styVis); v == "hidden" || v == "collapse" {
			continue
		}
		switch name {
		case "#text":
			s.textAtoms(li, ni)
		case "IMG":
			s.imgAtom(li, ni)
		case "INPUT", "TEXTAREA", "SELECT", "BUTTON":
			s.ctrlAtom(li, ni, name)
		default:
			if r, g, bb, a, ok := parseRGB(s.style(li, styBg)); ok && a > 0.98 && w >= 8 && h >= 8 {
				s.bgs = append(s.bgs, tblBg{x, y, w, h, w * h, hexOf(r, g, bb)})
			}
		}
	}
	// smallest background first, findBg picks the innermost container
	sort.Slice(s.bgs, func(i, j int) bool { return s.bgs[i].area < s.bgs[j].area })
}

func (s *tblSnap) addText(a tblAtom) {
	if strings.TrimSpace(a.text) == "" || allNonASCII(a.text) {
		return
	}
	s.atoms = append(s.atoms, a)
}

func (s *tblSnap) textAtoms(li int, ni int64) {
	if s.underControl(ni) {
		return
	}
	lay := s.doc.Layout
	if li >= len(lay.Text) {
		return
	}
	full := s.str(lay.Text[li])
	if strings.TrimSpace(full) == "" {
		return
	}
	var base tblAtom
	if r, g, b, _, ok := parseRGB(s.style(li, styColor)); ok {
		base.colorHex = hexOf(r, g, b)
		base.colorDark = r < 112 && g < 112 && b < 112
	}
	fs, _ := strconv.ParseFloat(strings.TrimSuffix(s.style(li, styFSize), "px"), 64)
	base.fontSize = fontSizeClass(fs)
	wt := s.style(li, styFWeight)
	if n, err := strconv.Atoi(wt); err == nil {
		base.bold = n >= 600
	} else {
		base.bold = strings.Contains(wt, "bold")
	}
	base.italic = strings.Contains(s.style(li, styFStyle), "italic")
	ff := strings.ToLower(s.style(li, styFFamily))
	base.mono = strings.Contains(ff, "mono") || strings.Contains(ff, "courier") || strings.Contains(ff, "consolas")
	base.href = s.linkFor(ni)

	boxes := s.tboxes[int64(li)]
	if len(boxes) == 0 {
		b := lay.Bounds[li]
		a := base
		a.x, a.y, a.w, a.h = b[0], b[1], b[2], b[3]
		a.text = full
		s.addText(a)
		return
	}
	u := utf16.Encode([]rune(full))
	tb := s.doc.TextBoxes
	for _, bi := range boxes {
		if bi >= len(tb.Start) || bi >= len(tb.Bounds) || len(tb.Bounds[bi]) < 4 {
			continue
		}
		st, ln := tb.Start[bi], tb.Length[bi]
		if st < 0 || st >= int64(len(u)) {
			continue
		}
		end := st + ln
		if end > int64(len(u)) {
			end = int64(len(u))
		}
		b := tb.Bounds[bi]
		a := base
		a.x, a.y, a.w, a.h = b[0], b[1], b[2], b[3]
		a.text = string(utf16.Decode(u[st:end]))
		s.addText(a)
	}
}

func (s *tblSnap) imgAtom(li int, ni int64) {
	src := s.rareStr(s.doc.Nodes.CurrentSourceURL, ni)
	if src == "" {
		src = s.attr(ni, "src")
	}
	if src == "" {
		src = s.attr(ni, "data-src")
	}
	if src == "" {
		return
	}
	b := s.doc.Layout.Bounds[li]
	iw, ih := int(math.Round(b[2])), int(math.Round(b[3]))
	if iw < 4 || ih < 4 { // tracking pixels, spacers
		return
	}
	img := &tblImg{id: shortuuid.New() + "." + s.imgExt, url: resolveURL(src, s.base), w: iw, h: ih, alt: s.attr(ni, "alt")}
	s.imgs = append(s.imgs, img)
	s.atoms = append(s.atoms, tblAtom{x: b[0], y: b[1], w: b[2], h: b[3], img: img, href: s.linkFor(ni)})
}

func tblSizeChars(w float64) int {
	c := int(w / 9)
	if c < 4 {
		c = 4
	}
	if c > 80 {
		c = 80
	}
	return c
}

func (s *tblSnap) options(ni int64, sb *strings.Builder) {
	for _, c := range s.kids[ni] {
		switch s.nodeName(c) {
		case "OPTION":
			sel := ""
			if rareBoolHas(s.doc.Nodes.OptionSelected, c) {
				sel = " SELECTED"
			}
			fmt.Fprintf(sb, "<OPTION%s>%s", sel, tblEsc(strings.TrimSpace(s.gatherText(c))))
		case "OPTGROUP":
			s.options(c, sb)
		}
	}
}

// NOTE: controls are display only for now, form submission is not wired up
func (s *tblSnap) ctrlAtom(li int, ni int64, name string) {
	b := s.doc.Layout.Bounds[li]
	a := tblAtom{x: b[0], y: b[1], w: b[2], h: b[3]}
	nd := s.doc.Nodes
	switch name {
	case "INPUT":
		typ := strings.ToLower(s.attr(ni, "type"))
		if typ == "" {
			typ = "text"
		}
		val := s.rareStr(nd.InputValue, ni)
		if val == "" {
			val = s.attr(ni, "value")
		}
		switch typ {
		case "hidden":
			return
		case "submit", "button", "reset", "image":
			if val == "" {
				val = "Submit"
			}
			a.ctrl = fmt.Sprintf(`<INPUT TYPE="SUBMIT" VALUE="%s">`, tblEsc(val))
		case "checkbox", "radio":
			chk := ""
			if rareBoolHas(nd.InputChecked, ni) {
				chk = " CHECKED"
			}
			a.ctrl = fmt.Sprintf(`<INPUT TYPE="%s"%s>`, strings.ToUpper(typ), chk)
		case "password":
			a.ctrl = fmt.Sprintf(`<INPUT TYPE="PASSWORD" SIZE="%d">`, tblSizeChars(b[2]))
		default:
			a.ctrl = fmt.Sprintf(`<INPUT TYPE="TEXT" VALUE="%s" SIZE="%d">`, tblEsc(val), tblSizeChars(b[2]))
		}
	case "TEXTAREA":
		rows := int(b[3] / 18)
		if rows < 2 {
			rows = 2
		}
		a.ctrl = fmt.Sprintf(`<TEXTAREA ROWS="%d" COLS="%d">%s</TEXTAREA>`, rows, tblSizeChars(b[2]), tblEsc(s.rareStr(nd.TextValue, ni)))
	case "SELECT":
		var opts strings.Builder
		s.options(ni, &opts)
		a.ctrl = "<SELECT>" + opts.String() + "</SELECT>"
	case "BUTTON":
		lbl := s.gatherText(ni)
		if lbl == "" {
			lbl = "Button"
		}
		a.ctrl = fmt.Sprintf(`<INPUT TYPE="SUBMIT" VALUE="%s">`, tblEsc(lbl))
	}
	if a.ctrl != "" {
		s.atoms = append(s.atoms, a)
	}
}

func bboxOf(a []tblAtom) tblBox {
	x0, y0 := math.Inf(1), math.Inf(1)
	x1, y1 := math.Inf(-1), math.Inf(-1)
	for _, t := range a {
		x0 = math.Min(x0, t.x)
		y0 = math.Min(y0, t.y)
		x1 = math.Max(x1, t.x+t.w)
		y1 = math.Max(y1, t.y+t.h)
	}
	return tblBox{x0, y0, x1 - x0, y1 - y0}
}

// smallest recorded background rect fully containing the region
func (s *tblSnap) findBg(bx tblBox) string {
	if bx.h < 8 {
		return ""
	}
	x0, y0, x1, y1 := bx.x+4, bx.y+4, bx.x+bx.w-4, bx.y+bx.h-4
	if x1 <= x0 {
		x0, x1 = bx.x, bx.x+bx.w
	}
	if y1 <= y0 {
		y0, y1 = bx.y, bx.y+bx.h
	}
	for _, g := range s.bgs {
		if g.x <= x0 && g.y <= y0 && g.x+g.w >= x1 && g.y+g.h >= y1 {
			return g.color
		}
	}
	return ""
}

// split atoms into segments along y (horiz=false) or x (horiz=true)
// wherever a gap of at least gap pixels crosses the whole region
func cutSegs(a []tblAtom, gap float64, horiz bool) [][]tblAtom {
	sorted := append([]tblAtom(nil), a...)
	pos := func(t tblAtom) (float64, float64) {
		if horiz {
			return t.x, t.w
		}
		return t.y, t.h
	}
	sort.Slice(sorted, func(i, j int) bool {
		pi, _ := pos(sorted[i])
		pj, _ := pos(sorted[j])
		return pi < pj
	})
	var segs [][]tblAtom
	var cur []tblAtom
	edge := math.Inf(-1)
	for _, t := range sorted {
		p, l := pos(t)
		if len(cur) > 0 && p >= edge+gap {
			segs = append(segs, cur)
			cur = nil
		}
		cur = append(cur, t)
		if p+l > edge {
			edge = p + l
		}
	}
	if len(cur) > 0 {
		segs = append(segs, cur)
	}
	return segs
}

func emitAtom(a tblAtom, bgLum int) string {
	if a.ctrl != "" {
		return a.ctrl
	}
	if a.img != nil {
		if !a.img.ok {
			if a.img.alt != "" {
				return "[" + tblEsc(a.img.alt) + "]"
			}
			return ""
		}
		alt := ""
		if a.img.alt != "" {
			alt = fmt.Sprintf(` ALT="%s"`, tblEsc(a.img.alt))
		}
		im := fmt.Sprintf(`<IMG SRC="%s%s" WIDTH="%d" HEIGHT="%d" BORDER="0"%s>`, imgZpfx, a.img.id, a.img.w, a.img.h, alt)
		if a.href != "" {
			return fmt.Sprintf(`<A HREF="%s">%s</A>`, a.href, im)
		}
		return im
	}
	t := tblEsc(a.text)
	var fa []string
	if a.fontSize != 3 {
		fa = append(fa, fmt.Sprintf(`SIZE="%d"`, a.fontSize))
	}
	// linked text keeps browser link colors, near-black on light bg is default
	if a.colorHex != "" && a.href == "" && !(a.colorDark && bgLum > 128) {
		fa = append(fa, fmt.Sprintf(`COLOR="%s"`, a.colorHex))
	}
	if len(fa) > 0 {
		t = "<FONT " + strings.Join(fa, " ") + ">" + t + "</FONT>"
	}
	if a.bold {
		t = "<B>" + t + "</B>"
	}
	if a.italic {
		t = "<I>" + t + "</I>"
	}
	if a.mono {
		t = "<TT>" + t + "</TT>"
	}
	if a.href != "" {
		t = fmt.Sprintf(`<A HREF="%s">%s</A>`, a.href, t)
	}
	return t
}

// no more cuts possible: group into visual lines, emit inline
func (s *tblSnap) leaf(a []tblAtom, bgLum int) string {
	sorted := append([]tblAtom(nil), a...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].y != sorted[j].y {
			return sorted[i].y < sorted[j].y
		}
		return sorted[i].x < sorted[j].x
	})
	var lines [][]tblAtom
	var cur []tblAtom
	bottom := math.Inf(-1)
	for _, t := range sorted {
		if len(cur) > 0 && t.y >= bottom-2 {
			lines = append(lines, cur)
			cur = nil
			bottom = math.Inf(-1)
		}
		cur = append(cur, t)
		if b := t.y + t.h; b > bottom {
			bottom = b
		}
	}
	if len(cur) > 0 {
		lines = append(lines, cur)
	}
	var out strings.Builder
	first := true
	for _, ln := range lines {
		sort.Slice(ln, func(i, j int) bool { return ln[i].x < ln[j].x })
		var lb strings.Builder
		right := math.Inf(-1)
		for _, t := range ln {
			e := emitAtom(t, bgLum)
			if e == "" {
				continue
			}
			if lb.Len() > 0 && t.x > right+1.5 {
				lb.WriteByte(' ')
			}
			lb.WriteString(e)
			if r := t.x + t.w; r > right {
				right = r
			}
		}
		if lb.Len() == 0 {
			continue
		}
		if !first {
			out.WriteString("<BR>\n")
		}
		out.WriteString(lb.String())
		first = false
	}
	return out.String()
}

// recursive XY-cut: y-gaps become vertical flow, x-gaps become table columns
func (s *tblSnap) render(a []tblAtom, parentBg string, depth int) string {
	if len(a) == 0 {
		return ""
	}
	bgLum := hexLum(parentBg)
	if depth > tblMaxDepth {
		return s.leaf(a, bgLum)
	}
	bx := bboxOf(a)
	if bg := s.findBg(bx); bg != "" && bg != parentBg {
		return fmt.Sprintf(`<TABLE BORDER="0" CELLSPACING="0" CELLPADDING="4" WIDTH="%d"><TR><TD BGCOLOR="%s">%s</TD></TR></TABLE>`,
			int(math.Round(bx.w)), bg, s.render(a, bg, depth+1))
	}
	rows := cutSegs(a, tblRowGap, false)
	var parts []string
	for _, row := range rows {
		cols := cutSegs(row, tblColGap, true)
		if len(cols) == 1 {
			parts = append(parts, s.leaf(row, bgLum))
			continue
		}
		rb := bboxOf(row)
		var tb strings.Builder
		fmt.Fprintf(&tb, `<TABLE BORDER="0" CELLSPACING="0" CELLPADDING="0" WIDTH="%d"><TR>`, int(math.Round(rb.w)))
		prev := rb.x
		for _, col := range cols {
			cb := bboxOf(col)
			if gap := cb.x - prev; gap >= tblColGap {
				fmt.Fprintf(&tb, `<TD WIDTH="%d">&nbsp;</TD>`, int(math.Round(gap)))
			}
			fmt.Fprintf(&tb, `<TD VALIGN="TOP" WIDTH="%d">%s</TD>`, int(math.Round(cb.w)), s.render(col, parentBg, depth+1))
			prev = cb.x + cb.w
		}
		tb.WriteString("</TR></TABLE>")
		parts = append(parts, tb.String())
	}
	var out strings.Builder
	prevTbl := false
	first := true
	for _, p := range parts {
		if p == "" {
			continue
		}
		isTbl := strings.HasPrefix(p, "<TABLE")
		if !first {
			if !isTbl && !prevTbl {
				out.WriteString("<BR>\n")
			} else {
				out.WriteByte('\n')
			}
		}
		out.WriteString(p)
		prevTbl = strings.HasSuffix(p, "</TABLE>")
		first = false
	}
	return out.String()
}

func (rq *wrpReq) captureTable() {
	imgStor.clear()
	log.Printf("Processing table layout conversion for %v", rq.url)
	var h int64
	chromedp.Run(ctx,
		emulation.SetDeviceMetricsOverride(rq.width, 10, 1.0, false),
		chromedp.Location(&rq.url),
		chromedp.ActionFunc(func(c context.Context) error {
			_, _, _, _, _, cs, err := page.GetLayoutMetrics().Do(c)
			if err == nil && cs != nil {
				h = int64(math.Ceil(cs.Height))
			}
			return nil
		}),
	)
	if h < 600 {
		h = 600
	}
	if rq.proxy {
		rq.url = strings.Replace(rq.url, "https://", "http://", 1)
	}
	var docs []*domsnapshot.DocumentSnapshot
	var strs []string
	err := chromedp.Run(ctx,
		emulation.SetDeviceMetricsOverride(rq.width, h+30, 1.0, false),
		waitForRender(),
		chromedp.ActionFunc(func(c context.Context) error {
			var err error
			docs, strs, err = domsnapshot.CaptureSnapshot(tblStyles).Do(c)
			return err
		}),
	)
	if err != nil || len(docs) == 0 {
		log.Printf("Failed to capture DOM snapshot: %v", err)
		http.Error(rq.w, fmt.Sprintf("DOM snapshot failed: %v", err), http.StatusInternalServerError)
		return
	}
	doc := docs[0]
	if doc.Nodes == nil || doc.Layout == nil || doc.TextBoxes == nil {
		http.Error(rq.w, "DOM snapshot is missing nodes/layout", http.StatusInternalServerError)
		return
	}
	s := &tblSnap{
		rq:     rq,
		doc:    doc,
		strs:   strs,
		kids:   make(map[int64][]int64),
		tboxes: make(map[int64][]int),
		pageW:  float64(rq.width),
	}
	baseURL := s.str(doc.BaseURL)
	if baseURL == "" {
		baseURL = rq.url
	}
	s.base, _ = url.Parse(baseURL)
	if s.base == nil {
		s.base, _ = url.Parse(rq.url)
	}
	s.imgExt = rq.imgType
	if s.imgExt == "gip" {
		s.imgExt = "gif"
	}
	switch rq.imgType {
	case "jpg":
		s.imgOpt = int(rq.jQual)
	case "gif":
		s.imgOpt = int(rq.nColors)
	}
	s.linkPfx = fmt.Sprintf("/?m=table&t=%s&s=%d&w=%d&url=", rq.imgType, rq.maxSize, rq.width)
	s.collect()
	log.Printf("Table mode: %d atoms, %d bg rects, %d images for %v", len(s.atoms), len(s.bgs), len(s.imgs), rq.url)

	var wg sync.WaitGroup
	var mu sync.Mutex
	totSize := 0
	for _, im := range s.imgs {
		wg.Add(1)
		go func(im *tblImg) {
			defer wg.Done()
			maxDim := int(rq.maxSize)
			if m := max(im.w, im.h); m < maxDim {
				maxDim = m
			}
			if maxDim < 8 {
				maxDim = 8
			}
			size, _, _, err := fetchImage(im.id, im.url, rq.imgType, maxDim, s.imgOpt)
			if err != nil {
				log.Print(err)
				return
			}
			mu.Lock()
			totSize += size
			mu.Unlock()
			im.ok = true
		}(im)
	}
	wg.Wait()

	body := s.render(s.atoms, s.pageBg, 0)
	bg := s.pageBg
	if bg == "" {
		bg = *bgColor
	}
	log.Printf("Table mode: %d bytes html, %d KB images for %v", len(body), totSize/1024, rq.url)
	if rq.proxy {
		rq.w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(rq.w, "<HTML><HEAD>%s<TITLE>%s</TITLE></HEAD><BODY BGCOLOR=\"%s\">%s</BODY></HTML>",
			rq.baseTag(), rq.url, bg, body)
		return
	}
	rq.printUI(uiParams{
		text:    body,
		bgColor: bg,
		imgSize: fmt.Sprintf("%.0f KB", float32(totSize)/1024.0),
	})
}
