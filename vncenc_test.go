package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"math/rand"
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

// A tall page: text like rows of dark blobs, picture blocks and whitespace on a
// light background. Scroll detection is tested against viewports cut out of
// this and round tripped through jpeg, because that is the whole difficulty:
// flat synthetic patterns survive jpeg in a way real page content does not.
func testPage(w, h, seed int) *image.RGBA {
	rnd := rand.New(rand.NewSource(int64(seed)))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{0xf8, 0xf8, 0xf6, 0xff}), image.Point{}, draw.Src)
	set := func(x, y int, r, g, b byte) {
		if x < 0 || y < 0 || x >= w || y >= h {
			return
		}
		o := img.PixOffset(x, y)
		img.Pix[o], img.Pix[o+1], img.Pix[o+2] = r, g, b
	}
	for y := 0; y < h; {
		switch n := rnd.Intn(10); {
		case n < 6: // a line of text
			for x := 20; x < w-20; {
				gw := 3 + rnd.Intn(6)
				for gy := 0; gy < 9; gy++ {
					for gx := 0; gx < gw; gx++ {
						if rnd.Intn(3) == 0 {
							continue
						}
						set(x+gx, y+gy, 0x20, 0x20, 0x28)
					}
				}
				x += gw + 2 + rnd.Intn(3)
			}
			y += 14
		case n < 8: // a picture
			ph := 60 + rnd.Intn(80)
			for py := 0; py < ph; py++ {
				for px := 40; px < w-40; px++ {
					set(px, y+py, byte(px*255/w), byte(py*3+40), byte(200-py))
				}
			}
			y += ph + 10
		default:
			y += 8 + rnd.Intn(20)
		}
	}
	return img
}

// One screencast frame: the viewport at this scroll offset, through jpeg
func testViewport(t *testing.T, page *image.RGBA, top, w, h, qual int) *image.RGBA {
	t.Helper()
	var buf bytes.Buffer
	sub := page.SubImage(image.Rect(0, top, w, top+h))
	if err := jpeg.Encode(&buf, sub, &jpeg.Options{Quality: qual}); err != nil {
		t.Fatalf("encode viewport: %v", err)
	}
	img := vncDecodeRGBA(buf.Bytes())
	if img == nil {
		t.Fatal("viewport did not decode")
	}
	return img
}

func testSamp(t *testing.T, img *image.RGBA) *vncSamp {
	t.Helper()
	_, s, _, _ := vncHashFrame(img)
	return s
}

// A scroll is the one case where the client already holds the pixels, so the
// detector has to find the shift through jpeg noise, and the leftover rows have
// to be the ones a CopyRect could not supply.
func TestScrollDetect(t *testing.T) {
	const w, h, qual, top = 640, 600, 70, 500
	page := testPage(w, 2400, 7)
	at := func(y int) *vncSamp { return testSamp(t, testViewport(t, page, y, w, h, qual)) }
	base := at(top)

	for _, dy := range []int{16, -16, 53, -53, 137, -137, 300} {
		cur := at(top + dy)
		if got := vncDetectShift(cur, base); got != dy {
			t.Fatalf("scroll of %d detected as %d", dy, got)
		}
		// the copy has to account for everything but the newly exposed band,
		// give or take the jpeg block rows either side of the seam
		rows := 0
		for _, r := range vncShiftDirty(cur, base, dy, w, h) {
			rows += r.Dy()
		}
		if rows < abs(dy) {
			t.Fatalf("scroll of %d left %d dirty rows, the exposed band alone is %d", dy, rows, abs(dy))
		}
		if rows > abs(dy)+48 {
			t.Fatalf("scroll of %d left %d dirty rows, expected about %d", dy, rows, abs(dy))
		}
	}

	// the same content captured twice is not a scroll, however the jpeg landed
	if got := vncDetectShift(at(top), base); got != 0 {
		t.Fatalf("an unchanged frame reported a shift of %d", got)
	}
	if got := vncDetectShift(at(top+2), base); got != 0 {
		t.Fatalf("a 2 row shift is not worth a CopyRect, got %d", got)
	}
	// A jump to an unrelated part of the page has too little in common to be
	// worth a copy, whatever offset happens to line up best on the way past.
	// Correctness does not rest on this - the rows an offset cannot account for
	// are repainted either way - but bandwidth does.
	far := at(top + h - 8)
	if dy := vncDetectShift(far, base); dy != 0 {
		if abs(dy) > h-vncMinShiftRows {
			t.Fatalf("shift %d leaves less than %d rows to copy", dy, vncMinShiftRows)
		}
		rows := 0
		for _, r := range vncShiftDirty(far, base, dy, w, h) {
			rows += r.Dy()
		}
		if rows*100 < h*vncCopyGain {
			t.Fatalf("a jump to unrelated content claimed a shift of %d saving %d%%", dy, 100-rows*100/h)
		}
	}
	// unrelated content must not be explained away as a scroll
	other := testSamp(t, testViewport(t, testPage(w, 2400, 99), top, w, h, qual))
	if got := vncDetectShift(other, base); got != 0 {
		t.Fatalf("a different page reported a shift of %d", got)
	}
	// a blank page has nothing to match on and would vote for any shift at all
	blank := testSamp(t, testViewport(t, testPage(w, 2400, 7), 0, w, h, qual))
	for i := range blank.act {
		blank.act[i] = 0
	}
	if got := vncDetectShift(blank, base); got != 0 {
		t.Fatalf("featureless rows produced a phantom shift of %d", got)
	}

	// content that changed on top of the scroll still has to be repainted
	const dy = 80
	moved := testViewport(t, page, top+dy, w, h, qual)
	for y := 300; y < 312; y++ {
		for x := 0; x < w; x++ {
			o := moved.PixOffset(x, y)
			moved.Pix[o], moved.Pix[o+1], moved.Pix[o+2] = 0xff, 0x00, 0x00
		}
	}
	found := false
	for _, r := range vncShiftDirty(testSamp(t, moved), base, dy, w, h) {
		if r.Min.Y <= 300 && r.Max.Y >= 312 {
			found = true
		}
	}
	if !found {
		t.Fatal("a band that changed on top of the scroll was not marked dirty")
	}
}

