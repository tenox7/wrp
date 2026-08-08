package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"image"
	"io"
	"testing"
)

// A frame with flat areas, hard two colour edges and smooth gradients, so every
// hextile subencoding gets exercised.
func testFrame(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			o := img.PixOffset(x, y)
			var r, g, b byte
			switch {
			case y < h/4: // flat
				r, g, b = 0x20, 0x40, 0x80
			case y < h/2: // two colour, like text
				if (x/3+y/5)%2 == 0 {
					r, g, b = 0xff, 0xff, 0xff
				}
			case y < 3*h/4: // horizontal gradient
				r, g, b = byte(x*255/w), 0x80, byte(255-x*255/w)
			default: // busy
				r, g, b = byte(x*7+y*3), byte(x*13+y*5), byte(x*29+y*11)
			}
			img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = r, g, b, 0xff
		}
	}
	return img
}

func rectPayload(t *testing.T, rect []byte) (w, h int, enc int32, payload []byte) {
	t.Helper()
	if len(rect) < 12 {
		t.Fatalf("rect too short: %d bytes", len(rect))
	}
	w = int(binary.BigEndian.Uint16(rect[4:]))
	h = int(binary.BigEndian.Uint16(rect[6:]))
	enc = int32(binary.BigEndian.Uint32(rect[8:]))
	return w, h, enc, rect[12:]
}

func decodeZlib(t *testing.T, payload []byte, n int) []byte {
	t.Helper()
	l := int(binary.BigEndian.Uint32(payload))
	zr, err := zlib.NewReader(bytes.NewReader(payload[4 : 4+l]))
	if err != nil {
		t.Fatalf("zlib reader: %v", err)
	}
	out := make([]byte, n)
	if _, err := io.ReadFull(zr, out); err != nil {
		t.Fatalf("zlib read: %v", err)
	}
	return out
}

// Reference hextile decoder, deliberately written straight from the spec
func decodeHextile(t *testing.T, payload []byte, w, h, bpp int) []byte {
	t.Helper()
	out := make([]byte, w*h*bpp)
	p := 0
	take := func(n int) []byte {
		if p+n > len(payload) {
			t.Fatalf("hextile payload truncated at %d, wanted %d more", p, n)
		}
		b := payload[p : p+n]
		p += n
		return b
	}
	put := func(x, y int, px []byte) {
		if x >= w || y >= h {
			t.Fatalf("hextile wrote outside the rect at %d,%d", x, y)
		}
		copy(out[(y*w+x)*bpp:], px)
	}
	var bg, fg []byte
	for ty := 0; ty < h; ty += vncHexTile {
		th := min(vncHexTile, h-ty)
		for tx := 0; tx < w; tx += vncHexTile {
			tw := min(vncHexTile, w-tx)
			sub := take(1)[0]
			if sub&1 != 0 { // raw
				d := take(tw * th * bpp)
				for j := 0; j < th; j++ {
					for i := 0; i < tw; i++ {
						put(tx+i, ty+j, d[(j*tw+i)*bpp:(j*tw+i+1)*bpp])
					}
				}
				continue
			}
			if sub&2 != 0 {
				bg = append([]byte(nil), take(bpp)...)
			}
			if sub&4 != 0 {
				fg = append([]byte(nil), take(bpp)...)
			}
			for j := 0; j < th; j++ {
				for i := 0; i < tw; i++ {
					put(tx+i, ty+j, bg)
				}
			}
			if sub&8 == 0 {
				continue
			}
			for s := int(take(1)[0]); s > 0; s-- {
				col := fg
				if sub&16 != 0 {
					col = append([]byte(nil), take(bpp)...)
				}
				xy, wh := take(1)[0], take(1)[0]
				sx, sy := int(xy>>4), int(xy&0xf)
				sw, sh := int(wh>>4)+1, int(wh&0xf)+1
				if sx+sw > tw || sy+sh > th {
					t.Fatalf("hextile subrect %d,%d %dx%d escapes a %dx%d tile", sx, sy, sw, sh, tw, th)
				}
				for j := 0; j < sh; j++ {
					for i := 0; i < sw; i++ {
						put(tx+sx+i, ty+sy+j, col)
					}
				}
			}
		}
	}
	if p != len(payload) {
		t.Fatalf("hextile left %d bytes unconsumed", len(payload)-p)
	}
	return out
}

