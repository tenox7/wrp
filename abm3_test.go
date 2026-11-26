package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"math/rand"
	"testing"
)

// mqDec reads the coder's bits, as a client does
type mqDec struct {
	st   []byte
	a, c uint32 // the interval's size, and the code's place in it
	lo   uint32 // the bits of the last byte read that are not in c yet
	n    uint   // how many they are
	in   []byte
	pos  int
}

func (d *mqDec) next() uint32 {
	d.pos++
	if d.pos <= len(d.in) {
		return uint32(d.in[d.pos-1])
	}
	return 0
}

func (d *mqDec) bit(cx int) byte {
	s := d.st[cx]
	q := mqQe[s]
	b := s & 1
	d.a -= q
	if d.c < q {
		if d.a < q {
			d.st[cx] = mqMPS[s]
		} else {
			b ^= 1
			d.st[cx] = mqLPS[s]
		}
		d.a = q
	} else {
		d.c -= q
		if d.a&0x8000 != 0 {
			return b
		}
		if d.a < q {
			b ^= 1
			d.st[cx] = mqLPS[s]
		} else {
			d.st[cx] = mqMPS[s]
		}
	}
	for d.a < 0x8000 {
		if d.n == 0 {
			d.lo = d.next()
			d.n = 8
		}
		d.a <<= 1
		d.c = d.c<<1 | d.lo>>7
		d.lo = d.lo << 1 & 0xff
		d.n--
	}
	return b
}

// abm3Unpack undoes abm3Pack, as its file comment says: h rows of bpl
// bytes, w pixels each
func abm3Unpack(in []byte, bpl, w, h int) ([]byte, error) {
	bits := make([]byte, bpl*h)
	d := &mqDec{st: abm3Contexts(), a: 0x8000, in: in}
	d.c = d.next() << 8
	d.c |= d.next()
	zero := make([]byte, bpl)
	fa := bytes.Repeat([]byte{1}, bpl+2)
	fc := bytes.Repeat([]byte{1}, bpl+2)
	up, up2 := zero, zero
	prev := byte(1)
	for y := 0; y < h; y++ {
		cur := bits[y*bpl : (y+1)*bpl]
		prev = d.bit(abm3Row + int(prev))
		switch {
		case prev == 1:
			copy(cur, up)
			for i := 1; i <= bpl; i++ {
				fa[i] = 1
			}
		case d.bit(abm3White) == 1:
			for i := 1; i <= bpl; i++ {
				fa[i] = 0
			}
		default:
			p := 1
			for xb := 0; xb < bpl; xb++ {
				cx := p | int(fa[xb])<<2 | int(fa[xb+1])<<3 | int(fa[xb+2])<<4
				if up[xb] == 0 {
					cx |= 2
				}
				p = int(d.bit(abm3Byte + cx))
				fc[xb+1] = byte(p)
				if p == 1 {
					cur[xb] = up[xb]
				}
			}
			for xb := 0; xb < bpl; xb++ {
				if fc[xb+1] == 1 {
					continue
				}
				for x := xb * 8; x < xb*8+8 && x < w; x++ {
					if d.bit(abm3Pix+abm3PixCtx(cur, up, up2, w, x)) == 1 {
						cur[xb] |= 0x80 >> uint(x&7)
					}
				}
			}
			fa, fc = fc, fa
		}
		up2, up = up, cur
	}
	if d.pos > len(in) {
		return nil, fmt.Errorf("%d bytes read after the end", d.pos-len(in))
	}
	return bits, nil
}

// rows of bpl bytes, w pixels each, for the pack tests: name and bits
func abmTestRows(bpl, w int) map[string][]byte {
	rnd := rand.New(rand.NewSource(1))
	last := byte(0xff << uint(bpl*8-w)) // the pixels of a row's last byte
	trim := func(d []byte) []byte {
		for i := bpl - 1; i < len(d); i += bpl {
			d[i] &= last
		}
		return d
	}
	pages := map[string][]byte{
		"empty": {},
		"one":   trim(bytes.Repeat([]byte{0x55}, bpl)),
		"white": make([]byte, 40*bpl),
		"black": trim(bytes.Repeat([]byte{0xff}, 40*bpl)),
	}
	rows := make([]byte, 0, 30*bpl)
	for len(rows) < 30*bpl {
		rows = append(rows, []byte{0, 0, 0x7e, 0x42, 0x42, 0x7e, 0, 0, 0x18, 0x18, 0, 0, 0}...)
	}
	pages["rows"] = trim(rows[:30*bpl])
	noise := make([]byte, 50*bpl)
	rnd.Read(noise)
	pages["noise"] = trim(noise)
	// text-like: a few "letters" at random places on white, some lines twice
	text := make([]byte, 200*bpl)
	for k := 0; k < 300 && len(text) > 4; k++ {
		at := rnd.Intn(len(text) - 4)
		copy(text[at:], []byte{0x3c, 0x66, 0x7e, 0x60}[:1+rnd.Intn(4)])
	}
	copy(text[100*bpl:], text[20*bpl:60*bpl])
	pages["text"] = trim(text)
	// gray: a dither's checkerboard, with a white band
	gray := make([]byte, 60*bpl)
	for i := range gray {
		if y := i / bpl; y < 20 || y >= 30 {
			gray[i] = 0x55 << uint(y&1)
		}
	}
	pages["gray"] = trim(gray)
	return pages
}

