// Black and white (2 color) pictures, for clients with a 1-bit display.
// The D menu of the form (d=), or the -d flag, says what is dithered:
//
//	all   Floyd-Steinberg on all of the page, as WRP did before. Good
//	      for photos. But text in a color, gray text, and all that is on
//	      a color come out as dots, and are hard to read.
//	pics  the default, for reading. Only photos are dithered. Text,
//	      lines and flat colors are sharp: black or white, with no dots.
//	      These pictures are also smaller.
//	none  no dither at all: photos are sharp too. The smallest pictures.
//
// How pics and none make a pixel black or white:
//
//   - An edge (text, a line, the side of a box): the pixel is black when
//     it is darker than the middle between the darkest and the lightest of
//     its 3x3 neighbors. So light gray text and thin gray lines stay, as
//     black, and white text on a color stays white.
//   - A flat place (the background of a page or a box): black when darker
//     than the middle gray. No dots.
//   - A photo (only for pics): Floyd-Steinberg. A place is a photo when
//     nearly all its pixels differ from their neighbors (noise, soft
//     changes) but few of them differ much. A page's own colors are flat,
//     and text differs much.
package main

import (
	"image"
	"image/color"
	"runtime"
	"strconv"
	"sync"

	"github.com/MaxHalford/halfgone"
)

const (
	bwAll  = "all"
	bwPics = "pics"
	bwNone = "none"
)

// bwMode gives a valid dither choice: s, or else def
func bwMode(s, def string) string {
	switch s {
	case bwAll, bwPics, bwNone:
		return s
	}
	return def
}

// colorLetter: the letters of the C menu (c=) for the dither choices
var colorLetter = map[byte]string{'d': bwAll, 'n': bwNone, 'p': bwPics}

// colorChoice reads the C menu (c=): a number of colors, and a letter for
// the dither: d dithers all of the picture, n nothing, and p only its photos
// (2 colors only, see above). With no letter (an old client), each number has
// its own: 216 n (the web colors, sharp), 2 the d= field or else -d, and the
// others d. A c= that is not a number from 2 to 256 is -g's.
func colorChoice(c, d string) (int64, string) {
	m := ""
	if k := len(c) - 1; k > 0 && colorLetter[c[k]] != "" {
		c, m = c[:k], colorLetter[c[k]]
	}
	n, err := strconv.ParseInt(c, 10, 64)
	if err != nil || n < 2 || n > 256 {
		n, m = defGeom.c, ""
	}
	switch {
	case n == 2 && m == "":
		m = bwMode(d, *defDither)
	case n == 216 && m != bwAll:
		m = bwNone
	case n != 2 && n != 216 && m != bwNone:
		m = bwAll
	}
	return n, m
}

// colorVal is the C menu's value for n colors and dither m
func colorVal(n int64, m string) string {
	v := strconv.FormatInt(n, 10)
	for l, s := range colorLetter {
		if s == m {
			return v + string(l)
		}
	}
	return v
}

const (
	bwEdge    = 40  // 3x3 contrast from which a pixel is an edge
	bwPEdge   = 96  // the same in a photo: only strong edges are sharp there
	bwFlat    = 128 // a flat place is black below this
	bwCell    = 8   // a photo is made of cells of this many pixels
	bwSoft    = 70  // percent of pixels that are not flat, around a photo cell
	bwHard    = 128 // 3x3 contrast of a hard edge, as text has
	bwHardMax = 35  // percent of hard edges around a cell: more is text
	bwLo      = 16  // a photo's black point
	bwHi      = 240 // and its white point
)

// toBW makes the picture black and white: a gray picture with only 0 and 255
func toBW(i image.Image, how string) *image.Gray {
	if how != bwPics && how != bwNone {
		return halfgone.FloydSteinbergDitherer{}.Apply(halfgone.ImageToGray(i))
	}
	g := toGray(i)
	var photo []bool
	if how == bwPics {
		photo = bwPhotoCells(g)
	}
	return bwSharp(g, photo)
}

