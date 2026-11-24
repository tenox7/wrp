package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/color/palette"
	"image/gif"
	"math/rand"
	"net/http/httptest"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"
)

// convPage: a tall page, w x h, as a screenshot: text on white, and in every
// 600 rows a photo (soft noise) 300 rows tall, which crosses a band's edge,
// and a color gradient across the page
func convPage(w, h int) *image.RGBA {
	i := image.NewRGBA(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewSource(5))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{255, 255, 255, 255}
			switch m := y % 600; {
			case m >= 200 && m < 500 && x >= w/4 && x < 3*w/4: // a photo
				v := 60 + (x+y)%120 + r.Intn(30)
				c = color.RGBA{uint8(v), uint8(v * 3 / 4), uint8(255 - v), 255}
			case m >= 520 && m < 580: // a gradient
				c = color.RGBA{uint8(x * 255 / w), uint8((m - 520) * 4), 160, 255}
			case m%16 < 9 && x%9 < 6 && (x/9+m/16)%5 != 0: // text
				c = color.RGBA{0, 0, 0, 255}
				if (y/600)%2 == 1 {
					c = color.RGBA{40, 60, 170, 255} // a link
				}
			}
			i.SetRGBA(x, y, c)
		}
	}
	return i
}

// samePix fails the test at the first pixel that differs
func samePix(t *testing.T, what string, got, want *image.Paletted) {
	t.Helper()
	b := want.Bounds()
	if got.Bounds() != b {
		t.Fatalf("%s: bounds %v, want %v", what, got.Bounds(), b)
	}
	if len(got.Palette) != len(want.Palette) {
		t.Fatalf("%s: %d colors, want %d", what, len(got.Palette), len(want.Palette))
	}
	for k := range want.Palette {
		if got.Palette[k] != want.Palette[k] {
			t.Fatalf("%s: color %d is %v, want %v", what, k, got.Palette[k], want.Palette[k])
		}
	}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if g, w := got.ColorIndexAt(x, y), want.ColorIndexAt(x, y); g != w {
				t.Fatalf("%s: pixel %d,%d is %d, want %d", what, x, y, g, w)
			}
		}
	}
}

// meanRGB: the mean red, green and blue of a picture, 0 to 255
func meanRGB(p *image.Paletted) (m [3]float64) {
	b := p.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := p.Palette[p.ColorIndexAt(x, y)].RGBA()
			m[0] += float64(r >> 8)
			m[1] += float64(g >> 8)
			m[2] += float64(bl >> 8)
		}
	}
	for c := range m {
		m[c] /= float64(b.Dx() * b.Dy())
	}
	return m
}

// doneBands lists the bands that are done
func (c *pageConv) doneBands() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	var d []int
	for k, s := range c.state {
		if s == bandDone {
			d = append(d, k)
		}
	}
	return d
}

// Without the dither, the bands make exactly the whole page
func TestPageConvExact(t *testing.T) {
	src := convPage(200, 2000)
	for _, m := range []struct {
		n int64
		d string
	}{{2, bwNone}, {16, bwNone}, {216, bwNone}, {256, bwNone}} {
		c := newPageConv(src, m.n, m.d)
		samePix(t, fmt.Sprintf("%d%s", m.n, m.d), c.rows(0, 2000).(*image.Paletted),
			paletted(gifPalette(src, m.n, m.d)).(*image.Paletted))
	}
}

// 2p: the bands find the photo cells of the whole page, so every pixel but
// the dithered ones is the whole page's, and the dithered ones are as light
func TestPageConvPics(t *testing.T) {
	const w, h = 200, 2000
	src := convPage(w, h)
	got := newPageConv(src, 2, bwPics).rows(0, h).(*image.Paletted)
	whole := paletted(toBW(src, bwPics)).(*image.Paletted)
	photo := bwPhotoCells(toGray(src))
	cw := (w + bwCell - 1) / bwCell
	var n, gw, ww int
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			g, v := got.Pix[y*got.Stride+x], whole.Pix[y*whole.Stride+x]
			if photo[(y/bwCell)*cw+x/bwCell] {
				n, gw, ww = n+1, gw+int(g), ww+int(v)
				continue
			}
			if g != v {
				t.Fatalf("2p: pixel %d,%d is %d, the whole page's %d", x, y, g, v)
			}
		}
	}
	if n < w*h/10 {
		t.Fatalf("2p: only %d pixels in photo cells", n)
	}
	if d := gw - ww; d*100 > n || -d*100 > n {
		t.Errorf("2p: %d white pixels in the photos, the whole page has %d", gw, ww)
	}
}