// The sample plane has to be comparable across frames, and a change has to show
// up in it - it is what decides whether a copy left the client correct.
func TestFrameSamples(t *testing.T) {
	a := testFrame(300, 200)
	_, sa, _, _ := vncHashFrame(a)
	_, sb, _, _ := vncHashFrame(testFrame(300, 200))
	if sa.h != 200 || sa.stride != (300+vncSampX-1)/vncSampX*3 {
		t.Fatalf("sampled a 300x200 frame as stride %d, %d rows", sa.stride, sa.h)
	}
	if !bytes.Equal(sa.pix, sb.pix) {
		t.Fatal("identical frames sampled differently")
	}
	for y := 0; y < sa.h; y++ {
		if !sa.rowMatch(sb, y, y) {
			t.Fatalf("identical frames did not match at row %d", y)
		}
	}
	// flat rows must not read as matchable content, busy ones must
	if sa.act[5] != 0 {
		t.Fatalf("a flat row has activity %d", sa.act[5])
	}
	if sa.act[190] < vncSampAct {
		t.Fatalf("a busy row has activity %d, below the %d probe threshold", sa.act[190], vncSampAct)
	}

	for x := 0; x < 8; x++ { // a change one sample wide, at the hard threshold
		a.Pix[a.PixOffset(x, 57)] = byte(int(a.Pix[a.PixOffset(x, 57)]) ^ 0xff)
	}
	_, sc, _, _ := vncHashFrame(a)
	if sc.rowMatch(sa, 57, 57) {
		t.Fatal("a changed row still counted as a match")
	}
	for y := 0; y < sa.h; y++ {
		if y != 57 && !sc.rowMatch(sa, y, y) {
			t.Fatalf("changing row 57 also disturbed row %d", y)
		}
	}
}

// What a CopyRect does to the client's framebuffer: row y becomes row y+dy
func testShiftFB(img *image.RGBA, dy int) {
	s := img.Stride
	h := img.Bounds().Dy()
	if dy > 0 {
		copy(img.Pix[0:], img.Pix[dy*s:h*s])
		return
	}
	copy(img.Pix[-dy*s:], img.Pix[0:(h+dy)*s])
}

// Mean and worst per channel difference between two frames, and how many
// samples are off by more than tol
func testDiff(a, b *image.RGBA, tol int) (mean float64, worst, over int) {
	sum, n := 0, 0
	for i := 0; i < len(a.Pix); i += 4 {
		for k := 0; k < 3; k++ {
			d := abs(int(a.Pix[i+k]) - int(b.Pix[i+k]))
			sum, n = sum+d, n+1
			if d > worst {
				worst = d
			}
			if d > tol {
				over++
			}
		}
	}
	return float64(sum) / float64(n), worst, over
}

