package main

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"math/rand"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAcceptQ(t *testing.T) {
	rq := httptest.NewRequest("GET", "/", nil)
	rq.Header.Set("Accept", "text/html, image/x-abm, IMAGE/X-ABM3;q=0.5 ,image/x-ab8; q=0 ; x=1,image/gif;q=7, */*;q=0.1,,")
	got := acceptQ(rq)
	want := map[string]float64{"text/html": 1, "image/x-abm": 1, "image/x-abm3": 0.5, "image/x-ab8": 0, "image/gif": 1, "*/*": 0.1}
	if len(got) != len(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: q %v, want %v", k, got[k], v)
		}
	}
	if n := len(acceptQ(httptest.NewRequest("GET", "/", nil))); n != 0 {
		t.Errorf("no Accept: %d types", n)
	}
}

// a page of 632 x 60 for the choice tests. bw: a dither (ABM3 is smaller
// than ABM). Else colors: flat, with noise in the lower half if noisy
// (AB8 is then larger than the GIF)
func choicePage(bw, noisy bool) *image.Paletted {
	rnd := rand.New(rand.NewSource(3))
	if bw {
		p := image.NewPaletted(image.Rect(0, 0, 632, 60), color.Palette{color.Black, color.White})
		for i := range p.Pix {
			p.Pix[i] = byte(rnd.Intn(4) / 3)
		}
		return p
	}
	pal := make(color.Palette, 216)
	for i := range pal {
		pal[i] = color.RGBA{uint8(i / 36 * 51), uint8(i / 6 % 6 * 51), uint8(i % 6 * 51), 255}
	}
	p := image.NewPaletted(image.Rect(0, 0, 632, 60), pal)
	for i := range p.Pix {
		p.Pix[i] = byte(i / 632 / 8)
		if noisy && i >= 632*30 {
			p.Pix[i] = byte(rnd.Intn(3))
		}
	}
	return p
}

// The slice server sends the type that the client's Accept asks for
func TestSliceServerChoice(t *testing.T) {
	wrpCach.clear()
	wrpCach.addFull("/img/bw.gif", choicePage(true, false), "gif", 75)
	wrpCach.addFull("/img/flat.gif", choicePage(false, false), "gif", 75)
	wrpCach.addFull("/img/noisy.gif", choicePage(false, true), "gif", 75)
	wrpCach.addFull("/img/page.png", choicePage(false, false), "png", 75)
	const (
		abm  = "image/x-abm"
		abm3 = "image/x-abm3"
		ab8  = "image/x-ab8"
		gf   = "image/gif"
	)
	for _, c := range []struct {
		path, accept, want string
		fast               bool // X-Fast-Length comes
	}{
		// black and white
		{"bw", "", gf, false},
		{"bw", "text/html, image/gif", gf, false},
		{"bw", "image/*, */*", gf, false},
		{"bw", "text/html, image/x-abm, image/gif", abm, false}, // Archaic 0.11
		{"bw", "text/html, image/x-abm3, image/gif", abm3, false},
		{"bw", "text/html, image/x-abm, image/x-abm3;q=0.5, image/gif", abm, false},
		{"bw", "text/html, image/x-abm, image/x-abm3, image/gif", abm3, true},
		{"bw", "text/html, image/x-abm;q=0.5, image/x-abm3, image/gif", abm3, false},
		{"bw", "text/html, image/x-abm;q=0, image/x-abm3;q=0", gf, false},
		{"bw", "image/x-ab8", gf, false},
		// colors
		{"flat", "", gf, false},
		{"flat", "text/html, image/x-abm, image/gif", gf, false}, // Archaic 0.11
		{"flat", "image/x-abm, image/x-abm3", gf, false},
		{"flat", "image/x-ab8", ab8, false},
		{"flat", "image/x-ab8, image/gif;q=0.5", ab8, false},
		{"flat", "image/x-ab8, image/gif", ab8, true}, // the smaller one
		{"noisy", "image/x-ab8, image/gif;q=0.5", ab8, false},
		{"noisy", "image/x-ab8, image/gif", gf, true}, // the smaller one
		{"noisy", "image/x-ab8;q=0.5, image/gif", gf, false},
		{"noisy", "image/x-ab8;q=0", gf, false},
	} {
		rr := httptest.NewRecorder()
		rq := httptest.NewRequest("GET", "/img/"+c.path+".gif?y=10&h=40", nil)
		if c.accept != "" {
			rq.Header.Set("Accept", c.accept)
		}
		imgServerMap(rr, rq)
		typ, fast := rr.Header().Get("Content-Type"), rr.Header().Get(fastLenHeader)
		if typ != c.want || (fast != "") != c.fast {
			t.Errorf("%s, Accept %q: got %s (%s %q), want %s (fast length: %v)", c.path, c.accept, typ, fastLenHeader, fast, c.want, c.fast)
			continue
		}
		if rr.Header().Get("Vary") != "Accept" {
			t.Errorf("%s, Accept %q: no Vary: Accept", c.path, c.accept)
		}
		b := rr.Body.Bytes()
		if n, _ := strconv.Atoi(rr.Header().Get("Content-Length")); n != len(b) {
			t.Errorf("%s, Accept %q: Content-Length %d, %d bytes", c.path, c.accept, n, len(b))
		}
		magic := map[string]string{gf: "GIF8", abm: "ABM1", abm3: "ABM3", ab8: "AB81"}[typ]
		if len(b) < 4 || string(b[:4]) != magic {
			t.Errorf("%s, Accept %q: %s starts with %q", c.path, c.accept, typ, b[:min(4, len(b))])
		}
		if c.fast {
			// the fast length is the size that comes with a lower q for the other type
			r2 := httptest.NewRecorder()
			q2 := httptest.NewRequest("GET", "/img/"+c.path+".gif?y=10&h=40", nil)
			q2.Header.Set("Accept", map[string]string{"bw": "image/x-abm", "flat": "image/x-ab8", "noisy": "image/x-ab8"}[c.path])
			imgServerMap(r2, q2)
			if n, _ := strconv.Atoi(fast); n != r2.Body.Len() || n < len(b) {
				t.Errorf("%s: %s %s, the fast type is %d bytes, this one %d", c.path, fastLenHeader, fast, r2.Body.Len(), len(b))
			}
		}
	}
	// a page that is not a GIF stays as it is
	rr := httptest.NewRecorder()
	rq := httptest.NewRequest("GET", "/img/page.png?y=0&h=20", nil)
	rq.Header.Set("Accept", "image/x-ab8, image/x-abm, image/x-abm3")
	imgServerMap(rr, rq)
	if typ := rr.Header().Get("Content-Type"); typ != "image/png" || rr.Header().Get("Vary") != "" {
		t.Errorf("a PNG page: got %s (Vary %q)", typ, rr.Header().Get("Vary"))
	}
}

