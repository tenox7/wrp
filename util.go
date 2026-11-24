// wrp utility functions
package main

import (
	"encoding/json"
	"image"
	"image/color"
	"image/color/palette"
	"image/draw"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ericpauley/go-quantize/quantize"
	"github.com/tenox7/gip"
)

func printMyIPs(b string) {
	ap := strings.Split(b, ":")
	if len(ap) < 2 {
		log.Fatal("Wrong format of ipaddress:port")
	}
	port := ap[len(ap)-1]
	if ap[0] != "" && ap[0] != "0.0.0.0" {
		log.Printf("Listen address: %v", b)
		return
	}
	a, err := net.InterfaceAddrs()
	if err != nil {
		log.Printf("Listen address: %v", b)
		return
	}
	var m string
	for _, i := range a {
		n, ok := i.(*net.IPNet)
		if !ok || n.IP.IsLoopback() || strings.Contains(n.IP.String(), ":") {
			continue
		}
		m += n.IP.String() + ":" + port + " "
	}
	log.Printf("Listen address: %v", m)
}

// gifPalette makes the picture one of n colors, and dither (the C menu's
// letter, see colorChoice) says what is dithered. 2 colors are black and
// white: see bw.go. 216 colors are the web colors: with no dither as GIP makes
// them (see webColors), or dithered. Other numbers get the page's own colors
// (median cut): dithered (see ditherPalette), or the nearest color.
func gifPalette(i image.Image, n int64, dither string) image.Image {
	switch {
	case n == 2:
		return toBW(i, dither)
	case n == 216 && dither != bwAll:
		return webColors(i)
	case n == 216:
		return ditherPalette(i, palette.WebSafe)
	}
	return toColors(i, medianCut(i, n), dither)
}

// medianCut gives the picture's own n colors, from some of its rows (see
// palSample)
func medianCut(i image.Image, n int64) color.Palette {
	q := quantize.MedianCutQuantizer{}
	return q.Quantize(make([]color.Color, 0, int(n)), palSample(i))
}

// toColors makes the picture one of the colors of p: dithered, or with
// dither bwNone each pixel the nearest color
func toColors(i image.Image, p color.Palette, dither string) *image.Paletted {
	if dither == bwNone {
		return toPalette(i, p)
	}
	return ditherPalette(i, p)
}

// webColors makes the picture one of the 216 web colors, with GIP's table:
// red, green and blue each go to the nearest of 0, 51, 102, 153, 204 and 255,
// with no dither. So a GIF of 216 colors is GIP's picture: the same pixels,
// and the same size. The colors are the same on every page, so a display
// with a colormap (8 bits) keeps them, and shares them with other programs
// that use the web colors. Text stays sharp; light tints become white, and
// photos get bands of color.
func webColors(i image.Image) *image.Paletted {
	b := i.Bounds()
	o := image.NewPaletted(b, palette.WebSafe)
	lut := &gip.FastGifLut
	n := runtime.GOMAXPROCS(0)
	if n > b.Dy() {
		n = b.Dy()
	}
	var wg sync.WaitGroup
	for w := 0; w < n; w++ {
		y0, y1 := b.Min.Y+b.Dy()*w/n, b.Min.Y+b.Dy()*(w+1)/n
		wg.Add(1)
		go func() {
			defer wg.Done()
			for y := y0; y < y1; y++ {
				d := o.Pix[o.PixOffset(b.Min.X, y):]
				var s []uint8 // the row's bytes, as GIP reads them
				switch p := i.(type) {
				case *image.RGBA:
					s = p.Pix[p.PixOffset(b.Min.X, y):]
				case *image.NRGBA:
					s = p.Pix[p.PixOffset(b.Min.X, y):]
				}
				for x := 0; x < b.Dx(); x++ {
					var r, g, bl uint8
					if s != nil {
						r, g, bl = s[4*x], s[4*x+1], s[4*x+2]
					} else {
						rr, gg, bb, _ := i.At(b.Min.X+x, y).RGBA()
						r, g, bl = uint8(rr>>8), uint8(gg>>8), uint8(bb>>8)
					}
					d[x] = 36*lut[r] + 6*lut[g] + lut[bl]
				}
			}
		}()
	}
	wg.Wait()
	return o
}