// With the dither: each band is what the whole-page function makes of its
// rows with bandMargin rows above them, made in any order, also while
// slices come at the same time; and the page is as light as the whole page
func TestPageConvDither(t *testing.T) {
	const w, h = 200, 2000
	src := convPage(w, h)
	for _, m := range []struct {
		n int64
		d string
	}{{2, bwAll}, {16, bwAll}, {216, bwAll}, {256, bwAll}} {
		name := fmt.Sprintf("%d%s", m.n, m.d)
		one := newPageConv(src, m.n, m.d) // a band at a time, from the bottom
		for k := len(one.state) - 1; k >= 0; k-- {
			one.rows(k*bandRows, 1)
		}
		ref := one.rows(0, h).(*image.Paletted)

		many := newPageConv(src, m.n, m.d) // slices at the same time, and the background
		go many.background()
		many.start()
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := rand.New(rand.NewSource(int64(g)))
				for s := 0; s < 5; s++ {
					many.rows(r.Intn(h), 1+r.Intn(600))
				}
			}()
		}
		wg.Wait()
		samePix(t, name+": in another order", many.rows(0, h).(*image.Paletted), ref)
		many.close()

		// band 3: rows 768 to 1023, from the dither of rows 728 to 1023
		y0 := 3 * bandRows
		part := subRows(src, y0-bandMargin, y0+bandRows)
		var p *image.Paletted
		switch m.n {
		case 2:
			p = paletted(toBW(part, m.d)).(*image.Paletted)
		case 216:
			p = ditherPalette(part, palette.WebSafe)
		default:
			p = toColors(part, ref.Palette, m.d)
		}
		samePix(t, name+": band 3", cut(ref, y0, bandRows).(*image.Paletted),
			cut(p, bandMargin, bandRows).(*image.Paletted))

		got, want := meanRGB(ref), meanRGB(paletted(gifPalette(src, m.n, m.d)).(*image.Paletted))
		for c := range got {
			if d := got[c] - want[c]; d > 1 || d < -1 {
				t.Errorf("%s: mean %v, the whole page's %v", name, got, want)
				break
			}
		}
	}
}

// A slice makes only its bands. Then the background goes down from it to
// the end of the page, then up from it, the nearest first. A jump down moves
// it there.
func TestPageConvOrder(t *testing.T) {
	c := newPageConv(convPage(50, 40*bandRows), 16, bwAll)
	c.rows(10*bandRows+bandRows/2, bandRows)
	if d := c.doneBands(); !slices.Equal(d, []int{10, 11}) {
		t.Fatalf("a slice in bands 10 and 11: bands %v are done", d)
	}
	var got, want []int
	for range 3 {
		got = append(got, c.next())
	}
	c.rows(30*bandRows, 10) // a jump down, to band 30
	for k := c.next(); k >= 0; k = c.next() {
		got = append(got, k)
	}
	for k := 12; k <= 14; k++ { // below the slice
		want = append(want, k)
	}
	for k := 31; k < 40; k++ { // below the jump, to the end
		want = append(want, k)
	}
	for k := 29; k >= 15; k-- { // up from the jump
		want = append(want, k)
	}
	for k := 9; k >= 0; k-- { // above the first slice
		want = append(want, k)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the background's order:\n%v, want\n%v", got, want)
	}
}

// The background waits for the first slice to be ready to go (start), then
// makes the whole page, lets the screenshot go and ends. close stops it.
func TestPageConvBackground(t *testing.T) {
	const nb = 40
	c := newPageConv(convPage(50, nb*bandRows), 16, bwAll)
	exited := make(chan struct{})
	go func() {
		c.background()
		close(exited)
	}()
	c.rows(10*bandRows+bandRows/2, bandRows)
	time.Sleep(50 * time.Millisecond)
	if d := c.doneBands(); !slices.Equal(d, []int{10, 11}) {
		t.Fatalf("before the first slice is ready to go, bands %v are done", d)
	}
	c.start()
	select {
	case <-exited:
	case <-time.After(20 * time.Second):
		t.Fatalf("the background did not end: bands %v are done", c.doneBands())
	}
	if d := c.doneBands(); len(d) != nb {
		t.Fatalf("the background ended with bands %v done, want all %d", d, nb)
	}
	c.mu.Lock()
	kept := c.src != nil
	c.mu.Unlock()
	if kept {
		t.Error("every band is done, but the screenshot is kept")
	}
	if c.rows(nb*bandRows, 10) != nil || c.rows(0, 0) != nil {
		t.Error("rows past the end: want nil")
	}

	for _, first := range []bool{false, true} { // close before the first slice, and after it
		c := newPageConv(convPage(200, 100*bandRows), 2, bwAll)
		exited := make(chan struct{})
		go func() {
			c.background()
			close(exited)
		}()
		if first {
			c.rows(0, 1)
			c.start()
		}
		c.close()
		n := len(c.doneBands())
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Fatal("close did not stop the background")
		}
		if d := len(c.doneBands()); d > n+runtime.GOMAXPROCS(0) {
			t.Errorf("first slice %v: after close, the background made %d bands", first, d-n)
		}
	}
}

