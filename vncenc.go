// WRP VNC encodings and frame differencing.
//
// Frames arrive from the CDP screencast as whole jpegs. They are decoded once
// per frame into a shared RGBA and hashed in tiles; each client diffs those
// hashes against what it last received and only the changed tiles get encoded.
// A client that lags several frames still gets a correct picture because the
// comparison is against its own hashes, not against the previous frame.
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"

	"github.com/ericpauley/go-quantize/quantize"
)

// Encodings we can send, best first. Raw is mandatory for every RFB client.
type vncEnc int

const (
	encTight vncEnc = iota
	encZlib
	encHextile
	encRaw
)

func (e vncEnc) String() string {
	switch e {
	case encTight:
		return "tight"
	case encZlib:
		return "zlib"
	case encHextile:
		return "hextile"
	}
	return "raw"
}

const (
	rfbEncodingRaw      = 0
	rfbEncodingHextile  = 5
	rfbEncodingZlib     = 6
	rfbEncodingDeskName = -307

	vncTile     = 64  // frame diff granularity
	vncHexTile  = 16  // hextile is defined on 16x16 tiles
	vncFullPct  = 60  // dirty above this percent of the frame, just send it whole
	vncMaxSubs  = 255 // hextile subrect count is a single byte
	vncFNVOff   = 14695981039346656037
	vncFNVPrime = 1099511628211
)

// ---- frame differencing ----

// Tile hashes of a frame, row major over a vncTile grid
func vncHashTiles(img *image.RGBA) (h []uint64, gw, gh int) {
	b := img.Bounds()
	w, ht := b.Dx(), b.Dy()
	gw = (w + vncTile - 1) / vncTile
	gh = (ht + vncTile - 1) / vncTile
	h = make([]uint64, gw*gh)
	for ty := 0; ty < gh; ty++ {
		y0, y1 := ty*vncTile, min((ty+1)*vncTile, ht)
		for tx := 0; tx < gw; tx++ {
			x0, x1 := tx*vncTile, min((tx+1)*vncTile, w)
			s := uint64(vncFNVOff)
			for y := y0; y < y1; y++ {
				o := img.PixOffset(b.Min.X+x0, b.Min.Y+y)
				for x := x0; x < x1; x++ {
					s = (s ^ uint64(binary.LittleEndian.Uint32(img.Pix[o:]))) * vncFNVPrime
					o += 4
				}
			}
			h[ty*gw+tx] = s
		}
	}
	return h, gw, gh
}

// Rectangles covering every tile that differs, merged into runs. A nil prev
// (new client, or resized) means everything is dirty.
func vncDirtyRects(cur, prev []uint64, gw, gh, w, h int) []image.Rectangle {
	full := []image.Rectangle{image.Rect(0, 0, w, h)}
	if prev == nil || len(prev) != len(cur) {
		return full
	}
	var out []image.Rectangle
	area := 0
	for ty := 0; ty < gh; ty++ {
		for x := 0; x < gw; {
			if cur[ty*gw+x] == prev[ty*gw+x] {
				x++
				continue
			}
			x0 := x
			for x < gw && cur[ty*gw+x] != prev[ty*gw+x] {
				x++
			}
			r := image.Rect(x0*vncTile, ty*vncTile, min(x*vncTile, w), min((ty+1)*vncTile, h))
			area += r.Dx() * r.Dy()
			// grow the run directly above instead of adding a rect
			if n := len(out); n > 0 && out[n-1].Min.X == r.Min.X && out[n-1].Max.X == r.Max.X && out[n-1].Max.Y == r.Min.Y {
				out[n-1].Max.Y = r.Max.Y
				continue
			}
			out = append(out, r)
		}
	}
	if area*100 > w*h*vncFullPct { // mostly changed, one rect beats many
		return full
	}
	return out
}

// ---- pixel conversion ----

func (pf vncPixFmt) bytes() int {
	if pf.bpp < 8 {
		return 1
	}
	return int(pf.bpp) / 8
}

// Pack one true color pixel into the client's format
func (pf vncPixFmt) pack(r, g, b uint32) uint32 {
	return ((r*uint32(pf.rMax)+127)/255)<<pf.rSh |
		((g*uint32(pf.gMax)+127)/255)<<pf.gSh |
		((b*uint32(pf.bMax)+127)/255)<<pf.bSh
}

