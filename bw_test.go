package main

import (
	"image"
	"image/color"
	"math/rand"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"text/template"

	"github.com/MaxHalford/halfgone"
)

func sameGray(t *testing.T, what string, got, want *image.Gray) {
	t.Helper()
	if got.Rect.Dx() != want.Rect.Dx() || got.Rect.Dy() != want.Rect.Dy() {
		t.Fatalf("%s: size %v, want %v", what, got.Rect, want.Rect)
	}
	for y := 0; y < got.Rect.Dy(); y++ {
		for x := 0; x < got.Rect.Dx(); x++ {
			if g, w := got.Pix[y*got.Stride+x], want.Pix[y*want.Stride+x]; g != w {
				t.Fatalf("%s: pixel %d,%d is %d, want %d", what, x, y, g, w)
			}
		}
	}
}

// toGray gives the grays of halfgone.ImageToGray, for each kind of picture
func TestToGray(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	rgba := image.NewRGBA(image.Rect(0, 0, 37, 29))
	r.Read(rgba.Pix)
	for i := 3; i < len(rgba.Pix); i += 4 {
		rgba.Pix[i] = 255
	}
	nrgba := image.NewNRGBA(image.Rect(0, 0, 37, 29))
	copy(nrgba.Pix, rgba.Pix)
	clear := image.NewNRGBA(image.Rect(0, 0, 37, 29)) // not opaque
	r.Read(clear.Pix)
	gray := image.NewGray(image.Rect(0, 0, 37, 29))
	r.Read(gray.Pix)
	pal := image.NewPaletted(image.Rect(0, 0, 37, 29), color.Palette{color.Black, color.White, color.RGBA{200, 30, 90, 255}})
	for i := range pal.Pix {
		pal.Pix[i] = uint8(r.Intn(3))
	}
	sub := image.Rect(5, 3, 30, 20)
	for name, i := range map[string]image.Image{
		"rgba": rgba, "nrgba": nrgba, "nrgba not opaque": clear, "gray": gray, "paletted": pal,
		"rgba part": rgba.SubImage(sub), "nrgba part": nrgba.SubImage(sub), "gray part": gray.SubImage(sub),
	} {
		want := halfgone.ImageToGray(i)
		w := image.NewGray(image.Rect(0, 0, want.Rect.Dx(), want.Rect.Dy()))
		for y := 0; y < w.Rect.Dy(); y++ {
			for x := 0; x < w.Rect.Dx(); x++ {
				w.SetGray(x, y, want.GrayAt(want.Rect.Min.X+x, want.Rect.Min.Y+y))
			}
		}
		sameGray(t, name, toGray(i), w)
	}
}

// bwNear gives the darkest and lightest of the 3x3 neighbors, from any row
func TestBWNear(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	for _, sz := range [][2]int{{1, 1}, {1, 9}, {9, 1}, {2, 2}, {3, 3}, {17, 13}, {64, 40}} {
		w, h := sz[0], sz[1]
		g := image.NewGray(image.Rect(0, 0, w, h))
		r.Read(g.Pix)
		for y0 := 0; y0 < h; y0 += 1 + h/3 {
			n := newBWNear(g, y0)
			for y := y0; y < h; y++ {
				lo, hi := n.next()
				for x := 0; x < w; x++ {
					a, b := uint8(255), uint8(0)
					for yy := y - 1; yy <= y+1; yy++ {
						for xx := x - 1; xx <= x+1; xx++ {
							if xx < 0 || xx >= w || yy < 0 || yy >= h {
								continue
							}
							v := g.Pix[yy*g.Stride+xx]
							a, b = min(a, v), max(b, v)
						}
					}
					if lo[x] != a || hi[x] != b {
						t.Fatalf("%dx%d from row %d: pixel %d,%d: %d..%d, want %d..%d", w, h, y0, x, y, lo[x], hi[x], a, b)
					}
				}
			}
		}
	}
}

// A page of 200 x 120: white, with the things that the 2 color dither made
// hard to read. fill puts gray v in a box; text draws a word as thin strokes.
type bwPage struct{ g *image.Gray }

func newBWPage() bwPage {
	p := bwPage{image.NewGray(image.Rect(0, 0, 200, 120))}
	p.fill(0, 0, 200, 120, 255)
	return p
}