// palSamplePixels: median cut looks at all of a screen, but only at some rows
// of a whole page kept for slices. A page 75000 px tall has 59 million pixels:
// the palette took seconds and a gigabyte of memory, and every 30th row gives
// nearly the same one.
const palSamplePixels = 1 << 21

func palSample(i image.Image) image.Image {
	b := i.Bounds()
	px := b.Dx() * b.Dy()
	if px <= palSamplePixels {
		return i
	}
	k := (px + palSamplePixels - 1) / palSamplePixels
	o := image.NewRGBA(image.Rect(0, 0, b.Dx(), (b.Dy()+k-1)/k))
	for y := 0; y*k < b.Dy(); y++ {
		draw.Draw(o, image.Rect(0, y, b.Dx(), y+1), i, image.Pt(b.Min.X, b.Min.Y+y*k), draw.Src)
	}
	return o
}

// toPalette does what image.Paletted's Set does for each pixel (the nearest
// color, as color.Palette.Index finds it), with the same result, but fast. A
// page has few colors, so each part remembers the ones it has found, and the
// parts run in parallel. With Set, a page 75000 px tall took 40 seconds.
func toPalette(i image.Image, p color.Palette) *image.Paletted {
	b := i.Bounds()
	o := image.NewPaletted(b, p)
	pal := make([][4]uint32, len(p))
	for k, c := range p {
		r, g, bl, a := c.RGBA()
		pal[k] = [4]uint32{r, g, bl, a}
	}
	n := runtime.GOMAXPROCS(0)
	if n > b.Dy() {
		n = b.Dy()
	}
	var wg sync.WaitGroup
	for w := 0; w < n; w++ {
		y0, y1 := b.Min.Y+b.Dy()*w/n, b.Min.Y+b.Dy()*(w+1)/n
		wg.Add(1)
		go func() {
			defer wg.Done()
			mapRows(o, i, pal, y0, y1)
		}()
	}
	wg.Wait()
	return o
}

type palMemo struct {
	key uint32 // the pixel's 4 bytes
	idx uint8
	set bool
}

// mapRows maps rows y0 to y1-1 of i into o
func mapRows(o *image.Paletted, i image.Image, pal [][4]uint32, y0, y1 int) {
	b := i.Bounds()
	var memo [1 << 12]palMemo
	idx := func(p []uint8, nrgba bool) uint8 {
		key := uint32(p[0])<<24 | uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
		m := &memo[(key*2654435761)>>20]
		if !m.set || m.key != key {
			var r, g, bl, a uint32
			if nrgba {
				r, g, bl, a = color.NRGBA{p[0], p[1], p[2], p[3]}.RGBA()
			} else {
				r, g, bl, a = color.RGBA{p[0], p[1], p[2], p[3]}.RGBA()
			}
			*m = palMemo{key, nearest(pal, r, g, bl, a), true}
		}
		return m.idx
	}
	for y := y0; y < y1; y++ {
		d := o.Pix[o.PixOffset(b.Min.X, y):]
		switch s := i.(type) {
		case *image.RGBA:
			r := s.Pix[s.PixOffset(b.Min.X, y):]
			for x := 0; x < b.Dx(); x++ {
				d[x] = idx(r[4*x:4*x+4], false)
			}
		case *image.NRGBA:
			r := s.Pix[s.PixOffset(b.Min.X, y):]
			for x := 0; x < b.Dx(); x++ {
				d[x] = idx(r[4*x:4*x+4], true)
			}
		default:
			for x := b.Min.X; x < b.Max.X; x++ {
				o.Set(x, y, i.At(x, y))
			}
		}
	}
}