func TestABM3Pack(t *testing.T) {
	for _, sz := range [][2]int{{13, 104}, {13, 99}, {1, 1}, {1, 8}, {2, 9}, {80, 632}, {79, 632 - 7}} {
		bpl, w := sz[0], sz[1]
		for name, d := range abmTestRows(bpl, w) {
			p := abm3Pack(d, bpl, w)
			got, err := abm3Unpack(p, bpl, w, len(d)/bpl)
			if err != nil {
				t.Errorf("%s, %d pixels wide: %v", name, w, err)
			} else if !bytes.Equal(got, d) {
				t.Errorf("%s, %d pixels wide: %d bytes packed to %d do not come back the same", name, w, len(d), len(p))
			}
			if len(p) > len(d)+len(d)/8+4 {
				t.Errorf("%s, %d pixels wide: %d bytes packed to %d", name, w, len(d), len(p))
			}
			// cut short, it must not read far past its end, and must not crash
			if len(p) > 4 {
				if _, err := abm3Unpack(p[:len(p)/2], bpl, w, len(d)/bpl); err == nil && name == "noise" {
					t.Errorf("%s, %d pixels wide: half of the file decodes with no error", name, w)
				}
			}
		}
	}
	if n := len(abm3Pack(make([]byte, 351*79), 79, 632)); n > 8 {
		t.Errorf("a white slice: %d bytes, want 8 or fewer", n)
	}
}

// A dither is where ABM3 gains most: ABM's byte LZ finds little in it
func TestABM3Smaller(t *testing.T) {
	rnd := rand.New(rand.NewSource(2))
	const bpl, w, h = 80, 640, 200
	d := make([]byte, bpl*h)
	for y := 0; y < h; y++ { // a gray that gets darker to the right, as noise
		for x := 0; x < w; x++ {
			if rnd.Intn(w) < x/2 {
				d[y*bpl+x>>3] |= 0x80 >> uint(x&7)
			}
		}
	}
	a, b := len(abmPack(d, bpl)), len(abm3Pack(d, bpl, w))
	if b >= a {
		t.Errorf("ABM3 %d bytes, ABM %d: want fewer", b, a)
	}
	got, err := abm3Unpack(abm3Pack(d, bpl, w), bpl, w, h)
	if err != nil || !bytes.Equal(got, d) {
		t.Errorf("the rows do not come back the same (%v)", err)
	}
}

func TestEncodeABM3(t *testing.T) {
	p := paletted(stripes()).(*image.Paletted)
	var b bytes.Buffer
	if err := encodeABM3(&b, p); err != nil {
		t.Fatal(err)
	}
	f := b.Bytes()
	wd, ht := p.Rect.Dx(), p.Rect.Dy()
	want := []byte{'A', 'B', 'M', '3', byte(wd >> 8), byte(wd), byte(ht >> 8), byte(ht), 1, 0}
	if len(f) < 10 || !bytes.Equal(f[:10], want) {
		t.Fatalf("header % x, want % x", f, want)
	}
	bits, bpl := abmBits(p)
	got, err := abm3Unpack(f[10:], bpl, wd, ht)
	if err != nil || !bytes.Equal(got, bits) {
		t.Errorf("the rows do not come back the same (%v)", err)
	}
	c := image.NewPaletted(image.Rect(0, 0, 3, 3), color.Palette{color.Black, color.White, color.RGBA{255, 0, 0, 255}})
	if encodeABM3(&b, c) == nil {
		t.Error("a picture with 3 colors: no error")
	}
}