func (p bwPage) fill(x0, y0, x1, y1 int, v uint8) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			p.g.Pix[y*p.g.Stride+x] = v
		}
	}
}

// strokes 1 pixel wide and 7 high, each 4 pixels: "text" of 10 letters
func (p bwPage) text(x0, y0 int, v uint8) {
	for k := 0; k < 10; k++ {
		p.fill(x0+4*k, y0, x0+4*k+1, y0+7, v)
	}
}

func (p bwPage) isText(t *testing.T, o *image.Gray, what string, x0, y0 int, ink, paper uint8) {
	t.Helper()
	for y := y0; y < y0+7; y++ {
		for x := x0; x < x0+40; x++ {
			want := paper
			if (x-x0)%4 == 0 {
				want = ink
			}
			if got := o.Pix[y*o.Stride+x]; got != want {
				t.Fatalf("%s: pixel %d,%d is %d, want %d", what, x, y, got, want)
			}
		}
	}
}

func (p bwPage) isFlat(t *testing.T, o *image.Gray, what string, x0, y0, x1, y1 int, want uint8) {
	t.Helper()
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			if got := o.Pix[y*o.Stride+x]; got != want {
				t.Fatalf("%s: pixel %d,%d is %d, want %d", what, x, y, got, want)
			}
		}
	}
}

// pics and none: text of any gray is solid black, flat colors have no dots
func TestBWText(t *testing.T) {
	p := newBWPage()
	p.text(10, 5, 0)             // black text
	p.text(10, 20, 62)           // a link: blue
	p.text(10, 35, 168)          // light gray text
	p.fill(5, 50, 195, 51, 204)  // a thin light gray line
	p.fill(100, 2, 195, 45, 240) // a box that is nearly white
	p.fill(5, 60, 95, 80, 98)    // a blue button, white text
	p.text(20, 66, 255)
	p.fill(105, 60, 195, 80, 136) // an orange bar, black text
	p.text(120, 66, 0)
	p.fill(5, 90, 195, 115, 60) // a dark bar, light gray text
	p.text(20, 98, 200)
	for _, how := range []string{bwPics, bwNone} {
		o := toBW(p.g, how)
		p.isText(t, o, how+": black text", 10, 5, 0, 255)
		p.isText(t, o, how+": blue text", 10, 20, 0, 255)
		p.isText(t, o, how+": light gray text", 10, 35, 0, 255)
		p.isFlat(t, o, how+": light gray line", 5, 50, 195, 51, 0)
		p.isFlat(t, o, how+": above the line", 5, 45, 195, 50, 255)
		p.isFlat(t, o, how+": nearly white box", 100, 2, 195, 45, 255)
		p.isText(t, o, how+": white text on blue", 20, 66, 255, 0)
		p.isFlat(t, o, how+": blue button", 6, 61, 19, 79, 0)
		p.isText(t, o, how+": black text on orange", 120, 66, 0, 255)
		p.isFlat(t, o, how+": orange bar, inside", 107, 62, 119, 78, 255)
		p.isFlat(t, o, how+": orange bar, its top side", 106, 60, 194, 61, 0)
		p.isText(t, o, how+": light gray text on dark", 20, 98, 255, 0)
		p.isFlat(t, o, how+": dark bar", 6, 91, 19, 114, 0)
	}
}

// A flat color is one color, also in the middle of the grays, where the
// dither of all makes dots
func TestBWFlat(t *testing.T) {
	g := image.NewGray(image.Rect(0, 0, 96, 64))
	for _, v := range []uint8{0, 60, 127, 128, 200, 255} {
		for i := range g.Pix {
			g.Pix[i] = v
		}
		want := uint8(255)
		if v < 128 {
			want = 0
		}
		for _, how := range []string{bwPics, bwNone} {
			for i, got := range toBW(g, how).Pix {
				if got != want {
					t.Fatalf("%s: gray %d: pixel %d is %d, want %d", how, v, i, got, want)
				}
			}
		}
	}
}

