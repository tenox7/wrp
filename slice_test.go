package main

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"net/http/httptest"
	"testing"
)

// A page 3 pixels wide and 10 high, as the 2 color dither leaves it:
// gray 0 or 255. Even rows are white, odd rows black.
func stripes() *image.Gray {
	g := image.NewGray(image.Rect(0, 0, 3, 10))
	for y := 0; y < 10; y += 2 {
		for x := 0; x < 3; x++ {
			g.SetGray(x, y, color.Gray{Y: 255})
		}
	}
	return g
}

func white(c color.Color) bool {
	r, g, b, _ := c.RGBA()
	return r > 0x8000 && g > 0x8000 && b > 0x8000
}

func TestPalettedCut(t *testing.T) {
	p, ok := paletted(stripes()).(*image.Paletted)
	if !ok || len(p.Palette) != 2 {
		t.Fatalf("paletted: want a 2 color paletted picture, got %T", paletted(stripes()))
	}
	s := cut(p, 3, 4)
	if s == nil || s.Bounds() != image.Rect(0, 0, 3, 4) {
		t.Fatalf("cut 3,4: bounds %v", s.Bounds())
	}
	for y := 0; y < 4; y++ {
		if got, want := white(s.At(1, y)), (3+y)%2 == 0; got != want {
			t.Errorf("cut 3,4: row %d white %v, want %v", y, got, want)
		}
	}
	if s := cut(p, 8, 5); s == nil || s.Bounds().Dy() != 2 {
		t.Errorf("cut 8,5: want the last 2 rows, got %v", s)
	}
	if s := cut(p, 10, 5); s != nil {
		t.Errorf("cut 10,5: want nil, got %v", s.Bounds())
	}
	rgba := image.NewRGBA(image.Rect(0, 0, 3, 10))
	if s := cut(rgba, 4, 3); s == nil || s.Bounds() != image.Rect(0, 0, 3, 3) {
		t.Errorf("cut of RGBA 4,3: bounds %v", s.Bounds())
	}
}

func TestSliceServer(t *testing.T) {
	wrpCach.clear()
	wrpCach.addFull("/img/t.gif", paletted(stripes()), "gif", 75)
	get := func(u string) (*httptest.ResponseRecorder, image.Image) {
		rr := httptest.NewRecorder()
		imgServerMap(rr, httptest.NewRequest("GET", u, nil))
		if rr.Code != 200 {
			return rr, nil
		}
		i, err := gif.Decode(bytes.NewReader(rr.Body.Bytes()))
		if err != nil {
			t.Fatalf("%s: not a GIF: %v", u, err)
		}
		return rr, i
	}
	rr, i := get("/img/t.gif?y=2&h=5")
	if i == nil || i.Bounds() != image.Rect(0, 0, 3, 5) {
		t.Fatalf("slice 2,5: code %d, picture %v", rr.Code, i)
	}
	if !white(i.At(0, 0)) || white(i.At(0, 1)) {
		t.Errorf("slice 2,5: rows are not the page's rows 2 and 3")
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/gif" {
		t.Errorf("slice: Content-Type %q", ct)
	}
	if _, i = get("/img/t.gif"); i == nil || i.Bounds().Dy() != 10 {
		t.Errorf("whole page: want 10 rows, got %v", i)
	}
	if rr, _ = get("/img/t.gif?y=20&h=5"); rr.Code != 404 {
		t.Errorf("rows past the end: code %d, want 404", rr.Code)
	}
	wrpCach.clear()
}
