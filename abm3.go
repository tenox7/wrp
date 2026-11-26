// WRP ABM3 pictures: the dense black and white picture, for a client whose
// line is slower than its CPU (Accept: image/x-abm3), such as Archaic at
// 9600 baud. It is about 39% smaller than ABM (see abm.go), and its
// decoder works for each coded byte and pixel: a 10 MHz 68010 needs 2.75
// seconds for one screen (ABM: 0.2 seconds).
//
// The file:
//
//	"ABM"           3 bytes
//	how packed      1 byte: '3'
//	width, height   2 bytes each, the high byte first
//	depth           1 byte: bits a pixel, 1
//	flags           1 byte: 0
//	the rows        coded bits, see below
//
// The rows are bits from an adaptive binary arithmetic coder: the MQ
// coder's 47 states (as in JBIG2 and JPEG 2000), with plain bytes (no
// byte stuffing: the encoder adds a carry to the bytes before). A bit is
// coded in a context: a state of its own, which learns how likely the bit
// is there. A context starts in state 0; the flags' contexts start with 1
// as the likely bit, the pixels' with 0 (white).
//
// The bits, for each row from the top:
//
//	same row        1: the row is as the row above (the first row: white).
//	                Context: this flag of the row before (the first row: 1).
//	white row       if not the same: 1: the row is all white. One context.
//	same bytes      if neither: for each byte of the row, 1: the byte is as
//	                the byte above. All of a row's flags come before its
//	                pixels. Context, 5 bits: this flag of the byte before
//	                in the row (the first byte: 1), 2 if the byte above is
//	                white, and at 4, 8 and 16 this flag of the three bytes
//	                above: left of it, at it, right of it (outside the row:
//	                1. After a same row they are all 1, after a white row
//	                all 0).
//	pixels          then each pixel of the bytes that are not the same,
//	                from the left: 1 is black. Context, 14 bits: bits 0 to
//	                3 are the 4 pixels before it in its row (bit 0: the
//	                nearest). Then for the places x-2 to x+2: the pixel of
//	                the row above (bits 4, 6, 8, 10, 12) and of the row two
//	                up (bits 5, 7, 9, 11, 13). Outside the picture: white.
//
// The file has all of the coder's bytes. A decoder reads at most 1 byte
// more than the file has (as 0): if it wants more, the file was cut.
package main

import (
	"bytes"
	"errors"
	"image"
	"io"
)

const abm3Type = "image/x-abm3"

// the MQ coder's states: the size of the less likely bit's part (Qe), the
// next state after the more likely bit (MPS) and after the less likely one
// (LPS), and 1 if an LPS swaps which bit is the likely one
var mqStates = [47][4]uint16{
	{0x5601, 1, 1, 1}, {0x3401, 2, 6, 0}, {0x1801, 3, 9, 0}, {0x0AC1, 4, 12, 0},
	{0x0521, 5, 29, 0}, {0x0221, 38, 33, 0}, {0x5601, 7, 6, 1}, {0x5401, 8, 14, 0},
	{0x4801, 9, 14, 0}, {0x3801, 10, 14, 0}, {0x3001, 11, 17, 0}, {0x2401, 12, 18, 0},
	{0x1C01, 13, 20, 0}, {0x1601, 29, 21, 0}, {0x5601, 15, 14, 1}, {0x5401, 16, 14, 0},
	{0x5101, 17, 15, 0}, {0x4801, 18, 16, 0}, {0x3801, 19, 17, 0}, {0x3401, 20, 18, 0},
	{0x3001, 21, 19, 0}, {0x2801, 22, 19, 0}, {0x2401, 23, 20, 0}, {0x2201, 24, 21, 0},
	{0x1C01, 25, 22, 0}, {0x1801, 26, 23, 0}, {0x1601, 27, 24, 0}, {0x1401, 28, 25, 0},
	{0x1201, 29, 26, 0}, {0x1101, 30, 27, 0}, {0x0AC1, 31, 28, 0}, {0x09C1, 32, 29, 0},
	{0x08A1, 33, 30, 0}, {0x0521, 34, 31, 0}, {0x0441, 35, 32, 0}, {0x02A1, 36, 33, 0},
	{0x0221, 37, 34, 0}, {0x0141, 38, 35, 0}, {0x0111, 39, 36, 0}, {0x0085, 40, 37, 0},
	{0x0049, 41, 38, 0}, {0x0025, 42, 39, 0}, {0x0015, 43, 40, 0}, {0x0009, 44, 41, 0},
	{0x0005, 45, 42, 0}, {0x0001, 45, 43, 0}, {0x5601, 46, 46, 0},
}

// A context is a byte: 2 * state + the likely bit. For such a byte: Qe,
// and the context after an MPS and after an LPS
var (
	mqQe         [94]uint32
	mqMPS, mqLPS [94]byte
)

func init() {
	for i, t := range mqStates {
		for m := uint16(0); m < 2; m++ {
			s := 2*i + int(m)
			mqQe[s] = uint32(t[0])
			mqMPS[s] = byte(2*t[1] + m)
			mqLPS[s] = byte(2*t[2] + (m ^ t[3]))
		}
	}
}

// ABM3's contexts: the pixels' first, then the flags'
const (
	abm3Pix   = 0     // 16384 for the pixels
	abm3Byte  = 16384 // 32 for the same bytes
	abm3Row   = 16416 // 2 for the same row
	abm3White = 16418 // 1 for the white row
	abm3NCtx  = 16419
)

