// WRP ABM pictures: black and white, 1 bit a pixel, for clients that ask
// for them (Accept: image/x-abm, see picfmt.go), such as Archaic on a black
// and white display. A 10 MHz 68010 needs 7.5 seconds to decode the GIF of one
// screen: GIF has a code for every few pixels. ABM is the display's own
// bytes, packed with a byte LZ: the same computer decodes it in 0.15
// seconds, and it is about 11% smaller than the GIF.
//
// The file:
//
//	"ABM"           3 bytes
//	how packed      1 byte: '1'
//	width, height   2 bytes each, the high byte first
//	depth           1 byte: bits a pixel, 1
//	flags           1 byte: 0
//	the rows        packed. Each row is whole bytes; the high bit is the
//	                left pixel; 1 is black
//
// The packed rows are tokens. A control byte, then:
//
//	00nnnnnn        n+1 bytes follow as they are
//	01nnnnnn        n+1 white bytes (0)
//	10nnnnnn        n+1 bytes as in the row above
//	110nnnnn d      n+2 bytes as d+1 bytes back
//	111nnnnn dh dl  n+3 bytes as 256*dh+dl+1 bytes back
package main

import (
	"errors"
	"image"
	"io"
)

const abmType = "image/x-abm"

// The picture as ABM's rows: 1 bit a pixel, 1 = black, or nil if it has
// more than black and white
func abmBits(p *image.Paletted) (bits []byte, bpl int) {
	if len(p.Palette) > 2 {
		return nil, 0
	}
	var black [2]bool
	for i, c := range p.Palette {
		r, g, b, _ := c.RGBA()
		switch {
		case r == 0 && g == 0 && b == 0:
			black[i] = true
		case r == 0xffff && g == 0xffff && b == 0xffff:
		default:
			return nil, 0
		}
	}
	w, h := p.Rect.Dx(), p.Rect.Dy()
	bpl = (w + 7) / 8
	bits = make([]byte, bpl*h)
	for y := 0; y < h; y++ {
		s := p.Pix[y*p.Stride : y*p.Stride+w]
		d := bits[y*bpl : (y+1)*bpl]
		for x, v := range s {
			if v < 2 && black[v] {
				d[x>>3] |= 0x80 >> (x & 7)
			}
		}
	}
	return bits, bpl
}

const (
	abmLit  = iota // bytes as they are
	abmZero        // white bytes
	abmUp          // as the row above
	abmNear        // as 1..256 bytes back
	abmFar         // as 1..65536 bytes back

	abmTries = 64 // older places looked at for a match
)