// Raw, hextile and zlib are all lossless, so for one pixel format they must
// each reproduce exactly the same bytes.
func TestEncodingsAgree(t *testing.T) {
	img := testFrame(200, 140)
	r := img.Bounds()

	for _, tc := range []struct {
		name    string
		pf      vncPixFmt
		indexed bool
	}{
		{"rgb888", vncDefaultPF, false},
		{"bgr233", vncBGR233, false},
		{"colormap", vncBGR233, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pal *vncPal
			if tc.indexed {
				pal = vncBuildPal(img, 216, 1)
			}
			mk := func(e vncEnc) *vncClient {
				return &vncClient{pf: tc.pf, indexed: tc.indexed, enc: e}
			}
			want := mk(encRaw).pixels(img, pal, r)
			if n := r.Dx() * r.Dy() * tc.pf.bytes(); len(want) != n {
				t.Fatalf("pixels returned %d bytes, want %d", len(want), n)
			}

			_, _, enc, payload := rectPayload(t, mk(encRaw).rectRaw(0, 0, want, r.Dx(), r.Dy()))
			if enc != rfbEncodingRaw {
				t.Fatalf("raw rect carries encoding %d", enc)
			}
			if !bytes.Equal(payload, want) {
				t.Fatal("raw payload does not match the pixel buffer")
			}

			cz := mk(encZlib)
			_, _, enc, payload = rectPayload(t, cz.rectZlib(0, 0, cz.pixels(img, pal, r), r.Dx(), r.Dy()))
			if enc != rfbEncodingZlib {
				t.Fatalf("zlib rect carries encoding %d", enc)
			}
			if got := decodeZlib(t, payload, len(want)); !bytes.Equal(got, want) {
				t.Fatal("zlib round trip differs from raw")
			}

			ch := mk(encHextile)
			_, _, enc, payload = rectPayload(t, ch.rectHextile(0, 0, ch.pixels(img, pal, r), r.Dx(), r.Dy()))
			if enc != rfbEncodingHextile {
				t.Fatalf("hextile rect carries encoding %d", enc)
			}
			got := decodeHextile(t, payload, r.Dx(), r.Dy(), tc.pf.bytes())
			if !bytes.Equal(got, want) {
				n := 0
				for i := range want {
					if want[i] != got[i] {
						n++
					}
				}
				t.Fatalf("hextile round trip differs from raw in %d of %d bytes", n, len(want))
			}
		})
	}
}

// A tight rect may not be wider than 2048, so wider ones have to come out as
// several rects that still tile the original exactly.
func TestTightRectSplit(t *testing.T) {
	img := testFrame(2600, 40)
	c := &vncClient{pf: vncDefaultPF, enc: encTight}
	rects := c.encodeRects(img, nil, img.Bounds(), 0, vncChromeH, nil)
	if len(rects) < 2 {
		t.Fatalf("2600 wide rect produced %d rects, expected a split", len(rects))
	}
	covered, x := 0, 0
	for _, rc := range rects {
		rx := int(binary.BigEndian.Uint16(rc[0:]))
		ry := int(binary.BigEndian.Uint16(rc[2:]))
		w, h, _, _ := rectPayload(t, rc)
		if w > vncTightMax {
			t.Fatalf("split still produced a %d wide rect", w)
		}
		if rx != x || ry != vncChromeH || h != 40 {
			t.Fatalf("strip at %d,%d %dx%d is not where it should be", rx, ry, w, h)
		}
		x += w
		covered += w
	}
	if covered != 2600 {
		t.Fatalf("strips cover %d columns, want 2600", covered)
	}
}