func (pf vncPixFmt) appendPix(dst []byte, v uint32) []byte {
	n := pf.bytes()
	for i := 0; i < n; i++ {
		if pf.bigEnd {
			dst = append(dst, byte(v>>((n-1-i)*8)))
			continue
		}
		dst = append(dst, byte(v>>(i*8)))
	}
	return dst
}

// Pixel bytes for a rect in this client's format: dithered palette indices for
// colormap clients, dithered bytes at 8 bpp, packed true color otherwise.
func (c *vncClient) pixels(img *image.RGBA, pal *vncPal, r image.Rectangle) []byte {
	if c.indexed && pal != nil {
		if c.palVer != pal.ver { // palette moved, the cached lookups are stale
			c.lut = vncNewLUT()
			c.palVer = pal.ver
		}
		return vncDitherRect(img, r, func(r, g, b int32) (byte, int32, int32, int32) {
			i := pal.index(c.lut, r, g, b)
			v := pal.rgb[i]
			return i, v[0], v[1], v[2]
		})
	}
	pf := c.pf
	if pf.bpp == 8 { // 3-3-2 and friends go muddy without error diffusion
		rMax, gMax, bMax := int32(pf.rMax), int32(pf.gMax), int32(pf.bMax)
		if rMax < 1 || gMax < 1 || bMax < 1 {
			rMax, gMax, bMax = 7, 7, 3
		}
		return vncDitherRect(img, r, func(r, g, b int32) (byte, int32, int32, int32) {
			qr, qg, qb := (r*rMax+127)/255, (g*gMax+127)/255, (b*bMax+127)/255
			return byte(qr<<pf.rSh | qg<<pf.gSh | qb<<pf.bSh),
				(qr*255 + rMax/2) / rMax, (qg*255 + gMax/2) / gMax, (qb*255 + bMax/2) / bMax
		})
	}
	out := make([]byte, 0, r.Dx()*r.Dy()*pf.bytes())
	for y := r.Min.Y; y < r.Max.Y; y++ {
		o := img.PixOffset(r.Min.X, y)
		for x := r.Min.X; x < r.Max.X; x++ {
			out = pf.appendPix(out, pf.pack(uint32(img.Pix[o]), uint32(img.Pix[o+1]), uint32(img.Pix[o+2])))
			o += 4
		}
	}
	return out
}

// ---- encoders ----

func vncRectHdrEnc(x, y, w, h int, enc int32) *bytes.Buffer {
	b := &bytes.Buffer{}
	binary.Write(b, binary.BigEndian, uint16(x))
	binary.Write(b, binary.BigEndian, uint16(y))
	binary.Write(b, binary.BigEndian, uint16(w))
	binary.Write(b, binary.BigEndian, uint16(h))
	binary.Write(b, binary.BigEndian, enc)
	return b
}

// Raw: pixels straight out, every client understands this
func (c *vncClient) rectRaw(x, y int, pix []byte, w, h int) []byte {
	b := vncRectHdrEnc(x, y, w, h, rfbEncodingRaw)
	b.Write(pix)
	return b.Bytes()
}

// Zlib: raw pixels through the client's persistent deflate stream
func (c *vncClient) rectZlib(x, y int, pix []byte, w, h int) []byte {
	b := vncRectHdrEnc(x, y, w, h, rfbEncodingZlib)
	comp := c.deflate(pix)
	binary.Write(b, binary.BigEndian, uint32(len(comp)))
	b.Write(comp)
	return b.Bytes()
}

// Read pixel i out of a packed buffer as a comparable value
func pixAt(pix []byte, i, bpp int) uint64 {
	var v uint64
	for k := 0; k < bpp; k++ {
		v |= uint64(pix[i*bpp+k]) << (k * 8)
	}
	return v
}

func putPix(dst []byte, v uint64, bpp int) []byte {
	for k := 0; k < bpp; k++ {
		dst = append(dst, byte(v>>(k*8)))
	}
	return dst
}