// abmPack packs rows of bpl bytes as tokens: of all the ways to cut d into
// tokens, the one with the fewest bytes
func abmPack(d []byte, bpl int) []byte {
	n := len(d)
	// zero[i], up[i]: how many bytes from i on are white, are as in the
	// row above (64 at most: a token's length)
	zero := make([]uint8, n+1)
	up := make([]uint8, n+1)
	for i := n - 1; i >= 0; i-- {
		if d[i] == 0 && zero[i+1] < 64 {
			zero[i] = zero[i+1] + 1
		} else if d[i] == 0 {
			zero[i] = 64
		}
		if i >= bpl && d[i] == d[i-bpl] {
			if up[i+1] < 64 {
				up[i] = up[i+1] + 1
			} else {
				up[i] = 64
			}
		}
	}
	// cost[i]: the fewest bytes for d[:i]. kind, size, dist: its last token
	const inf = 1 << 30
	cost := make([]int32, n+1)
	kind := make([]uint8, n+1)
	size := make([]uint8, n+1)
	dist := make([]uint16, n+1)
	for i := 1; i <= n; i++ {
		cost[i] = inf
	}
	head := make([]int32, 1<<16) // the last place of each pair of bytes, +1
	prev := make([]int32, n)     // the place before with the same pair, +1
	for i := 0; i < n; i++ {
		c := cost[i]
		for k := 1; k <= 64 && i+k <= n; k++ {
			if c+1+int32(k) < cost[i+k] {
				cost[i+k], kind[i+k], size[i+k] = c+1+int32(k), abmLit, uint8(k)
			}
		}
		for k := 1; k <= int(zero[i]); k++ {
			if c+1 < cost[i+k] {
				cost[i+k], kind[i+k], size[i+k] = c+1, abmZero, uint8(k)
			}
		}
		for k := 1; k <= int(up[i]); k++ {
			if c+1 < cost[i+k] {
				cost[i+k], kind[i+k], size[i+k] = c+1, abmUp, uint8(k)
			}
		}
		if i+1 >= n {
			continue
		}
		// the longest match 1..256 back (2 bytes), and the longest one
		// at all (3 bytes): each of their starts is a match too
		key := int(d[i]) | int(d[i+1])<<8
		var nl, nd, fl, fd int
		for p, t := int(head[key])-1, 0; p >= 0 && t < abmTries && i-p <= 65536; p, t = int(prev[p])-1, t+1 {
			m := 2
			for i+m < n && m < 34 && d[p+m] == d[i+m] {
				m++
			}
			if m > fl {
				fl, fd = m, i-p
			}
			if i-p <= 256 && m > nl {
				nl, nd = m, i-p
			}
			if nl >= 33 {
				break
			}
		}
		if nl > 33 {
			nl = 33
		}
		for k := 2; k <= nl; k++ {
			if c+2 < cost[i+k] {
				cost[i+k], kind[i+k], size[i+k], dist[i+k] = c+2, abmNear, uint8(k), uint16(nd-1)
			}
		}
		for k := 3; k <= fl; k++ {
			if c+3 < cost[i+k] {
				cost[i+k], kind[i+k], size[i+k], dist[i+k] = c+3, abmFar, uint8(k), uint16(fd-1)
			}
		}
		prev[i] = head[key]
		head[key] = int32(i + 1)
	}
	// the tokens, from the last one back, then written from the first
	type tok struct {
		at   int
		kind uint8
		size uint8
		dist uint16
	}
	var toks []tok
	for i := n; i > 0; i -= int(size[i]) {
		toks = append(toks, tok{i - int(size[i]), kind[i], size[i], dist[i]})
	}
	o := make([]byte, 0, int(cost[n]))
	for j := len(toks) - 1; j >= 0; j-- {
		t := toks[j]
		switch t.kind {
		case abmLit:
			o = append(o, t.size-1)
			o = append(o, d[t.at:t.at+int(t.size)]...)
		case abmZero:
			o = append(o, 0x40|(t.size-1))
		case abmUp:
			o = append(o, 0x80|(t.size-1))
		case abmNear:
			o = append(o, 0xc0|(t.size-2), byte(t.dist))
		case abmFar:
			o = append(o, 0xe0|(t.size-3), byte(t.dist>>8), byte(t.dist))
		}
	}
	return o
}

// writeABM writes an ABM file: the header, then the packed rows. pack is
// how they are packed: '1' (abmPack) or '3' (abm3Pack, see abm3.go).
func writeABM(w io.Writer, pack byte, wd, ht int, rows []byte) error {
	if wd <= 0 || ht <= 0 || wd > 0xffff || ht > 0xffff {
		return errors.New("abm: a picture of this size does not fit")
	}
	h := []byte{'A', 'B', 'M', pack, byte(wd >> 8), byte(wd), byte(ht >> 8), byte(ht), 1, 0}
	if _, err := w.Write(h); err != nil {
		return err
	}
	_, err := w.Write(rows)
	return err
}

// encodeABM writes a black and white picture as ABM
func encodeABM(w io.Writer, p *image.Paletted) error {
	bits, bpl := abmBits(p)
	if bits == nil {
		return errors.New("abm: not a black and white picture")
	}
	return writeABM(w, '1', p.Rect.Dx(), p.Rect.Dy(), abmPack(bits, bpl))
}