// a photo: grays that change softly, with noise
func bwPhotoPic(w, h int) *image.Gray {
	r := rand.New(rand.NewSource(3))
	g := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			g.Pix[y*g.Stride+x] = uint8(60 + 120*x/w + r.Intn(7))
		}
	}
	return g
}

func bwMean(g *image.Gray, x0, y0, x1, y1 int) int {
	s := 0
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			s += int(g.Pix[y*g.Stride+x])
		}
	}
	return s / ((x1 - x0) * (y1 - y0))
}

// pics dithers a photo: each part is as light as it was. none does not.
func TestBWPhoto(t *testing.T) {
	g := bwPhotoPic(256, 96)
	o := toBW(g, bwPics)
	for x := 16; x < 240; x += 32 {
		in := (bwMean(g, x, 16, x+32, 80) - bwLo) * 255 / (bwHi - bwLo)
		if out := bwMean(o, x, 16, x+32, 80); out < in-12 || out > in+12 {
			t.Errorf("pics: columns %d to %d: mean %d, want about %d", x, x+32, out, in)
		}
	}
	n := toBW(g, bwNone)
	if a, b := bwMean(n, 16, 16, 48, 80), bwMean(n, 208, 16, 240, 80); a != 0 || b != 255 {
		t.Errorf("none: the dark end is %d and the light end %d, want 0 and 255", a, b)
	}
	// a photo in a page: the page around it stays white, its text sharp
	p := newBWPage()
	ph := bwPhotoPic(96, 64)
	for y := 0; y < 64; y++ {
		copy(p.g.Pix[(40+y)*p.g.Stride+90:], ph.Pix[y*ph.Stride:y*ph.Stride+96])
	}
	p.text(10, 5, 0)
	p.text(10, 60, 168)
	o = toBW(p.g, bwPics)
	p.isText(t, o, "black text over a photo", 10, 5, 0, 255)
	p.isText(t, o, "light gray text beside a photo", 10, 60, 0, 255)
	p.isFlat(t, o, "the page beside the photo", 0, 20, 80, 55, 255)
	in := (bwMean(p.g, 100, 50, 176, 94) - bwLo) * 255 / (bwHi - bwLo)
	if out := bwMean(o, 100, 50, 176, 94); out < in-12 || out > in+12 {
		t.Errorf("a photo in a page: mean %d, want about %d", out, in)
	}
}

// all is the dither that WRP always had, and the result of each choice does
// not change with the number of CPUs
func TestBWAll(t *testing.T) {
	p := newBWPage()
	ph := bwPhotoPic(96, 64)
	for y := 0; y < 64; y++ {
		copy(p.g.Pix[(40+y)*p.g.Stride+90:], ph.Pix[y*ph.Stride:y*ph.Stride+96])
	}
	p.text(10, 5, 62)
	rgba := image.NewRGBA(p.g.Rect)
	for i, v := range p.g.Pix {
		rgba.Pix[4*i], rgba.Pix[4*i+1], rgba.Pix[4*i+2], rgba.Pix[4*i+3] = v, v, v, 255
	}
	want := halfgone.FloydSteinbergDitherer{}.Apply(halfgone.ImageToGray(rgba))
	sameGray(t, "all", toBW(rgba, bwAll), want)
	sameGray(t, "an unknown choice", toBW(rgba, "x"), want)

	big := image.NewGray(image.Rect(0, 0, 300, 700))
	r := rand.New(rand.NewSource(4))
	r.Read(big.Pix)
	for y := 100; y < 600; y++ { // a photo in the noise
		copy(big.Pix[y*big.Stride+20:], bwPhotoPic(256, 1).Pix)
	}
	for _, how := range []string{bwPics, bwNone} {
		old := runtime.GOMAXPROCS(1)
		one := toBW(big, how)
		runtime.GOMAXPROCS(7)
		many := toBW(big, how)
		runtime.GOMAXPROCS(old)
		sameGray(t, how+" with 1 and 7 CPUs", many, one)
		for i, v := range one.Pix {
			if v != 0 && v != 255 {
				t.Fatalf("%s: pixel %d is %d, not black or white", how, i, v)
			}
		}
	}
}