func TestDirtyRects(t *testing.T) {
	const gw, gh, w, h = 4, 3, 250, 190
	base := make([]uint64, gw*gh)
	for i := range base {
		base[i] = uint64(i + 1)
	}

	if got := vncDirtyRects(base, nil, gw, gh, w, h); len(got) != 1 || got[0] != image.Rect(0, 0, w, h) {
		t.Fatalf("a client with no history should get one full rect, got %v", got)
	}
	if got := vncDirtyRects(base, base, gw, gh, w, h); len(got) != 0 {
		t.Fatalf("an unchanged frame should produce no rects, got %v", got)
	}

	// two adjacent tiles on one row merge into a single run
	cur := append([]uint64(nil), base...)
	cur[1], cur[2] = 99, 98
	got := vncDirtyRects(cur, base, gw, gh, w, h)
	if len(got) != 1 || got[0] != image.Rect(vncTile, 0, 3*vncTile, vncTile) {
		t.Fatalf("adjacent dirty tiles did not merge: %v", got)
	}

	// the same run on two rows merges vertically
	cur = append([]uint64(nil), base...)
	cur[1], cur[gw+1] = 99, 98
	got = vncDirtyRects(cur, base, gw, gh, w, h)
	if len(got) != 1 || got[0] != image.Rect(vncTile, 0, 2*vncTile, 2*vncTile) {
		t.Fatalf("stacked dirty runs did not merge: %v", got)
	}

	// past the threshold it gives up and sends the whole frame
	cur = make([]uint64, gw*gh)
	got = vncDirtyRects(cur, base, gw, gh, w, h)
	if len(got) != 1 || got[0] != image.Rect(0, 0, w, h) {
		t.Fatalf("a mostly dirty frame should collapse to one rect, got %v", got)
	}
}

func TestCompactLen(t *testing.T) {
	for _, n := range []int{0, 1, 127, 128, 16383, 16384, 1 << 20} {
		var b bytes.Buffer
		vncCompact(n, &b)
		got, shift := 0, 0
		for i, c := range b.Bytes() {
			got |= int(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				if i != b.Len()-1 {
					t.Fatalf("%d: %d trailing bytes after the terminator", n, b.Len()-1-i)
				}
				break
			}
		}
		if got != n {
			t.Fatalf("compact length %d decoded as %d", n, got)
		}
	}
}

// The palette reserves its top two slots for the cursor and every index the
// lookup returns has to be inside it.
func TestPaletteBounds(t *testing.T) {
	img := testFrame(120, 80)
	for _, n := range []int{2, 4, 16, 216, 256} {
		p := vncBuildPal(img, n, 1)
		if len(p.pal) > n || len(p.pal) < 2 {
			t.Fatalf("size %d: built a palette of %d entries", n, len(p.pal))
		}
		if int(p.white) >= len(p.pal) || int(p.black) >= len(p.pal) {
			t.Fatalf("size %d: cursor slots %d/%d outside a %d entry palette", n, p.black, p.white, len(p.pal))
		}
		lut := vncNewLUT()
		for _, v := range [][3]int32{{0, 0, 0}, {255, 255, 255}, {17, 200, 90}, {255, 0, 128}} {
			if i := p.index(lut, v[0], v[1], v[2]); int(i) >= len(p.pal) {
				t.Fatalf("size %d: colour %v mapped to index %d", n, v, i)
			}
		}
	}
}

// The old colormap path ran median cut and then a linear palette search for
// every pixel of every frame. These two measure what replaced it.
func BenchmarkColormapOld(b *testing.B) {
	img := testFrame(1152, 600)
	for i := 0; i < b.N; i++ {
		gifPalette(img, 216) // median cut plus Palette.Index per pixel
	}
}

func BenchmarkColormapNew(b *testing.B) {
	img := testFrame(1152, 600)
	c := &vncClient{pf: vncBGR233, indexed: true, enc: encRaw}
	pal := vncBuildPal(img, 216, 1)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.pixels(img, pal, img.Bounds()) // dither plus memoized lookup
	}
}

func BenchmarkPaletteBuild(b *testing.B) {
	img := testFrame(1152, 600)
	for i := 0; i < b.N; i++ {
		vncBuildPal(img, 216, uint64(i))
	}
}