// While a slice makes bands, the background takes no new band. Then it goes
// on, or ends when it is stopped.
func TestPageConvPause(t *testing.T) {
	for _, stop := range []bool{false, true} {
		c := newPageConv(convPage(50, 20*bandRows), 16, bwAll)
		c.rows(0, 1)
		c.mu.Lock()
		c.slicing++ // as need does while it makes bands
		c.mu.Unlock()
		exited := make(chan struct{})
		go func() {
			c.background()
			close(exited)
		}()
		c.start()
		time.Sleep(100 * time.Millisecond)
		if d := c.doneBands(); !slices.Equal(d, []int{0}) {
			t.Fatalf("while a slice makes bands, the background made bands: %v", d)
		}
		want := 20
		if stop {
			c.close()
			want = 1
		} else {
			c.mu.Lock()
			c.slicing--
			c.cond.Broadcast()
			c.mu.Unlock()
		}
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Fatalf("stop %v: the background did not end", stop)
		}
		if d := c.doneBands(); len(d) != want {
			t.Errorf("stop %v: bands %v are done, want %d", stop, d, want)
		}
	}
}

// The slice server makes the bands that a slice needs: a GIF, or ABM for a
// client that takes it. After the first slice, the background makes the
// rest. A new page request stops it, and its slices still come.
func TestSliceServerConv(t *testing.T) {
	wrpCach.clear()
	c := newPageConv(convPage(120, 1500), 2, bwPics)
	wrpCach.addConv("/img/p.gif", c, 75)
	exited := make(chan struct{})
	go func() {
		c.background()
		close(exited)
	}()
	get := func(u, accept string) *httptest.ResponseRecorder {
		rq := httptest.NewRequest("GET", u, nil)
		if accept != "" {
			rq.Header.Set("Accept", accept)
		}
		rr := httptest.NewRecorder()
		imgServerMap(rr, rq)
		return rr
	}
	rr := get("/img/p.gif?y=500&h=300", "")
	if rr.Code != 200 || rr.Header().Get("Content-Type") != "image/gif" {
		t.Fatalf("slice 500,300: code %d, %q", rr.Code, rr.Header().Get("Content-Type"))
	}
	i, err := gif.Decode(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("slice 500,300: not a GIF: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("after the first slice, the background did not make the page")
	}
	if d := c.doneBands(); len(d) != len(c.state) {
		t.Fatalf("after the background, bands %v are done", d)
	}
	want := c.rows(500, 300)
	if i.Bounds() != want.Bounds() {
		t.Fatalf("slice 500,300: bounds %v, want %v", i.Bounds(), want.Bounds())
	}
	for y := 0; y < 300; y++ {
		for x := 0; x < 120; x++ {
			if white(i.At(x, y)) != white(want.At(x, y)) {
				t.Fatalf("slice 500,300: pixel %d,%d is not the page's", x, y)
			}
		}
	}
	if rr = get("/img/p.gif?y=0&h=100", abmType); rr.Header().Get("Content-Type") != abmType {
		t.Errorf("a client that takes ABM: %q", rr.Header().Get("Content-Type"))
	}
	if rr = get("/img/p.gif", ""); rr.Code != 200 {
		t.Errorf("the whole page: code %d", rr.Code)
	} else if i, err = gif.Decode(bytes.NewReader(rr.Body.Bytes())); err != nil || i.Bounds().Dy() != 1500 {
		t.Errorf("the whole page: %v, %v", err, i)
	}
	if rr = get("/img/p.gif?y=1500&h=10", ""); rr.Code != 404 {
		t.Errorf("rows past the end: code %d, want 404", rr.Code)
	}

	c2 := newPageConv(convPage(120, 1500), 2, bwAll)
	wrpCach.addConv("/img/q.gif", c2, 75)
	wrpCach.stopConvs() // a new page request
	select {
	case <-c2.stop:
	default:
		t.Error("a new page request did not stop the background")
	}
	if rr = get("/img/q.gif?y=1000&h=300", ""); rr.Code != 200 {
		t.Errorf("a slice after the background stopped: code %d", rr.Code)
	}
	c3 := newPageConv(convPage(120, 1500), 2, bwAll)
	wrpCach.addConv("/img/r.gif", c3, 75)
	wrpCach.clear() // a new page
	select {
	case <-c3.stop:
	default:
		t.Error("a new page did not stop the background")
	}
}

// A page 79500 px tall, as virtuallyfun.com's: the answer does not wait for
// its colors, and a slice makes only its bands
func BenchmarkPageConvSlice(b *testing.B) {
	src := convPage(775, 79500)
	for _, m := range []struct {
		n int64
		d string
	}{{2, bwAll}, {2, bwPics}, {16, bwAll}, {256, bwAll}} {
		b.Run(fmt.Sprintf("%d%s", m.n, m.d), func(b *testing.B) {
			for k := 0; k < b.N; k++ {
				newPageConv(src, m.n, m.d).rows(40000, 549)
			}
		})
	}
}