// c= in a request with its dither letter, d= and -d for 2 colors with no
// letter, and the C menu: its lines, the one chosen, and no D menu
func TestColorOption(t *testing.T) {
	old, oldC, oldTmpl := *defDither, defGeom.c, htmlTmpl
	defer func() { *defDither, defGeom.c, htmlTmpl = old, oldC, oldTmpl }()
	defGeom.c = 216
	var err error
	if htmlTmpl, err = template.New("wrp.html").Parse(wrpTemplate("wrp.html")); err != nil {
		t.Fatal(err)
	}
	lines := `"256d" "256n" "216n" "216d" "128d" "128n" "16d" "16n" "2p" "2d" "2n"`
	for _, c := range []struct {
		def, query string
		n          int64
		want, menu string
	}{
		{bwAll, "/?t=gif&c=2", 2, bwAll, "2d"},
		{bwAll, "/?t=gif&c=2&d=pics", 2, bwPics, "2p"},
		{bwPics, "/?t=gif&c=2", 2, bwPics, "2p"},
		{bwPics, "/?t=gif&c=2&d=none", 2, bwNone, "2n"},
		{bwPics, "/?t=gif&c=2&d=x", 2, bwPics, "2p"},
		{bwPics, "/?t=gif&c=2d", 2, bwAll, "2d"},
		{bwPics, "/?t=gif&c=2n&d=all", 2, bwNone, "2n"},
		{bwAll, "/?t=gif&c=2p", 2, bwPics, "2p"},
		{bwPics, "/?t=gif&c=256", 256, bwAll, "256d"},
		{bwPics, "/?t=gif&c=256n", 256, bwNone, "256n"},
		{bwPics, "/?t=gif&c=256p", 256, bwAll, "256d"},
		{bwPics, "/?t=gif&c=216", 216, bwNone, "216n"},
		{bwPics, "/?t=gif&c=216d", 216, bwAll, "216d"},
		{bwPics, "/?t=gif&c=216p", 216, bwNone, "216n"},
		{bwPics, "/?t=gif&c=16&d=none", 16, bwAll, "16d"},
		{bwPics, "/?t=gif&c=100n", 100, bwNone, ""},
		{bwPics, "/?t=gif&c=x", 216, bwNone, "216n"},
		{bwPics, "/?t=gif&c=999d", 216, bwNone, "216n"},
		{bwPics, "/?t=png&c=128n", 128, bwNone, ""},
	} {
		*defDither = c.def
		rr := httptest.NewRecorder()
		rq := wrpReq{r: httptest.NewRequest("GET", c.query, nil), w: rr}
		rq.parseForm()
		if rq.nColors != c.n || rq.dither != c.want {
			t.Errorf("-d %s, %s: %d colors, dither %q, want %d, %q", c.def, c.query, rq.nColors, rq.dither, c.n, c.want)
		}
		if v := colorVal(rq.nColors, rq.dither); c.menu != "" && v != c.menu {
			t.Errorf("-d %s, %s: the C menu's value is %q, want %q", c.def, c.query, v, c.menu)
		}
		rq.printUI(uiParams{})
		body := rr.Body.String()
		if strings.Contains(body, `NAME="d"`) {
			t.Errorf("-d %s, %s: a D menu", c.def, c.query)
		}
		gif := strings.Contains(c.query, "t=gif")
		if has := strings.Contains(body, `NAME="c"`); has != gif {
			t.Errorf("-d %s, %s: C menu shown: %v", c.def, c.query, has)
		}
		if !gif {
			continue
		}
		menu := body[strings.Index(body, `NAME="c"`):]
		menu = menu[:strings.Index(menu, "</SELECT>")]
		var got []string
		for _, f := range strings.Split(menu, `<OPTION VALUE=`)[1:] {
			got = append(got, strings.Fields(f)[0])
		}
		if g := strings.Join(got, " "); g != lines {
			t.Errorf("-d %s, %s: the C menu has %s, want %s", c.def, c.query, g, lines)
		}
		n := strings.Count(menu, "SELECTED>")
		if c.menu == "" && n != 0 || c.menu != "" && (n != 1 || !strings.Contains(menu, `VALUE="`+c.menu+`" SELECTED`)) {
			t.Errorf("-d %s, %s: the C menu has %d chosen, want %q", c.def, c.query, n, c.menu)
		}
	}
}