// The one that matters: drive a page past the viewport in uneven steps,
// applying exactly what the server would send - a CopyRect, then the rows it
// could not account for - and check the client's framebuffer never drifts away
// from the frame it should be showing. Copies are matched with a tolerance, so
// this is where accumulated staleness would surface.
func TestScrollReconstruct(t *testing.T) {
	const w, h, qual, steps = 640, 600, 70, 40
	page := testPage(w, 4000, 7)
	fb := testViewport(t, page, 0, w, h, qual) // the client starts with a full frame
	prev := testSamp(t, fb)

	top, copies, painted := 0, 0, 0
	for step := 0; step < steps; step++ {
		top += 13 + step*7%61 // uneven, so the jpeg block phase keeps changing
		cur := testViewport(t, page, top, w, h, qual)
		cs := testSamp(t, cur)
		dirty := []image.Rectangle{image.Rect(0, 0, w, h)}
		if dy := vncDetectShift(cs, prev); dy != 0 {
			copies++
			testShiftFB(fb, dy)
			dirty = vncShiftDirty(cs, prev, dy, w, h)
		}
		for _, r := range dirty {
			painted += r.Dy()
			draw.Draw(fb, r, cur, r.Min, draw.Src)
		}
		// the server now believes the client holds this frame exactly
		prev = prev.copyOf(cs)

		// jpeg noise between two encodes of the same content is a mean of about
		// 3 with a handful of samples ringing past 64 at text edges. Anything
		// beyond that is drift, not noise.
		mean, worst, over := testDiff(fb, cur, 64)
		if mean > 8 || over*10000 > w*h*3 {
			t.Fatalf("step %d (top %d): framebuffer drifted, mean %.2f worst %d, %d samples over 64",
				step, top, mean, worst, over)
		}
	}
	if copies < steps*3/4 {
		t.Fatalf("only %d of %d scroll steps produced a CopyRect", copies, steps)
	}
	if painted > steps*h/4 {
		t.Fatalf("repainted %d rows over %d steps, CopyRect saved little", painted, steps)
	}
	mean, worst, over := testDiff(fb, testViewport(t, page, top, w, h, qual), 64)
	t.Logf("%d/%d steps copied, %d rows repainted of %d, final mean err %.2f worst %d over64 %d",
		copies, steps, painted, steps*h, mean, worst, over)
}

func BenchmarkHashFrame(b *testing.B) {
	img := testFrame(1152, 600)
	for b.Loop() {
		vncHashFrame(img)
	}
}

func BenchmarkDetectShift(b *testing.B) {
	page := testPage(1152, 2400, 7)
	view := func(top int) *vncSamp {
		var buf bytes.Buffer
		jpeg.Encode(&buf, page.SubImage(image.Rect(0, top, 1152, top+600)), &jpeg.Options{Quality: 70})
		_, s, _, _ := vncHashFrame(vncDecodeRGBA(buf.Bytes()))
		return s
	}
	cur, prev := view(553), view(500)
	b.ResetTimer()
	for b.Loop() {
		if vncDetectShift(cur, prev) != 53 {
			b.Fatal("benchmark is not measuring a detected shift")
		}
	}
}

func TestContextMenu(t *testing.T) {
	vncSrv.menu = vncMenu{}
	t.Cleanup(func() { vncSrv.menu = vncMenu{} })

	m := vncMenu{open: true, x: 40, y: 30,
		items: []string{"Back", "Forward", "Reload", "Open in new tab", "Copy link"},
		link:  "http://example.com/"}
	w, h := m.size()
	if w < len("Open in new tab")*7 || h != 5*vncMenuRow+2 {
		t.Fatalf("menu sized %dx%d, too small for its items", w, h)
	}

	// drawing must stay inside the frame even when opened near an edge
	for _, at := range []image.Point{{40, 30}, {310, 190}, {0, 0}} {
		img := testFrame(320, 200)
		before := append([]byte(nil), img.Pix...)
		mm := m
		mm.x, mm.y = at.X, at.Y
		vncDrawMenu(img, mm) // must not panic or write out of bounds
		if bytes.Equal(before, img.Pix) {
			t.Fatalf("menu at %v drew nothing", at)
		}
	}

	// hit testing: a click on the third row runs Reload, outside dismisses
	vncSrv.menu = m
	if !vncMenuClick(m.x+5, m.y+1+2*vncMenuRow+2) {
		t.Fatal("click inside the menu was not consumed")
	}
	if vncSrv.menu.open {
		t.Fatal("menu stayed open after an item was picked")
	}

	vncSrv.menu = m
	if !vncMenuClick(m.x-10, m.y-10) {
		t.Fatal("click outside the menu should still be consumed to dismiss it")
	}
	if vncSrv.menu.open {
		t.Fatal("menu stayed open after a click outside")
	}

	if vncMenuClick(5, 5) { // closed menu must not swallow clicks
		t.Fatal("a closed menu consumed a click")
	}
}
