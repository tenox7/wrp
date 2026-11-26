// WRP AB8 pictures: a picture in up to 256 colors for a client with a slow
// CPU (Accept: image/x-ab8), such as Archaic on a 68030 Mac. It is ABM
// (see abm.go) with a byte for each pixel: the display's own bytes, packed
// with a byte LZ. A 16 MHz 68030 decodes one screen in 256 colors in 0.4
// seconds (the GIF: 1.3 seconds). It is most often smaller than the GIF;
// with a dither to the 216 web colors it is about 20% larger.
//
// The file:
//
//	"AB8"           3 bytes
//	how packed      1 byte: '1'
//	width, height   2 bytes each, the high byte first
//	colors          1 byte: how many, less 1
//	the colors      3 bytes each: red, green, blue
//	the rows        packed. A byte for each pixel: its color's number
//
// The packed rows are tokens. A control byte, then:
//
//	00nnnnnn        n+1 bytes follow as they are
//	01nnnnnn        n+1 times the byte before. n = 63: 2 bytes c follow
//	                (the high one first), 64+c times
//	10nnnnnn        n+1 bytes as in the row above. n = 63 as for 01
//	110nnnnn d      n+2 bytes as d+1 bytes back
//	111nnnnn dh dl  n+3 bytes as 256*dh+dl+1 bytes back. n = 31: a byte b
//	                follows, 34+b bytes
package main

import (
	"errors"
	"image"
	"io"
	"sync"
)

const ab8Type = "image/x-ab8"

const (
	ab8Lit  = iota // bytes as they are
	ab8Run         // the byte before, again
	ab8Up          // as the row above
	ab8Near        // as 1..256 bytes back
	ab8Far         // as 1..65536 bytes back

	ab8Tries = 16 // older places looked at for a match
)

type ab8Tok struct {
	kind uint8
	n    int32 // the bytes it gives
	dist int32
}

// ab8Enc has the encoder's working memory: a slice needs about 11 MB
type ab8Enc struct {
	run, up, nl, nd, fl, fd, prev, best, lit []int32
	ch                                       []ab8Tok
	head                                     [1 << 15]int32
}

var ab8Encs = sync.Pool{New: func() any { return new(ab8Enc) }}

func ab8Zeros(b []int32, n int) []int32 {
	if cap(b) < n {
		return make([]int32, n)
	}
	b = b[:n]
	clear(b)
	return b
}

// tokens cuts pix (rows of w bytes) into tokens. The matches come from
// hash chains on 3 bytes (ab8Tries places at most, none inside a long run
// or a long copy of the row above). Of the ways to cut pix with what was
// found, it takes the one with the fewest bytes.
func (e *ab8Enc) tokens(pix []byte, w int) []ab8Tok {
	n := len(pix)
	e.run, e.up = ab8Zeros(e.run, n+1), ab8Zeros(e.up, n+1)
	e.nl, e.nd, e.fl, e.fd = ab8Zeros(e.nl, n), ab8Zeros(e.nd, n), ab8Zeros(e.fl, n), ab8Zeros(e.fd, n)
	e.prev, e.best, e.lit = ab8Zeros(e.prev, n), ab8Zeros(e.best, n+1), ab8Zeros(e.lit, n+1)
	if cap(e.ch) < n {
		e.ch = make([]ab8Tok, n)
	}
	run, up, nl, nd, fl, fd, prev, best, lit, ch := e.run, e.up, e.nl, e.nd, e.fl, e.fd, e.prev, e.best, e.lit, e.ch[:n]
	// run[i], up[i]: how many bytes from i on are as the byte before them,
	// as the byte above them
	for i := n - 1; i >= 1; i-- {
		if pix[i] == pix[i-1] {
			run[i] = run[i+1] + 1
		}
		if i >= w && pix[i] == pix[i-w] {
			up[i] = up[i+1] + 1
		}
	}
	// nl, nd: the longest match 1..256 back (33 bytes at most) and how far
	// back; fl, fd: the longest one at all (289 at most)
	head := &e.head
	for i := range head {
		head[i] = -1
	}
	for i := 0; i+2 < n; i++ {
		h := ((uint32(pix[i]) | uint32(pix[i+1])<<8 | uint32(pix[i+2])<<16) * 2654435761) >> (32 - 15)
		if run[i] < 64 && up[i] < 64 {
			var bn, bnd, bf, bfd int32
			maxl := min(289, n-i)
			t := ab8Tries
			for j := head[h]; j >= 0 && i-int(j) <= 65536 && t > 0; j, t = prev[j], t-1 {
				if bf > 0 && pix[int(j)+int(bf)] != pix[i+int(bf)] && (i-int(j) > 256 || bn >= 33) {
					continue
				}
				l := 0
				for l < maxl && pix[int(j)+l] == pix[i+l] {
					l++
				}
				if l < 3 {
					continue
				}
				d := int32(i - int(j))
				if d <= 256 && int32(min(l, 33)) > bn {
					bn, bnd = int32(min(l, 33)), d
				}
				if int32(l) > bf {
					bf, bfd = int32(l), d
					if l == maxl {
						break
					}
				}
			}
			nl[i], nd[i], fl[i], fd[i] = bn, bnd, bf, bfd
		}
		prev[i] = head[h]
		head[h] = int32(i)
	}
	// best[i]: the fewest bytes for pix[i:]. lit[i]: the same when i is a
	// byte in a row of literals that began before it (it needs no control
	// byte; a row longer than 64 gets one more when it is packed).
	for i := n - 1; i >= 0; i-- {
		lit[i] = 1 + min(lit[i+1], best[i+1])
		c := 1 + lit[i]
		tk := ab8Tok{ab8Lit, 1, 0}
		try := func(cost int32, kind uint8, l, d int32) {
			if v := cost + best[i+int(l)]; v < c {
				c, tk = v, ab8Tok{kind, l, d}
			}
		}
		if m := run[i]; m > 0 {
			try(1, ab8Run, min(m, 63), 0)
			if m >= 64 {
				try(3, ab8Run, min(m, 65599), 0)
			}
		}
		if m := up[i]; m > 0 {
			try(1, ab8Up, min(m, 63), 0)
			if m >= 64 {
				try(3, ab8Up, min(m, 65599), 0)
			}
		}
		if m := nl[i]; m >= 2 {
			try(2, ab8Near, m, nd[i])
		}
		if m := fl[i]; m >= 3 {
			try(3, ab8Far, min(m, 33), fd[i])
			if m >= 34 {
				try(4, ab8Far, min(m, 289), fd[i])
			}
		}
		best[i] = c
		ch[i] = tk
	}
	// from the start: literals go on while the next byte is cheaper as one
	out := make([]ab8Tok, 0, n/6)
	for i := 0; i < n; {
		tk := ch[i]
		if tk.kind != ab8Lit {
			out = append(out, tk)
			i += int(tk.n)
			continue
		}
		j := i + 1
		for j < n && j-i < 64 && lit[j] <= best[j] {
			j++
		}
		out = append(out, ab8Tok{ab8Lit, int32(j - i), 0})
		i = j
	}
	return out
}

