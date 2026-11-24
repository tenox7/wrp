package main

// A whole page kept for slices (sl=1) as a GIF is made its colors band by
// band, when a slice needs the band, and not all at once before WRP answers.
// Before, the answer waited for the whole page: for a page 79499 px tall, 2d
// took 4.7 s, 256d 2.1 s and 216d 1.4 s on a fast computer. An endless page
// (reddit) took longer than the client waits.
//
// A band is made with bandMargin rows above (and for 2 colors below) it,
// which it does not keep. So its dither starts above it, and its first rows
// get errors from the rows above them, as on the whole page: no seam shows.
// 2p (only photos dithered) finds the same photo cells as on the whole page:
// a cell's photo test sees the cells around it, and the two fills reach 4
// cells (32 rows) away. Without the dither, a band is what the whole page is.
//
// The bands are small, so that a slice's bands are made in parallel, each on
// a CPU: a slice of 549 rows is 9 bands. On an M1 Ultra, the first slice of
// a long page took 30 ms in 216d and 256d, against 85 ms with bands of 256
// rows. The page's colors (median cut, 20 ms) are found while the client
// asks for its first slice.
//
// The first slice has all the CPUs: the background starts when it is ready
// to go. Then the background makes the rest of the page with all the CPUs, a
// band on each: down from the last slice to the end of the page (the reader
// goes down), then up. So a scroll most often finds its rows ready, and a
// jump moves the background there. While a slice makes bands, the background
// takes no new band, so the slice soon has all the CPUs: a band takes about
// 30 ms. When every band is done, WRP lets the screenshot go. A new page
// request stops the background, so that the browser has the CPUs.

import (
	"image"
	"image/color"
	"image/color/palette"
	"image/draw"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const (
	bandRows   = 64 // rows of a band: a multiple of bwCell
	bandMargin = 40 // rows above and below a band that it sees: a multiple of bwCell
)

// A band with its margins (144 rows) is less than 2 x ditherRowsMin, so
// ditherPalette dithers it in one go: one band, from the top of its margin.
var _ = [1]int{}[(bandRows+2*bandMargin)/(2*ditherRowsMin)]

const (
	bandTodo = iota
	bandBusy // a slice or the background makes it now
	bandDone
)

type pageConv struct {
	src    image.Image // the screenshot; nil once all bands are done
	w, h   int
	n      int64  // colors
	dither string // bwAll, bwNone or bwPics (see colorChoice)
	name   string // for the log: the client and the picture's path

	once sync.Once
	out  *image.Paletted // the page in its colors: the rows of the bands that are done

	mu      sync.Mutex
	cond    *sync.Cond      // on mu: a slice has made its bands, or the background stops
	state   []uint8         // each band's: bandTodo, bandBusy or bandDone
	done    []chan struct{} // closed when the band is done
	left    int             // bands not done
	slicing int             // slices that make bands now: the background takes no new band
	k0, k1  int             // the bands of the last slice: the background goes down from there
	wake    chan struct{}   // the first slice is ready to go: the background starts (start)
	stop    chan struct{}   // closed when the background stops (close)
	closed  bool
}

func newPageConv(src image.Image, n int64, dither string) *pageConv {
	b := src.Bounds()
	nb := (b.Dy() + bandRows - 1) / bandRows
	c := &pageConv{
		src: src, w: b.Dx(), h: b.Dy(), n: n, dither: dither,
		state: make([]uint8, nb), done: make([]chan struct{}, nb), left: nb,
		wake: make(chan struct{}, 1), stop: make(chan struct{}),
	}
	for k := range c.done {
		c.done[k] = make(chan struct{})
	}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// colors makes out, with the page's colors: the same for all its bands.
// Median cut looks at some rows of the whole page (palSample).
func (c *pageConv) colors() {
	c.once.Do(func() {
		var p color.Palette
		switch {
		case c.n == 2:
			p = color.Palette{color.Black, color.White} // as paletted makes it
		case c.n == 216:
			p = palette.WebSafe
		default:
			p = medianCut(c.src, c.n)
		}
		c.out = image.NewPaletted(image.Rect(0, 0, c.w, c.h), p)
	})
}

// margins: the rows above and below a band that it needs to be made as on
// the whole page
func (c *pageConv) margins() (above, below int) {
	switch {
	case c.n == 2 && c.dither != bwAll:
		return bandMargin, bandMargin // 2p's photo cells, 3x3 neighbors
	case c.dither == bwAll:
		return bandMargin, 0 // the dither's errors come from above
	}
	return 0, 0 // each pixel on its own
}

// subImager: a picture that gives a part of itself, as RGBA and NRGBA do
type subImager interface {
	SubImage(image.Rectangle) image.Image
}

// subRows: rows y0 up to y1 of i, sharing its pixels where it can
func subRows(i image.Image, y0, y1 int) image.Image {
	b := i.Bounds()
	r := image.Rect(b.Min.X, b.Min.Y+y0, b.Max.X, b.Min.Y+y1)
	if s, ok := i.(subImager); ok {
		return s.SubImage(r)
	}
	o := image.NewRGBA(r)
	draw.Draw(o, r, i, r.Min, draw.Src)
	return o
}

// band makes band k in out: its rows with their margins, as gifPalette (and
// paletted for 2 colors) makes a whole page, but with the page's colors
func (c *pageConv) band(k int) {
	c.colors()
	y0 := k * bandRows
	y1 := min(y0+bandRows, c.h)
	above, below := c.margins()
	t0, t1 := max(0, y0-above), min(c.h, y1+below)
	part := subRows(c.src, t0, t1)
	var pix []uint8 // part's rows, from row t0 of the page
	var stride int
	gray := false
	switch {
	case c.n == 2:
		g := toBW(part, c.dither) // 0 or 255
		pix, stride, gray = g.Pix, g.Stride, true
	case c.n == 216 && c.dither != bwAll:
		p := webColors(part)
		pix, stride = p.Pix, p.Stride
	case c.n == 216:
		p := ditherPalette(part, palette.WebSafe)
		pix, stride = p.Pix, p.Stride
	default:
		p := toColors(part, c.out.Palette, c.dither)
		pix, stride = p.Pix, p.Stride
	}
	for y := y0; y < y1; y++ {
		s := pix[(y-t0)*stride : (y-t0)*stride+c.w]
		d := c.out.Pix[y*c.out.Stride : y*c.out.Stride+c.w]
		if !gray {
			copy(d, s)
			continue
		}
		for x, v := range s {
			d[x] = 0
			if v >= 128 {
				d[x] = 1
			}
		}
	}
	c.mu.Lock()
	c.state[k] = bandDone
	c.left--
	if c.left == 0 {
		c.src = nil // all bands are done
	}
	c.mu.Unlock()
	close(c.done[k])
}

// rows gives rows y up to y+h of the page in its colors, as cut does, once
// the bands they are in are done; nil if the page has no such rows
func (c *pageConv) rows(y, h int) image.Image {
	if y < 0 {
		y = 0
	}
	if y+h > c.h {
		h = c.h - y
	}
	if h <= 0 {
		return nil
	}
	c.need(y/bandRows, (y+h-1)/bandRows)
	return cut(c.out, y, h)
}

// need makes bands k0 to k1, in parallel (as many at a time as the CPUs),
// or waits for the ones that are made now, and moves the background there.
// While it makes bands, the background takes no new band.
func (c *pageConv) need(k0, k1 int) {
	var mine []int
	c.mu.Lock()
	for k := k0; k <= k1; k++ {
		if c.state[k] == bandTodo {
			c.state[k] = bandBusy
			mine = append(mine, k)
		}
	}
	c.k0, c.k1 = k0, k1
	if len(mine) > 0 {
		c.slicing++
	}
	c.mu.Unlock()
	if len(mine) > 0 {
		var wg sync.WaitGroup
		work := make(chan int)
		for range min(len(mine), runtime.GOMAXPROCS(0)) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for k := range work {
					c.band(k)
				}
			}()
		}
		for _, k := range mine {
			work <- k
		}
		close(work)
		wg.Wait()
		c.mu.Lock()
		c.slicing--
		c.cond.Broadcast() // the background may go on
		c.mu.Unlock()
	}
	for k := k0; k <= k1; k++ {
		<-c.done[k]
	}
}