// toGray is halfgone.ImageToGray (color.GrayModel), but fast for the
// pictures a screenshot gives
func toGray(i image.Image) *image.Gray {
	b := i.Bounds()
	w, h := b.Dx(), b.Dy()
	g := image.NewGray(image.Rect(0, 0, w, h))
	var pix []uint8
	var stride int
	switch s := i.(type) {
	case *image.Gray:
		for y := 0; y < h; y++ {
			copy(g.Pix[y*g.Stride:y*g.Stride+w], s.Pix[(y+b.Min.Y-s.Rect.Min.Y)*s.Stride+b.Min.X-s.Rect.Min.X:])
		}
		return g
	case *image.RGBA:
		pix, stride = s.Pix[s.PixOffset(b.Min.X, b.Min.Y):], s.Stride
	case *image.NRGBA:
		pix, stride = s.Pix[s.PixOffset(b.Min.X, b.Min.Y):], s.Stride
		for y := 0; y < h && pix != nil; y++ {
			for x := 0; x < w; x++ {
				if pix[y*stride+4*x+3] != 255 {
					pix = nil // not opaque: the slow way
					break
				}
			}
		}
	}
	if pix == nil {
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				g.Pix[y*g.Stride+x] = color.GrayModel.Convert(i.At(b.Min.X+x, b.Min.Y+y)).(color.Gray).Y
			}
		}
		return g
	}
	bwRows(h, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			s := pix[y*stride:]
			d := g.Pix[y*g.Stride : y*g.Stride+w]
			for x := range d {
				p := s[4*x : 4*x+3 : 4*x+3]
				d[x] = uint8((19595*uint32(p[0])*0x101 + 38470*uint32(p[1])*0x101 + 7471*uint32(p[2])*0x101 + 1<<15) >> 24)
			}
		}
	})
	return g
}

// bwRows runs f on parts of the rows 0 to h, in parallel
func bwRows(h int, f func(y0, y1 int)) {
	n := runtime.GOMAXPROCS(0)
	if n > h/64 {
		n = h/64 + 1
	}
	var wg sync.WaitGroup
	for k := 0; k < n; k++ {
		wg.Add(1)
		go func(y0, y1 int) {
			defer wg.Done()
			f(y0, y1)
		}(h*k/n, h*(k+1)/n)
	}
	wg.Wait()
}

// bwNear gives, for the rows of a picture one after the other, the darkest
// and the lightest of each pixel's 3x3 neighbors. It keeps only three rows.
type bwNear struct {
	g      *image.Gray
	w, h   int
	y      int        // the row that next gives
	mn, mx [3][]uint8 // of the 3 pixels left to right, for rows y-1, y, y+1
	lo, hi []uint8    // the result
}

func newBWNear(g *image.Gray, y int) *bwNear {
	n := &bwNear{g: g, w: g.Rect.Dx(), h: g.Rect.Dy(), y: y}
	for k := range n.mn {
		n.mn[k], n.mx[k] = make([]uint8, n.w), make([]uint8, n.w)
	}
	n.lo, n.hi = make([]uint8, n.w), make([]uint8, n.w)
	n.across(1, y-1)
	n.across(2, y)
	return n
}

// across fills slot k with row y's darkest and lightest of 3 pixels
func (n *bwNear) across(k, y int) {
	if y < 0 || y >= n.h {
		return
	}
	s := n.g.Pix[y*n.g.Stride : y*n.g.Stride+n.w]
	mn, mx := n.mn[k], n.mx[k]
	for x, v := range s {
		a, b := v, v
		if x > 0 {
			if p := s[x-1]; p < a {
				a = p
			} else if p > b {
				b = p
			}
		}
		if x+1 < len(s) {
			if p := s[x+1]; p < a {
				a = p
			} else if p > b {
				b = p
			}
		}
		mn[x], mx[x] = a, b
	}
}

// next gives lo and hi for row y, and goes to the next row
func (n *bwNear) next() (lo, hi []uint8) {
	n.mn[0], n.mn[1], n.mn[2] = n.mn[1], n.mn[2], n.mn[0]
	n.mx[0], n.mx[1], n.mx[2] = n.mx[1], n.mx[2], n.mx[0]
	n.across(2, n.y+1)
	copy(n.lo, n.mn[1])
	copy(n.hi, n.mx[1])
	for _, k := range [2]int{0, 2} {
		if y := n.y + k - 1; y < 0 || y >= n.h {
			continue
		}
		mn, mx := n.mn[k], n.mx[k]
		for x := range n.lo {
			if mn[x] < n.lo[x] {
				n.lo[x] = mn[x]
			}
			if mx[x] > n.hi[x] {
				n.hi[x] = mx[x]
			}
		}
	}
	n.y++
	return n.lo, n.hi
}