// ab8Pack packs rows of w bytes as tokens
func ab8Pack(pix []byte, w int) []byte {
	e := ab8Encs.Get().(*ab8Enc)
	defer ab8Encs.Put(e)
	toks := e.tokens(pix, w)
	o := make([]byte, 0, len(pix)/6)
	i := 0
	for _, t := range toks {
		n := int(t.n)
		switch t.kind {
		case ab8Lit:
			o = append(o, byte(n-1))
			o = append(o, pix[i:i+n]...)
		case ab8Run, ab8Up:
			b := byte(0x40)
			if t.kind == ab8Up {
				b = 0x80
			}
			if n <= 63 {
				o = append(o, b|byte(n-1))
			} else {
				o = append(o, b|63, byte((n-64)>>8), byte(n-64))
			}
		case ab8Near:
			o = append(o, 0xc0|byte(n-2), byte(t.dist-1))
		case ab8Far:
			d := int(t.dist) - 1
			if n <= 33 {
				o = append(o, 0xe0|byte(n-3), byte(d>>8), byte(d))
			} else {
				o = append(o, 0xe0|31, byte(d>>8), byte(d), byte(n-34))
			}
		}
		i += n
	}
	return o
}

// encodeAB8 writes a picture of up to 256 colors as AB8
func encodeAB8(w io.Writer, p *image.Paletted) error {
	wd, ht, nc := p.Rect.Dx(), p.Rect.Dy(), len(p.Palette)
	if wd <= 0 || ht <= 0 || wd > 0xffff || ht > 0xffff || nc < 1 || nc > 256 {
		return errors.New("ab8: not a picture of 1 to 256 colors")
	}
	h := []byte{'A', 'B', '8', '1', byte(wd >> 8), byte(wd), byte(ht >> 8), byte(ht), byte(nc - 1)}
	for _, c := range p.Palette {
		r, g, b, _ := c.RGBA()
		h = append(h, byte(r>>8), byte(g>>8), byte(b>>8))
	}
	var pix []byte
	if p.Stride == wd && len(p.Pix) >= wd*ht {
		pix = p.Pix[:wd*ht]
	} else {
		pix = make([]byte, 0, wd*ht)
		for y := 0; y < ht; y++ {
			pix = append(pix, p.Pix[y*p.Stride:y*p.Stride+wd]...)
		}
	}
	if _, err := w.Write(h); err != nil {
		return err
	}
	_, err := w.Write(ab8Pack(pix, wd))
	return err
}