// Hextile: 16x16 tiles, each solid, two colour with subrects, or raw. Supported
// by essentially every VNC client ever written, including the pre-Tight ones.
func (c *vncClient) rectHextile(x, y int, pix []byte, w, h int) []byte {
	bpp := c.pf.bytes()
	b := vncRectHdrEnc(x, y, w, h, rfbEncodingHextile)
	var bg, fg uint64
	haveBg, haveFg := false, false
	for ty := 0; ty < h; ty += vncHexTile {
		th := min(vncHexTile, h-ty)
		for tx := 0; tx < w; tx += vncHexTile {
			tw := min(vncHexTile, w-tx)
			// unique colours in this tile, bailing out past two
			var c0, c1 uint64
			n0, n1, distinct := 0, 0, 0
			for j := 0; j < th && distinct <= 2; j++ {
				for i := 0; i < tw; i++ {
					v := pixAt(pix, (ty+j)*w+tx+i, bpp)
					switch {
					case distinct == 0:
						c0, n0, distinct = v, 1, 1
					case v == c0:
						n0++
					case distinct == 1:
						c1, n1, distinct = v, 1, 2
					case v == c1:
						n1++
					default:
						distinct = 3
					}
					if distinct > 2 {
						break
					}
				}
			}
			if distinct > 2 { // too busy, raw tile
				b.WriteByte(1)
				for j := 0; j < th; j++ {
					o := ((ty+j)*w + tx) * bpp
					b.Write(pix[o : o+tw*bpp])
				}
				haveBg, haveFg = false, false
				continue
			}
			tbg, tfg := c0, c1
			if distinct == 2 && n1 > n0 {
				tbg, tfg = c1, c0
			}
			if distinct == 1 { // solid
				sub := byte(0)
				if !haveBg || tbg != bg {
					sub = 2
				}
				b.WriteByte(sub)
				if sub != 0 {
					b.Write(putPix(nil, tbg, bpp))
					bg, haveBg = tbg, true
				}
				continue
			}
			subs := hexSubrects(pix, w, tx, ty, tw, th, bpp, tfg)
			if len(subs) == 0 || len(subs) > vncMaxSubs { // degenerate, send raw
				b.WriteByte(1)
				for j := 0; j < th; j++ {
					o := ((ty+j)*w + tx) * bpp
					b.Write(pix[o : o+tw*bpp])
				}
				haveBg, haveFg = false, false
				continue
			}
			sub := byte(8) // AnySubrects
			if !haveBg || tbg != bg {
				sub |= 2
			}
			if !haveFg || tfg != fg {
				sub |= 4
			}
			b.WriteByte(sub)
			if sub&2 != 0 {
				b.Write(putPix(nil, tbg, bpp))
				bg, haveBg = tbg, true
			}
			if sub&4 != 0 {
				b.Write(putPix(nil, tfg, bpp))
				fg, haveFg = tfg, true
			}
			b.WriteByte(byte(len(subs)))
			b.Write(bytes.Join(subs, nil))
		}
	}
	return b.Bytes()
}

// Maximal runs of the foreground colour inside one hextile tile, grown
// downwards while the run below matches
func hexSubrects(pix []byte, stride, tx, ty, tw, th, bpp int, fg uint64) [][]byte {
	used := make([]bool, tw*th)
	var out [][]byte
	for j := 0; j < th; j++ {
		for i := 0; i < tw; {
			if used[j*tw+i] || pixAt(pix, (ty+j)*stride+tx+i, bpp) != fg {
				i++
				continue
			}
			w := 0
			for i+w < tw && !used[j*tw+i+w] && pixAt(pix, (ty+j)*stride+tx+i+w, bpp) == fg {
				w++
			}
			hgt := 1
			for j+hgt < th {
				ok := true
				for k := 0; k < w; k++ {
					if used[(j+hgt)*tw+i+k] || pixAt(pix, (ty+j+hgt)*stride+tx+i+k, bpp) != fg {
						ok = false
						break
					}
				}
				if !ok {
					break
				}
				hgt++
			}
			for jj := 0; jj < hgt; jj++ {
				for k := 0; k < w; k++ {
					used[(j+jj)*tw+i+k] = true
				}
			}
			out = append(out, []byte{byte(i<<4 | j), byte((w-1)<<4 | (hgt - 1))})
			i += w
			if len(out) > vncMaxSubs {
				return out
			}
		}
	}
	return out
}

// ---- helpers ----

// Decode a screencast jpeg into RGBA once, so every client shares the work
func vncDecodeRGBA(jpg []byte) *image.RGBA {
	i, err := jpeg.Decode(bytes.NewReader(jpg))
	if err != nil {
		return nil
	}
	if r, ok := i.(*image.RGBA); ok {
		return r
	}
	b := i.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), i, b.Min, draw.Src)
	return out
}