// ditherRowsMin: a band of fewer rows is not cut in two. Each band starts with
// no error from above, so a screen stays one band, as draw.FloydSteinberg
// does it, and only a whole page kept for slices is cut.
const ditherRowsMin = 256

// ditherBands is how many bands ditherPalette cuts h rows into
func ditherBands(h int) int {
	n := runtime.GOMAXPROCS(0)
	if n > h/ditherRowsMin {
		n = h / ditherRowsMin
	}
	if n < 1 {
		n = 1
	}
	return n
}

// ditherPalette makes the picture one of the colors of p, with Floyd-Steinberg
// dither: each pixel gives the error of its color to the next ones, so photos
// and shades keep their tones. A flat area in one of p's colors stays flat, so
// text on it stays sharp. Each band of rows is what draw.FloydSteinberg makes
// of it, but fast: each band remembers the colors it has found, and the bands
// run in parallel. With draw.FloydSteinberg, a page 20000 px tall took 4 s.
func ditherPalette(i image.Image, p color.Palette) *image.Paletted {
	b := i.Bounds()
	o := image.NewPaletted(b, p)
	pal := make([][4]uint32, len(p))
	for k, c := range p {
		r, g, bl, a := c.RGBA()
		pal[k] = [4]uint32{r, g, bl, a}
	}
	n := ditherBands(b.Dy())
	var wg sync.WaitGroup
	for w := 0; w < n; w++ {
		y0, y1 := b.Min.Y+b.Dy()*w/n, b.Min.Y+b.Dy()*(w+1)/n
		wg.Add(1)
		go func() {
			defer wg.Done()
			ditherRows(o, i, pal, y0, y1)
		}()
	}
	wg.Wait()
	return o
}

type ditherMemo struct {
	key uint64 // the color with its error, 16 bits for each of r, g, b, a
	idx uint8
	set bool
}

// ditherRows dithers rows y0 to y1-1 of i into o, with no error from above
// y0. It is draw.FloydSteinberg's loop: the same colors, errors and rounding.
func ditherRows(o *image.Paletted, i image.Image, pal [][4]uint32, y0, y1 int) {
	b := i.Bounds()
	w := b.Dx()
	cur, nxt := make([][4]int32, w+2), make([][4]int32, w+2)
	memo := make([]ditherMemo, 1<<14)
	px := func(x, y int) (r, g, b, a uint32) { return i.At(x, y).RGBA() }
	switch s := i.(type) {
	case *image.RGBA:
		px = func(x, y int) (r, g, b, a uint32) { return s.RGBAAt(x, y).RGBA() }
	case *image.NRGBA:
		px = func(x, y int) (r, g, b, a uint32) { return s.NRGBAAt(x, y).RGBA() }
	}
	for y := y0; y < y1; y++ {
		d := o.Pix[o.PixOffset(b.Min.X, y):]
		for x := 0; x < w; x++ {
			r, g, bl, a := px(b.Min.X+x, y)
			e := [4]int32{
				clamp16(int32(r) + cur[x+1][0]/16),
				clamp16(int32(g) + cur[x+1][1]/16),
				clamp16(int32(bl) + cur[x+1][2]/16),
				clamp16(int32(a) + cur[x+1][3]/16),
			}
			key := uint64(e[0])<<48 | uint64(e[1])<<32 | uint64(e[2])<<16 | uint64(e[3])
			m := &memo[(key*0x9e3779b97f4a7c15)>>50]
			if !m.set || m.key != key {
				*m = ditherMemo{key, nearest(pal, uint32(e[0]), uint32(e[1]), uint32(e[2]), uint32(e[3])), true}
			}
			k := m.idx
			d[x] = k
			for c := 0; c < 4; c++ {
				q := e[c] - int32(pal[k][c])
				nxt[x][c] += q * 3
				nxt[x+1][c] += q * 5
				nxt[x+2][c] += q
				cur[x+2][c] += q * 7
			}
		}
		cur, nxt = nxt, cur
		clear(nxt)
	}
}

