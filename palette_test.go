package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/color/palette"
	"image/draw"
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

// n (no dither): the nearest color of the palette for each pixel, as Set does
func TestToPaletteSame(t *testing.T) {
	for _, nrgba := range []bool{false, true} {
		for _, n := range []int{4, 16, 256} {
			i := palTestImage(300, 200, nrgba)
			want := oldGifPalette(i, n)
			got, ok := gifPalette(i, int64(n), bwNone).(*image.Paletted)
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

// checkBands: each band of ditherPalette's rows is what draw.FloydSteinberg
// makes of it
func checkBands(t *testing.T, name string, i image.Image, got *image.Paletted) {
	b := i.Bounds()
	n := ditherBands(b.Dy())
	for w := 0; w < n; w++ {
		y0, y1 := b.Min.Y+b.Dy()*w/n, b.Min.Y+b.Dy()*(w+1)/n
		r := image.Rect(b.Min.X, y0, b.Max.X, y1)
		want := image.NewPaletted(r, got.Palette)
		draw.FloydSteinberg.Draw(want, r, i, r.Min)
		for y := y0; y < y1; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				if got.ColorIndexAt(x, y) != want.ColorIndexAt(x, y) {
					t.Fatalf("%s: band %d of %d, pixel %d,%d is %d, draw.FloydSteinberg's %d",
						name, w, n, x, y, got.ColorIndexAt(x, y), want.ColorIndexAt(x, y))
				}
			}
		}
	}
}

// Colors other than 2 and 216: median cut, then Floyd-Steinberg
func TestGifPaletteDither(t *testing.T) {
	for _, nrgba := range []bool{false, true} {
		for _, n := range []int{4, 16, 128, 256} {
			i := palTestImage(300, 200, nrgba)
			got, ok := gifPalette(i, int64(n), bwAll).(*image.Paletted)
			if !ok {
				t.Fatalf("nrgba %v n %d: not a paletted picture", nrgba, n)
			}
			p := quantize.MedianCutQuantizer{}.Quantize(make([]color.Color, 0, n), i)
			if len(got.Palette) != len(p) {
				t.Fatalf("nrgba %v n %d: %d colors, want %d", nrgba, n, len(got.Palette), len(p))
			}
			checkBands(t, fmt.Sprintf("nrgba %v n %d", nrgba, n), i, got)
		}
		got := gifPalette(i0(nrgba), 216, bwAll).(*image.Paletted)
		if len(got.Palette) != len(palette.WebSafe) {
			t.Fatalf("nrgba %v 216d: %d colors, want the 216 web colors", nrgba, len(got.Palette))
		}
		checkBands(t, fmt.Sprintf("nrgba %v 216d", nrgba), i0(nrgba), got)
	}
}

func i0(nrgba bool) image.Image { return palTestImage(300, 200, nrgba) }

// A flat area in a color of the palette stays flat: no dots in it. A gray
// that is not in the palette becomes dots of its neighbors, as light as it.
func TestDitherFlat(t *testing.T) {
	i := image.NewRGBA(image.Rect(0, 0, 200, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 200; x++ {
			c := color.RGBA{240, 240, 240, 255}
			if x >= 100 {
				c = color.RGBA{128, 128, 128, 255}
			}
			i.SetRGBA(x, y, c)
		}
	}
	p := color.Palette{color.Black, color.RGBA{240, 240, 240, 255}, color.White}
	o := ditherPalette(i, p)
	for y := 0; y < 100; y++ {
		for x := 0; x < 90; x++ {
			if o.ColorIndexAt(x, y) != 1 {
				t.Fatalf("flat area: pixel %d,%d is %d, want 1", x, y, o.ColorIndexAt(x, y))
			}
		}
	}
	var sum, cnt int
	for y := 0; y < 100; y++ {
		for x := 110; x < 200; x++ {
			r, _, _, _ := p[o.ColorIndexAt(x, y)].RGBA()
			sum += int(r >> 8)
			cnt++
		}
	}
	if m := sum / cnt; m < 120 || m > 136 {
		t.Fatalf("gray 128 dithered: mean %d", m)
	}
}

// A whole page: the palette comes from some rows, and the page is dithered
// in bands, each as draw.FloydSteinberg does it.
func TestGifPaletteTall(t *testing.T) {
	i := palTestImage(400, 3000, false)
	if s := palSample(i); s.Bounds().Dx()*s.Bounds().Dy() > palSamplePixels {
		t.Fatalf("sample: %v", s.Bounds())
	}
	got := gifPalette(i, 256, bwAll).(*image.Paletted)
	checkBands(t, "tall", i, got)
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
		got, ok := gifPalette(i, 216, bwNone).(*image.Paletted)
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