// Adaptive palette for colormap clients, shared by everyone using the same
// size. Immutable once built: clients keep their own lookup cache against it.
//
// Median cut is far too slow to rerun on every frame and the palette barely
// moves between them, so it is rebuilt every vncPalFrames frames instead.
type vncPal struct {
	ver          uint64 // bumped on every rebuild, clients drop their lut on change
	pal          color.Palette
	rgb          [][3]int32 // same colours, unpacked for the dither error term
	cmap         []byte     // SetColourMapEntries for this palette
	black, white byte       // reserved cursor slots
}

// Build a palette of n entries from a frame, the last two reserved so the
// cursor stays crisp black on white whatever the page looks like
func vncBuildPal(img *image.RGBA, n int, ver uint64) *vncPal {
	n = min(max(n, 2), 256)
	var cols color.Palette
	if n <= 3 { // nothing to quantize into, go monochrome
		cols = color.Palette{color.Black, color.White}
	} else {
		q := quantize.MedianCutQuantizer{}
		cols = q.Quantize(make([]color.Color, 0, n-2), img)
		cols = append(cols, color.Black, color.White)
	}
	p := &vncPal{ver: ver, pal: cols}
	p.black, p.white = byte(len(cols)-2), byte(len(cols)-1)
	p.rgb = make([][3]int32, len(cols))
	for i, c := range cols {
		r, g, b, _ := c.RGBA()
		p.rgb[i] = [3]int32{int32(r >> 8), int32(g >> 8), int32(b >> 8)}
	}
	p.cmap = vncColorMapMsg(cols)
	return p
}

// Nearest palette entry, memoized per client in a 5-5-5 rgb cube. The cache is
// what removes the linear palette scan from the per pixel path.
func (p *vncPal) index(lut []int16, r, g, b int32) byte {
	k := (r>>3)<<10 | (g>>3)<<5 | b>>3
	if v := lut[k]; v >= 0 {
		return byte(v)
	}
	i := byte(p.pal.Index(color.RGBA{uint8(r), uint8(g), uint8(b), 255}))
	lut[k] = int16(i)
	return i
}

func vncNewLUT() []int16 {
	l := make([]int16, 1<<15)
	for i := range l {
		l[i] = -1
	}
	return l
}

func clamp255(v int32) int32 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

// Floyd steinberg error diffusion over a rect. quant maps a colour to the
// output byte and reports the colour actually chosen, so the error pushed onto
// the neighbours is the real one. Without this, both the 3-3-2 true colour
// path and median cut output band badly on photos and gradients.
func vncDitherRect(img *image.RGBA, r image.Rectangle, quant func(r, g, b int32) (byte, int32, int32, int32)) []byte {
	w, h := r.Dx(), r.Dy()
	out := make([]byte, 0, w*h)
	cur := make([][3]int32, w+2)
	next := make([][3]int32, w+2)
	for y := 0; y < h; y++ {
		o := img.PixOffset(r.Min.X, r.Min.Y+y)
		for x := 0; x < w; x++ {
			cr := clamp255(int32(img.Pix[o]) + cur[x+1][0])
			cg := clamp255(int32(img.Pix[o+1]) + cur[x+1][1])
			cb := clamp255(int32(img.Pix[o+2]) + cur[x+1][2])
			o += 4
			idx, qr, qg, qb := quant(cr, cg, cb)
			out = append(out, idx)
			er, eg, eb := cr-qr, cg-qg, cb-qb
			for _, d := range [4]struct {
				row *[][3]int32
				x   int
				m   int32
			}{{&cur, x + 2, 7}, {&next, x, 3}, {&next, x + 1, 5}, {&next, x + 2, 1}} {
				(*d.row)[d.x][0] += er * d.m / 16
				(*d.row)[d.x][1] += eg * d.m / 16
				(*d.row)[d.x][2] += eb * d.m / 16
			}
		}
		cur, next = next, cur
		for i := range next {
			next[i] = [3]int32{}
		}
	}
	return out
}

// Crop and re-encode a region as jpeg for tight clients
func vncJpegRegion(img *image.RGBA, r image.Rectangle, qual int) []byte {
	var buf bytes.Buffer
	sub := img.SubImage(r)
	if err := jpeg.Encode(&buf, sub, &jpeg.Options{Quality: qual}); err != nil {
		return nil
	}
	return buf.Bytes()
}

// DesktopName pseudo rect: pushes the page title to the viewer's title bar
func vncRectDeskName(name string) []byte {
	b := vncRectHdrEnc(0, 0, 0, 0, rfbEncodingDeskName)
	binary.Write(b, binary.BigEndian, uint32(len(name)))
	b.WriteString(name)
	return b.Bytes()
}