func clamp16(v int32) int32 {
	if v < 0 {
		return 0
	}
	if v > 0xffff {
		return 0xffff
	}
	return v
}

// nearest is color.Palette.Index for a color given as r, g, b, a
func nearest(pal [][4]uint32, r, g, b, a uint32) uint8 {
	ret, best := 0, uint32(1<<32-1)
	for k, v := range pal {
		sum := sqDiff(r, v[0]) + sqDiff(g, v[1]) + sqDiff(b, v[2]) + sqDiff(a, v[3])
		if sum == 0 {
			return uint8(k)
		}
		if sum < best {
			ret, best = k, sum
		}
	}
	return uint8(ret)
}

// sqDiff is image/color's: the squared difference, shifted so that 4 of them
// fit in a uint32
func sqDiff(x, y uint32) uint32 {
	d := x - y
	return (d * d) >> 2
}

func asciify(s []byte) []byte {
	a := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			a[i] = '.'
			continue
		}
		a[i] = s[i]
	}
	return a
}

func findBrowser() string {
	var paths []string
	switch runtime.GOOS {
	case "darwin":
		paths = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
			"/Applications/Brave Browser Beta.app/Contents/MacOS/Brave Browser Beta",
			"/Applications/Brave Browser Nightly.app/Contents/MacOS/Brave Browser Nightly",
			"/Applications/Vivaldi.app/Contents/MacOS/Vivaldi",
			"/Applications/Arc.app/Contents/MacOS/Arc",
		}
	case "windows":
		pf := os.Getenv("ProgramFiles")
		pfx86 := os.Getenv("ProgramFiles(x86)")
		lad := os.Getenv("LOCALAPPDATA")
		paths = []string{
			filepath.Join(pf, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(pfx86, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(lad, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(pf, `BraveSoftware\Brave-Browser\Application\brave.exe`),
			filepath.Join(pfx86, `BraveSoftware\Brave-Browser\Application\brave.exe`),
			filepath.Join(lad, `BraveSoftware\Brave-Browser\Application\brave.exe`),
			filepath.Join(pf, `Chromium\Application\chrome.exe`),
			filepath.Join(lad, `Chromium\Application\chrome.exe`),
			filepath.Join(pf, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(pfx86, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(pf, `Vivaldi\Application\vivaldi.exe`),
		}
	default:
		paths = []string{
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
			"/usr/bin/brave-browser",
			"/usr/bin/brave-browser-stable",
			"/usr/bin/brave",
			"/usr/bin/microsoft-edge",
			"/usr/bin/vivaldi",
			"/snap/bin/chromium",
			"/snap/bin/brave",
			"/opt/brave.com/brave/brave",
			"/opt/google/chrome/chrome",
			"/opt/vivaldi/vivaldi",
			"/usr/local/bin/chrome",
			"/usr/local/bin/chromium",
			"/usr/local/bin/brave",
		}
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, n := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "brave-browser", "brave", "microsoft-edge", "vivaldi", "chrome"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

func fetchJnrbsnUserAgent() string {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("https://jnrbsn.github.io/user-agents/user-agents.json")
	if err != nil {
		log.Printf("Failed to fetch user agents from jnrbsn: %v", err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("jnrbsn API returned status: %d", resp.StatusCode)
		return ""
	}

	var userAgents []string
	if err := json.NewDecoder(resp.Body).Decode(&userAgents); err != nil {
		log.Printf("Failed to decode jnrbsn user agents JSON: %v", err)
		return ""
	}

	if len(userAgents) == 0 {
		log.Printf("jnrbsn API returned no user agents")
		return ""
	}

	log.Printf("Fetched user agent from jnrbsn: %s", userAgents[0])
	return userAgents[0]
}
