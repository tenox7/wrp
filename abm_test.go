package main

import (
	"bytes"
	"image"
	"image/color"
	"math/rand"
	"net/http/httptest"
	"testing"
)

// abmUnpack undoes abmPack, as a client does: n bytes of rows, each row
// bpl bytes
func abmUnpack(t *testing.T, d []byte, n, bpl int) []byte {
	o := make([]byte, 0, n)
	for i := 0; i < len(d) && len(o) < n; {
		c := int(d[i])
		i++
		k := c&63 + 1
		switch {
		case c < 0x40:
			o = append(o, d[i:i+k]...)
			i += k
		case c < 0x80:
			o = append(o, make([]byte, k)...)
		default:
			back := bpl
			if c >= 0xe0 {
				k = c&31 + 3
				back = int(d[i])<<8 + int(d[i+1]) + 1
				i += 2
			} else if c >= 0xc0 {
				k = c&31 + 2
				back = int(d[i]) + 1
				i++
			}
			if back > len(o) {
				t.Fatalf("token at %d: %d bytes back, only %d there", i, back, len(o))
			}
			for ; k > 0; k-- {
				o = append(o, o[len(o)-back])
			}
		}
	}
	return o
}

func TestABMPack(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	const bpl = 13
	pages := map[string][]byte{
		"empty": {},
		"one":   {0x55},
		"white": make([]byte, 40*bpl),
		"black": bytes.Repeat([]byte{0xff}, 40*bpl),
		"rows":  bytes.Repeat([]byte{0, 0, 0x7e, 0x42, 0x42, 0x7e, 0, 0, 0x18, 0x18, 0, 0, 0}, 30),
	}
	noise := make([]byte, 50*bpl)
	rnd.Read(noise)
	pages["noise"] = noise
	// text-like: a few "letters" at random places on white, some lines twice
	text := make([]byte, 200*bpl)
	for k := 0; k < 300; k++ {
		at := rnd.Intn(len(text) - 4)
		copy(text[at:], []byte{0x3c, 0x66, 0x7e, 0x60}[:1+rnd.Intn(4)])
	}
	copy(text[100*bpl:], text[20*bpl:60*bpl])
	pages["text"] = text
	for name, d := range pages {
		p := abmPack(d, bpl)
		if got := abmUnpack(t, p, len(d), bpl); !bytes.Equal(got, d) {
			t.Errorf("%s: %d bytes packed to %d do not come back the same", name, len(d), len(p))
		}
		if len(p) > len(d)+len(d)/64+1 {
			t.Errorf("%s: %d bytes packed to %d: more than as they are", name, len(d), len(p))
		}
	}
	if n := len(abmPack(pages["white"], bpl)); n > 12 {
		t.Errorf("white: %d bytes, want 12 or fewer", n)
	}
}

// The slice server gives ABM only to a client that asks for it, and only
// for a black and white picture
func TestSliceServerABM(t *testing.T) {
	wrpCach.clear()
	wrpCach.addFull("/img/t.gif", paletted(stripes()), "gif", 75)
	get := func(u, accept string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		rq := httptest.NewRequest("GET", u, nil)
		if accept != "" {
			rq.Header.Set("Accept", accept)
		}
		imgServerMap(rr, rq)
		return rr
	}
	for _, a := range []string{"", "text/html, image/gif", "image/x-abm2, image/x-abmx"} {
		if rr := get("/img/t.gif?y=2&h=4", a); rr.Header().Get("Content-Type") != "image/gif" {
			t.Errorf("Accept %q: got %s, want a GIF", a, rr.Header().Get("Content-Type"))
		}
	}
	// 3 pixels wide, rows 3 to 6 of the stripes: black, white, black, white
	rr := get("/img/t.gif?y=3&h=4", "text/html, image/x-abm, image/gif")
	if rr.Header().Get("Content-Type") != abmType {
		t.Fatalf("ABM asked: got %s", rr.Header().Get("Content-Type"))
	}
	b := rr.Body.Bytes()
	want := []byte{'A', 'B', 'M', '1', 0, 3, 0, 4, 1, 0}
	if len(b) < 10 || !bytes.Equal(b[:10], want) {
		t.Fatalf("header % x, want % x", b, want)
	}
	if got := abmUnpack(t, b[10:], 4, 1); !bytes.Equal(got, []byte{0xe0, 0, 0xe0, 0}) {
		t.Errorf("rows % x, want e0 00 e0 00", got)
	}
	// a picture with colors stays a GIF
	c := image.NewPaletted(image.Rect(0, 0, 3, 10), color.Palette{color.Black, color.White, color.RGBA{255, 0, 0, 255}})
	wrpCach.addFull("/img/c.gif", c, "gif", 75)
	if rr := get("/img/c.gif", "image/x-abm"); rr.Header().Get("Content-Type") != "image/gif" {
		t.Errorf("colors: got %s, want a GIF", rr.Header().Get("Content-Type"))
	}
}
