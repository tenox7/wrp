package main

import (
	"bytes"
	"image"
	"image/color"
	"image/color/palette"
	"image/gif"
	"math/rand"
	"testing"

	"github.com/ericpauley/go-quantize/quantize"
	"github.com/tenox7/gip"
)

// A screen-like picture: flat areas, text-like specks and a photo-like part
// with many colors.
func palTestImage(w, h int, nrgba bool) image.Image {
	r := rand.New(rand.NewSource(1))
	var i draw2
	if nrgba {
		i = image.NewNRGBA(image.Rect(0, 0, w, h))
	} else {
		i = image.NewRGBA(image.Rect(0, 0, w, h))
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var c color.RGBA
			switch {
			case y < h/3:
				c = color.RGBA{240, 240, 240, 255}
				if r.Intn(9) == 0 {
					c = color.RGBA{20, 20, 30, 255}
				}
			case y < 2*h/3:
				c = color.RGBA{uint8(r.Intn(256)), uint8(x), uint8(y), 255}
			default:
				c = color.RGBA{uint8(x * 3), 90, uint8(255 - y), uint8(128 + r.Intn(128))}
				c.R, c.G, c.B = c.R/2, c.G/2, c.B/2 // keep it valid premultiplied
			}
			i.Set(x, y, c)
		}
	}
	return i
}

type draw2 interface {
	image.Image
	Set(x, y int, c color.Color)
}

// what gifPalette did before: median cut over all pixels, then Set per pixel
func oldGifPalette(i image.Image, n int) *image.Paletted {
	p := quantize.MedianCutQuantizer{}.Quantize(make([]color.Color, 0, n), i)
	b := i.Bounds()
	o := image.NewPaletted(b, p)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			o.Set(x, y, i.At(x, y))
		}
	}
	return o
}

func TestGifPaletteSame(t *testing.T) {
	for _, nrgba := range []bool{false, true} {
		for _, n := range []int{4, 16, 256} {
			i := palTestImage(300, 200, nrgba)
			want := oldGifPalette(i, n)
			got, ok := gifPalette(i, int64(n), bwAll).(*image.Paletted)
			if !ok {
				t.Fatalf("nrgba %v n %d: not a paletted picture", nrgba, n)
			}
			if len(got.Palette) != len(want.Palette) {
				t.Fatalf("nrgba %v n %d: %d colors, want %d", nrgba, n, len(got.Palette), len(want.Palette))
			}
			for k := range want.Pix {
				if got.Pix[k] != want.Pix[k] {
					t.Fatalf("nrgba %v n %d: pixel %d is %d, want %d", nrgba, n, k, got.Pix[k], want.Pix[k])
				}
			}
		}
	}
}

// A whole page: the palette comes from some rows, every pixel still gets
// the nearest color of that palette.
func TestGifPaletteTall(t *testing.T) {
	i := palTestImage(400, 6000, false)
	if s := palSample(i); s.Bounds().Dx()*s.Bounds().Dy() > palSamplePixels {
		t.Fatalf("sample: %v", s.Bounds())
	}
	got := gifPalette(i, 256, bwAll).(*image.Paletted)
	for _, pt := range []image.Point{{0, 0}, {399, 5999}, {123, 2500}, {7, 4100}} {
		want := got.Palette.Index(i.At(pt.X, pt.Y))
		if int(got.ColorIndexAt(pt.X, pt.Y)) != want {
			t.Errorf("%v: index %d, want %d", pt, got.ColorIndexAt(pt.X, pt.Y), want)
		}
	}
}

func BenchmarkGifPaletteTall(b *testing.B) {
	i := palTestImage(792, 20000, false)
	b.ResetTimer()
	for k := 0; k < b.N; k++ {
		gifPalette(i, 256, bwAll)
	}
}

// 216 colors: the web colors, the same pixels as GIP's, on every page
func TestGifPaletteWeb(t *testing.T) {
	for _, nrgba := range []bool{false, true} {
		i := palTestImage(300, 200, nrgba)
		got, ok := gifPalette(i, 216, bwAll).(*image.Paletted)
		if !ok {
			t.Fatalf("nrgba %v: not a paletted picture", nrgba)
		}
		if len(got.Palette) != len(palette.WebSafe) {
			t.Fatalf("nrgba %v: %d colors, want the 216 web colors", nrgba, len(got.Palette))
		}
		for k, c := range palette.WebSafe {
			if got.Palette[k] != c {
				t.Fatalf("nrgba %v: color %d is %v, want %v", nrgba, k, got.Palette[k], c)
			}
		}
		var buf bytes.Buffer
		if err := gip.Encode(&buf, i, nil); err != nil {
			t.Fatal(err)
		}
		g, err := gif.Decode(&buf)
		if err != nil {
			t.Fatal(err)
		}
		want := g.(*image.Paletted)
		for y := 0; y < 200; y++ {
			for x := 0; x < 300; x++ {
				if got.ColorIndexAt(x, y) != want.ColorIndexAt(x, y) {
					t.Fatalf("nrgba %v: pixel %d,%d is %d, GIP's %d", nrgba, x, y,
						got.ColorIndexAt(x, y), want.ColorIndexAt(x, y))
				}
			}
		}
	}
}