// bwPhotoCells says which cells of bwCell x bwCell pixels are photo
func bwPhotoCells(g *image.Gray) []bool {
	w, h := g.Rect.Dx(), g.Rect.Dy()
	cw, ch := (w+bwCell-1)/bwCell, (h+bwCell-1)/bwCell
	soft := make([]uint16, cw*ch) // pixels of the cell that are not flat
	hard := make([]uint16, cw*ch) // and those on a hard edge
	bwRows(ch, func(c0, c1 int) {
		y0, y1 := c0*bwCell, c1*bwCell
		if y1 > h {
			y1 = h
		}
		n := newBWNear(g, y0)
		for y := y0; y < y1; y++ {
			lo, hi := n.next()
			row := (y / bwCell) * cw
			for x := range lo {
				if c := hi[x] - lo[x]; c != 0 {
					soft[row+x/bwCell]++
					if c >= bwHard {
						hard[row+x/bwCell]++
					}
				}
			}
		}
	})
	// a photo cell: with the 8 cells around it, nearly all pixels are not
	// flat, and few are on a hard edge
	photo := make([]bool, cw*ch)
	for cy := 0; cy < ch; cy++ {
		for cx := 0; cx < cw; cx++ {
			var s, hd, a int
			for y := cy - 1; y <= cy+1; y++ {
				for x := cx - 1; x <= cx+1; x++ {
					if x < 0 || x >= cw || y < 0 || y >= ch {
						continue
					}
					s += int(soft[y*cw+x])
					hd += int(hard[y*cw+x])
					a += (min(w, (x+1)*bwCell) - x*bwCell) * (min(h, (y+1)*bwCell) - y*bwCell)
				}
			}
			photo[cy*cw+cx] = s*100 > a*bwSoft && hd*100 <= a*bwHardMax
		}
	}
	// a smooth part in a photo is photo too: fill the holes, two times
	for k := 0; k < 2; k++ {
		o := append([]bool(nil), photo...)
		for cy := 0; cy < ch; cy++ {
			for cx := 0; cx < cw; cx++ {
				if o[cy*cw+cx] {
					continue
				}
				n := 0
				for y := cy - 1; y <= cy+1; y++ {
					for x := cx - 1; x <= cx+1; x++ {
						if x >= 0 && x < cw && y >= 0 && y < ch && o[y*cw+x] {
							n++
						}
					}
				}
				photo[cy*cw+cx] = n >= 5
			}
		}
	}
	return photo
}

// bwSharp makes g black and white: sharp, but for the photo cells (nil:
// none), which get Floyd-Steinberg
func bwSharp(g *image.Gray, photo []bool) *image.Gray {
	w, h := g.Rect.Dx(), g.Rect.Dy()
	cw := (w + bwCell - 1) / bwCell
	o := image.NewGray(g.Rect)
	if photo == nil { // no row needs the row before it
		bwRows(h, func(y0, y1 int) {
			n := newBWNear(g, y0)
			for y := y0; y < y1; y++ {
				lo, hi := n.next()
				s := g.Pix[y*g.Stride : y*g.Stride+w]
				d := o.Pix[y*o.Stride : y*o.Stride+w]
				for x, v := range s {
					d[x] = bwPixel(v, lo[x], hi[x], bwEdge)
				}
			}
		})
		return o
	}
	n := newBWNear(g, 0)
	cur, nxt := make([]int, w+2), make([]int, w+2) // the dither's errors
	for y := 0; y < h; y++ {
		lo, hi := n.next()
		s := g.Pix[y*g.Stride : y*g.Stride+w]
		d := o.Pix[y*o.Stride : y*o.Stride+w]
		cells := photo[(y/bwCell)*cw:]
		for x, v := range s {
			if !cells[x/bwCell] {
				d[x] = bwPixel(v, lo[x], hi[x], bwEdge)
				continue
			}
			if int(hi[x])-int(lo[x]) >= bwPEdge {
				d[x] = bwPixel(v, lo[x], hi[x], bwPEdge)
				continue
			}
			// Floyd-Steinberg, between the photo's black and white points
			t := (int(v) - bwLo) * 255 / (bwHi - bwLo)
			if t < 0 {
				t = 0
			} else if t > 255 {
				t = 255
			}
			t += cur[x+1] / 16
			q := 0
			if t >= 128 {
				q = 255
			}
			d[x] = uint8(q)
			e := t - q
			cur[x+2] += e * 7
			nxt[x] += e * 3
			nxt[x+1] += e * 5
			nxt[x+2] += e
		}
		cur, nxt = nxt, cur
		for x := range nxt {
			nxt[x] = 0
		}
	}
	return o
}

// bwPixel is 0 (black) or 255 for a pixel that is not dithered: v, with the
// darkest and lightest of its neighbors
func bwPixel(v, lo, hi uint8, edge int) uint8 {
	if int(hi)-int(lo) >= edge {
		if 2*int(v) < int(lo)+int(hi) {
			return 0
		}
		return 255
	}
	if v < bwFlat {
		return 0
	}
	return 255
}