// start: a slice is ready to go. After the first one, the background may
// take the CPUs.
func (c *pageConv) start() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// next takes the band that the background makes next: the first one not
// done down from the last slice to the end of the page, else the nearest one
// above it. It waits while a slice makes bands. -1: none is left, or the
// background stops.
func (c *pageConv) next() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.slicing > 0 && !c.closed {
		c.cond.Wait() // the slice comes first
	}
	if c.closed {
		return -1
	}
	for k := c.k0; k < len(c.state); k++ {
		if c.state[k] == bandTodo {
			c.state[k] = bandBusy
			return k
		}
	}
	for k := c.k0 - 1; k >= 0; k-- {
		if c.state[k] == bandTodo {
			c.state[k] = bandBusy
			return k
		}
	}
	return -1
}

// background finds the page's colors, waits for the first slice (start),
// then makes the rest of the page with all the CPUs, a band on each (see
// next), until it is done or stopped (close)
func (c *pageConv) background() {
	c.colors()
	select {
	case <-c.wake:
	case <-c.stop:
		return
	}
	st, cpus := time.Now(), runtime.GOMAXPROCS(0)
	var n atomic.Int32
	var wg sync.WaitGroup
	for range cpus {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := c.next(); k >= 0; k = c.next() {
				c.band(k)
				n.Add(1)
			}
		}()
	}
	wg.Wait()
	if c.name == "" || n.Load() == 0 {
		return
	}
	ms := time.Since(st).Milliseconds()
	select {
	case <-c.stop:
		log.Printf("%s Stopped the background for a new page, after %d bands of %d rows, %vms\n",
			c.name, n.Load(), bandRows, ms)
	default:
		log.Printf("%s Made the rest of the page in the background: %d bands of %d rows on %d CPUs, %vms\n",
			c.name, n.Load(), bandRows, cpus, ms)
	}
}

// close stops the background: WRP has a new page request
func (c *pageConv) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.stop)
		c.cond.Broadcast() // a worker that waits for a slice ends
	}
}