func abm3Contexts() []byte {
	st := make([]byte, abm3NCtx)
	for i := abm3Byte; i < abm3NCtx; i++ {
		st[i] = 1
	}
	return st
}

// mqEnc is the coder. a is the interval's size (0x8000 and up between two
// bits). c is its low end: the low 16 bits and nb more bits above them are
// still open, the bytes before them are in out.
type mqEnc struct {
	st  []byte
	a   uint32
	c   uint32
	nb  uint
	out []byte
}

// a carry out of c goes into the bytes before
func (e *mqEnc) carry() {
	if e.c>>(16+e.nb) == 0 {
		return
	}
	e.c &^= 1 << (16 + e.nb)
	for i := len(e.out) - 1; i >= 0; i-- {
		e.out[i]++
		if e.out[i] != 0 {
			break
		}
	}
}

func (e *mqEnc) shift() {
	e.c <<= 1
	e.nb++
	if e.nb == 8 {
		e.out = append(e.out, byte(e.c>>16))
		e.c &= 0xffff
		e.nb = 0
	}
}

// code one bit in the context cx
func (e *mqEnc) code(cx int, bit byte) {
	s := e.st[cx]
	q := mqQe[s]
	e.a -= q
	if bit == s&1 {
		if e.a&0x8000 != 0 {
			e.c += q
			e.carry()
			return
		}
		if e.a < q {
			e.a = q
		} else {
			e.c += q
			e.carry()
		}
		e.st[cx] = mqMPS[s]
	} else {
		if e.a < q {
			e.c += q
			e.carry()
		} else {
			e.a = q
		}
		e.st[cx] = mqLPS[s]
	}
	for e.a < 0x8000 {
		e.a <<= 1
		e.shift()
	}
}

// the end: of the numbers in the interval, the one with the most zeros at
// its end, and all of its bytes
func (e *mqEnc) flush() []byte {
	for k := uint(15); ; k-- {
		v := (e.c + e.a - 1) &^ (1<<k - 1)
		if v >= e.c || k == 0 {
			e.c = v
			break
		}
	}
	e.carry()
	for i := 0; i < 16; i++ {
		e.shift()
	}
	for e.nb != 0 {
		e.shift()
	}
	return e.out
}

// pixel x of a row of w pixels: 1 is black, 0 outside the row
func abm3Px(row []byte, w, x int) int {
	if x < 0 || x >= w {
		return 0
	}
	return int(row[x>>3]>>(7-uint(x&7))) & 1
}

// the context of pixel x: cur is its row, up the row above, up2 the row
// two up
func abm3PixCtx(cur, up, up2 []byte, w, x int) int {
	c := 0
	for i := 0; i < 4; i++ {
		c |= abm3Px(cur, w, x-1-i) << uint(i)
	}
	for k := 0; k < 5; k++ {
		c |= abm3Px(up, w, x-2+k) << uint(4+2*k)
		c |= abm3Px(up2, w, x-2+k) << uint(5+2*k)
	}
	return c
}

// abm3Pack codes rows of bpl bytes, w pixels each (the bits after them in
// a row's last byte are 0)
func abm3Pack(bits []byte, bpl, w int) []byte {
	e := &mqEnc{st: abm3Contexts(), a: 0x8000}
	zero := make([]byte, bpl)
	// the same flags of the row above and of this row: [0] and [bpl+1]
	// are outside the row
	fa := bytes.Repeat([]byte{1}, bpl+2)
	fc := bytes.Repeat([]byte{1}, bpl+2)
	up, up2 := zero, zero
	prev := byte(1)
	for at := 0; at+bpl <= len(bits); at += bpl {
		cur := bits[at : at+bpl]
		same := byte(0)
		if bytes.Equal(cur, up) {
			same = 1
		}
		e.code(abm3Row+int(prev), same)
		prev = same
		white := byte(0)
		switch {
		case same == 1:
			for i := 1; i <= bpl; i++ {
				fa[i] = 1
			}
		default:
			if bytes.Equal(cur, zero) {
				white = 1
			}
			e.code(abm3White, white)
			if white == 1 {
				for i := 1; i <= bpl; i++ {
					fa[i] = 0
				}
				break
			}
			p := 1
			for xb := 0; xb < bpl; xb++ {
				cx := p | int(fa[xb])<<2 | int(fa[xb+1])<<3 | int(fa[xb+2])<<4
				if up[xb] == 0 {
					cx |= 2
				}
				p = 0
				if cur[xb] == up[xb] {
					p = 1
				}
				e.code(abm3Byte+cx, byte(p))
				fc[xb+1] = byte(p)
			}
			for xb := 0; xb < bpl; xb++ {
				if fc[xb+1] == 1 {
					continue
				}
				for x := xb * 8; x < xb*8+8 && x < w; x++ {
					e.code(abm3Pix+abm3PixCtx(cur, up, up2, w, x), cur[xb]>>(7-uint(x&7))&1)
				}
			}
			fa, fc = fc, fa
		}
		up2, up = up, cur
	}
	return e.flush()
}

// encodeABM3 writes a black and white picture as ABM3
func encodeABM3(w io.Writer, p *image.Paletted) error {
	bits, bpl := abmBits(p)
	if bits == nil {
		return errors.New("abm3: not a black and white picture")
	}
	return writeABM(w, '3', p.Rect.Dx(), p.Rect.Dy(), abm3Pack(bits, bpl, p.Rect.Dx()))
}