// What comes is the slice: each type gives the rows asked for
func TestSliceServerPixels(t *testing.T) {
	wrpCach.clear()
	bw, col := choicePage(true, false), choicePage(false, true)
	wrpCach.addFull("/img/bw.gif", bw, "gif", 75)
	wrpCach.addFull("/img/col.gif", col, "gif", 75)
	get := func(path, accept string) []byte {
		rr := httptest.NewRecorder()
		rq := httptest.NewRequest("GET", path+"?y=7&h=33", nil)
		rq.Header.Set("Accept", accept)
		imgServerMap(rr, rq)
		return rr.Body.Bytes()
	}
	bits, bpl := abmBits(cut(bw, 7, 33).(*image.Paletted))
	if f := get("/img/bw.gif", "image/x-abm3"); len(f) < 10 {
		t.Errorf("ABM3: %d bytes", len(f))
	} else if got, err := abm3Unpack(f[10:], bpl, 632, 33); err != nil || !bytes.Equal(got, bits) {
		t.Errorf("ABM3: not the slice's rows (%v)", err)
	}
	f := get("/img/col.gif", "image/x-ab8")
	want := col.Pix[7*632 : 40*632]
	if len(f) < 9+216*3 || f[8] != 215 {
		t.Fatalf("AB8: %d bytes", len(f))
	}
	if got, err := ab8Unpack(f[9+216*3:], 632, len(want)); err != nil || !bytes.Equal(got, want) {
		t.Errorf("AB8: not the slice's rows (%v)", err)
	}
	g, err := gif.Decode(bytes.NewReader(get("/img/col.gif", "image/gif")))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(want); i += 97 { // AB8's colors are the GIF's
		r, gr, b, _ := g.At(i%632, i/632).RGBA()
		if f[9+3*int(want[i])] != byte(r>>8) || f[10+3*int(want[i])] != byte(gr>>8) || f[11+3*int(want[i])] != byte(b>>8) {
			t.Fatalf("pixel %d: AB8's color is not the GIF's", i)
		}
	}
}

// Test pictures for a client, as files:
//
//	WRP_SAMPLES=dir go test -run TestSamples
//
// For each NAME.gif in dir it writes NAME.ab8, or, if the GIF has only
// black and white pixels, NAME.ab3 (ABM3).
func TestSamples(t *testing.T) {
	dir := os.Getenv("WRP_SAMPLES")
	if dir == "" {
		t.Skip("WRP_SAMPLES is not set")
	}
	names, _ := filepath.Glob(filepath.Join(dir, "*.gif"))
	for _, name := range names {
		f, err := os.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		i, err := gif.Decode(f)
		f.Close()
		p, ok := i.(*image.Paletted)
		if err != nil || !ok {
			t.Errorf("%s: %v", name, err)
			continue
		}
		p = cut(p, 0, p.Rect.Dy()).(*image.Paletted)
		// only black and white pixels: the same picture with 2 colors
		bw := image.NewPaletted(p.Rect, color.Palette{color.White, color.Black})
		for k, v := range p.Pix {
			switch r, g, b, _ := p.Palette[v].RGBA(); {
			case r == 0 && g == 0 && b == 0:
				bw.Pix[k] = 1
			case r != 0xffff || g != 0xffff || b != 0xffff:
				bw = nil
			}
			if bw == nil {
				break
			}
		}
		var b bytes.Buffer
		out := strings.TrimSuffix(name, ".gif")
		if bw != nil {
			err = encodeABM3(&b, bw)
			out += ".ab3"
		} else {
			err = encodeAB8(&b, p)
			out += ".ab8"
		}
		if err == nil {
			err = os.WriteFile(out, b.Bytes(), 0o644)
		}
		if err != nil {
			t.Errorf("%s: %v", out, err)
		}
		t.Logf("%s: %d bytes", out, b.Len())
	}
}
