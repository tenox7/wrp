package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// A real browser rendering a real page, scrolled with real wheel events. Unit
// tests cannot catch this class of bug on their own: synthetic frames do not
// have chrome's antialiasing, its smooth scrolling or its jpeg encoder, which
// between them are why exact row matching never fired on live content.
//
// Off by default - it needs a browser and takes a few seconds. Run it with
// WRP_BROWSER_TEST=1 go test -run TestBrowserScroll -v .
func TestBrowserScroll(t *testing.T) {
	if os.Getenv("WRP_BROWSER_TEST") == "" {
		t.Skip("set WRP_BROWSER_TEST=1 to run, needs a browser")
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, testHTMLPage())
	}))
	defer page.Close()

	defGeom = geom{w: 1152, h: 600, c: 216}
	*headless = true
	if err := chromedpStart(); err != nil {
		t.Fatalf("start browser: %v", err)
	}
	defer brStop()
	// load the page before the server's workers exist, so they are not racing
	// chromedp to bring the browser up
	ctx, cncl := context.WithTimeout(brCtx(), 60*time.Second)
	defer cncl()
	if err := chromedp.Run(ctx, chromedp.Navigate(page.URL), chromedp.WaitReady("#end", chromedp.ByID)); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	vncInit()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	vncWorkers()
	vncServe(ln)
	vncCastStart()
	time.Sleep(2 * time.Second) // let the first frames land

	for _, g := range []struct {
		name string
		do   func(*protoConn)
	}{
		{"wheel", (*protoConn).wheel},
		{"arrow key", (*protoConn).arrowDown},
	} {
		base := browserScroll(t, ln.Addr(), g.do, false)
		live := browserScroll(t, ln.Addr(), g.do, true)
		t.Logf("%s, no copyrect: %s", g.name, base)
		t.Logf("%s, copyrect:    %s", g.name, live)

		if live.copies == 0 {
			t.Fatalf("%s scroll produced no CopyRects, which is the whole point", g.name)
		}
		if live.rows*2 > base.rows {
			t.Fatalf("%s: CopyRect encoded %d rows against a %d row baseline, expected a large drop",
				g.name, live.rows, base.rows)
		}
		// and the picture has to still be right, which matters more than the bytes
		if live.mean > 8 || live.over*10000 > live.samples {
			t.Fatalf("%s: reconstruction drifted from a full refresh: mean %.2f worst %d, %d of %d samples over 64",
				g.name, live.mean, live.worst, live.over, live.samples)
		}
	}
}

// Chrome renders a <select> dropdown as a native OS window that no screencast
// frame ever contains, so a viewer clicking one used to see nothing at all.
// This drives the whole path a viewer does - RFB pointer events in, framebuffer
// updates out - and checks the list is really in the pixels the client got, and
// that clicking a row in it lands on the page.
//
// WRP_BROWSER_TEST=1 go test -run TestBrowserSelectPopup -v .
func TestBrowserSelectPopup(t *testing.T) {
	if os.Getenv("WRP_BROWSER_TEST") == "" {
		t.Skip("set WRP_BROWSER_TEST=1 to run, needs a browser")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, testSelectPage())
	}))
	defer srv.Close()

	defGeom = geom{w: 900, h: 700, c: 216}
	*headless = true
	if err := chromedpStart(); err != nil {
		t.Fatalf("start browser: %v", err)
	}
	defer brStop()
	selPopupInstall(brCtx())
	ctx, cncl := context.WithTimeout(brCtx(), 60*time.Second)
	defer cncl()
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL), chromedp.WaitReady("#s", chromedp.ByID)); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	vncInit()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	vncWorkers()
	vncServe(ln)
	vncCastStart()
	time.Sleep(2 * time.Second)

	p := dialProto(t, ln.Addr())
	p.setEncodings(rfbEncodingRaw)
	fb := image.NewRGBA(image.Rect(0, 0, p.w, p.h))
	p.fbur(false)
	p.apply(fb, p.readUpdate())
	// only ever look below the select: a focused one wears a blue ring of its
	// own, and the list is the only thing that can put colour under it
	sx, sy, below := testRect(t, ctx, "#s")
	below += vncChromeH + 4
	if n := testBlue(fb, below); n > 200 {
		t.Fatalf("%d highlight pixels before the select was ever clicked", n)
	}

	p.click(sx, sy+vncChromeH)
	p.settle(fb, 3*time.Second)
	if n := testBlue(fb, below); n < 500 {
		t.Fatalf("only %d highlight pixels after clicking the select: the dropdown never reached the client", n)
	}

	// and a click on the fourth row has to land on the page
	ox, oy, _ := testRect(t, ctx, "body>div>div:nth-child(4)")
	p.click(ox, oy+vncChromeH)
	p.settle(fb, 3*time.Second)
	var val, out string
	chromedp.Run(ctx, chromedp.Evaluate(`document.getElementById('s').value`, &val))
	chromedp.Run(ctx, chromedp.Evaluate(`document.getElementById('out').textContent`, &out))
	if val != "Books" {
		t.Fatalf("select value %q after picking the fourth row, want Books", val)
	}
	if out != "CHANGED TO: Books" {
		t.Fatalf("page change handler saw %q", out)
	}
	if n := testBlue(fb, below); n > 200 {
		t.Errorf("%d highlight pixels left after the dropdown was dismissed", n)
	}

	// WRP's own zoom is css zoom on the root, which scales every length the
	// script sets, so placement has to be checked at more than 100%
	for _, z := range []float64{1, 0.5, 1.5} {
		vncApplyZoom(z)
		time.Sleep(700 * time.Millisecond)
		zx, zy, _ := testRect(t, ctx, "#s")
		p.click(zx, zy+vncChromeH)
		time.Sleep(700 * time.Millisecond)
		var d []float64
		if err := chromedp.Run(ctx, chromedp.Evaluate(`(()=>{
			const s=document.querySelector('#s'),b=document.querySelector('body>div');
			if(!b)return[];const r=s.getBoundingClientRect(),g=b.getBoundingClientRect();
			return[g.left-r.left,g.top-r.bottom,g.width,r.width]})()`, &d)); err != nil || len(d) != 4 {
			t.Fatalf("zoom %v: no listbox in the dom: %v %v", z, err, d)
		}
		t.Logf("zoom %v: listbox offset %.1f,%.1f width %.1f vs select %.1f", z, d[0], d[1], d[2], d[3])
		if abs(int(d[0])) > 3 || abs(int(d[1])) > 3 {
			t.Errorf("zoom %v: listbox is %.1f,%.1f from where it belongs under the select", z, d[0], d[1])
		}
		if d[2] < d[3]-1 {
			t.Errorf("zoom %v: listbox %.1f narrower than the select %.1f", z, d[2], d[3])
		}
		chromedp.Run(ctx, chromedp.Evaluate(`document.body.dispatchEvent(
			new MouseEvent('mousedown',{bubbles:true}))`, nil)) // dismiss
	}
	vncApplyZoom(1)
}

// The same native popup problem breaks ISMAP mode, where the dropdown has the
// extra hazard that captureScreenshot stretches the viewport to the whole page
// between the click and the shot. Drives the real capture path and looks for
// the list in the image a browser would have been served.
//
// WRP_BROWSER_TEST=1 go test -run TestIsmapSelectPopup -v .
func TestIsmapSelectPopup(t *testing.T) {
	if os.Getenv("WRP_BROWSER_TEST") == "" {
		t.Skip("set WRP_BROWSER_TEST=1 to run, needs a browser")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, testSelectPage())
	}))
	defer srv.Close()

	defGeom = geom{w: 900, h: 700, c: 216}
	*headless = true
	*vncAddr = ""
	wrpCach.imgs = make(map[string]cachedImg)
	wrpCach.maps = make(map[string]cachedMap)
	if err := chromedpStart(); err != nil {
		t.Fatalf("start browser: %v", err)
	}
	defer brStop()
	selPopupInstall(brCtx())
	ctx, cncl := context.WithTimeout(brCtx(), 60*time.Second)
	defer cncl()
	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL), chromedp.WaitReady("#s", chromedp.ByID)); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	rq := &wrpReq{
		url: srv.URL, width: 900, height: 700, zoom: 1, jQual: 80, imgType: "png",
		proxy: true, w: httptest.NewRecorder(), r: httptest.NewRequest("GET", "/", nil),
	}
	sx, sy, below := testRect(t, ctx, "#s")
	if err := chromedp.Run(ctx, chromedp.MouseClickXY(float64(sx), float64(sy))); err != nil {
		t.Fatalf("click: %v", err)
	}
	rq.captureScreenshot()

	var buf bytes.Buffer
	for _, b := range wrpCach.imgs {
		buf = b.buf
	}
	if buf.Len() == 0 {
		t.Fatal("capture produced no image")
	}
	img, err := png.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	rgba := image.NewRGBA(img.Bounds())
	draw.Draw(rgba, img.Bounds(), img, img.Bounds().Min, draw.Src)
	if n := testBlue(rgba, below+4); n < 500 {
		t.Fatalf("only %d highlight pixels below the select in a %v capture: the dropdown is not in the image",
			n, img.Bounds().Size())
	}
}

// Viewport centre and bottom edge of the first element matching sel, in css pixels
func testRect(t *testing.T, ctx context.Context, sel string) (x, y, bottom int) {
	t.Helper()
	var r []float64
	js := fmt.Sprintf(`(()=>{const e=document.querySelector(%q);if(!e)return[];
		const r=e.getBoundingClientRect();return[r.x+r.width/2,r.y+r.height/2,r.bottom]})()`, sel)
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &r)); err != nil || len(r) != 3 {
		t.Fatalf("rect of %s: %v %v", sel, err, r)
	}
	return int(r[0]), int(r[1]), int(r[2])
}

// Pixels close to the listbox highlight, #1a73e8, after a round trip through jpeg
func testBlue(fb *image.RGBA, minY int) int {
	n := 0
	b := fb.Bounds()
	for y := max(b.Min.Y, minY); y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			o := fb.PixOffset(x, y)
			if abs(int(fb.Pix[o])-0x1a) < 40 && abs(int(fb.Pix[o+1])-0x73) < 40 && abs(int(fb.Pix[o+2])-0xe8) < 40 {
				n++
			}
		}
	}
	return n
}

// Pull updates until the picture stops changing, so the assertion sees the
// frame the viewer would be looking at rather than one mid repaint
func (p *protoConn) settle(fb *image.RGBA, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		p.fbur(true)
		rects, ok := p.readUpdateWithin(400 * time.Millisecond)
		if !ok {
			return
		}
		p.apply(fb, rects)
	}
}

func testSelectPage() string {
	return `<html><body style="font:16px sans-serif;padding:40px;background:#fff">
<select id=s style="font-size:20px;padding:6px">
<option>All Categories</option><option>Antiques</option><option>Art</option>
<option>Books</option><option>Cameras</option><option>Computers</option>
</select><p id=out>below the select</p>
<script>document.getElementById('s').addEventListener('change',function(e){
document.getElementById('out').textContent='CHANGED TO: '+e.target.value})</script>
</body></html>`
}

type scrollStats struct {
	copies, rows, frames, steps int
	mean                        float64
	worst, over, samples        int
}

func (s scrollStats) String() string {
	return fmt.Sprintf("%d steps, %d frames, %d CopyRects, %d rows encoded, vs refresh mean %.2f worst %d over64 %d",
		s.steps, s.frames, s.copies, s.rows, s.mean, s.worst, s.over)
}

// Scroll the page, tally what the server sends, then compare the framebuffer
// built from those updates against one clean full refresh
func browserScroll(t *testing.T, addr net.Addr, gesture func(*protoConn), copyRect bool) scrollStats {
	t.Helper()
	p := dialProto(t, addr)
	defer p.c.Close()
	encs := []int32{rfbEncodingRaw}
	if copyRect {
		encs = append(encs, rfbEncodingCopyRect)
	}
	p.setEncodings(encs...)
	scrollTop(t)
	time.Sleep(500 * time.Millisecond)

	p.fbur(false)
	fb := image.NewRGBA(image.Rect(0, 0, p.w, p.h))
	p.apply(fb, p.readUpdate())

	st := scrollStats{steps: 12}
	for i := 0; i < st.steps; i++ {
		gesture(p)
		for j := 0; j < 16; j++ { // chrome scrolls smoothly, one gesture is several frames
			p.fbur(true)
			rects, ok := p.readUpdateWithin(300 * time.Millisecond)
			if !ok {
				break
			}
			st.frames++
			for _, r := range rects {
				switch {
				case r.enc == rfbEncodingCopyRect:
					st.copies++
				case r.y >= vncChromeH:
					st.rows += r.h
				}
			}
			p.apply(fb, rects)
		}
	}

	p.c.SetDeadline(time.Now().Add(20 * time.Second))
	p.fbur(false)
	ref := image.NewRGBA(image.Rect(0, 0, p.w, p.h))
	p.apply(ref, p.readUpdate())
	st.mean, st.worst, st.over = testDiff(fb, ref, 64)
	st.samples = p.w * p.h * 3
	return st
}

// One wheel notch down, as a press and release of the wheel button
func (p *protoConn) wheel() {
	m := make([]byte, 6)
	m[0], m[1] = 5, 0x10
	binary.BigEndian.PutUint16(m[2:], 40)
	binary.BigEndian.PutUint16(m[4:], uint16(vncChromeH+300))
	p.c.Write(m)
	m[1] = 0
	p.c.Write(m)
}

func (p *protoConn) arrowDown() {
	m := make([]byte, 8)
	m[0], m[1] = 4, 1
	binary.BigEndian.PutUint32(m[4:], 0xff54) // XK_Down
	p.c.Write(m)
	m[1] = 0
	p.c.Write(m)
}

func scrollTop(t *testing.T) {
	t.Helper()
	if err := chromedp.Run(brCtx(), chromedp.Evaluate("window.scrollTo(0, 0)", nil)); err != nil {
		t.Fatalf("scroll to top: %v", err)
	}
}

// An update if one arrives in time. The server only sends when it has a new
// frame, so a quiet moment is normal rather than a failure.
func (p *protoConn) readUpdateWithin(d time.Duration) ([]protoRect, bool) {
	p.c.SetDeadline(time.Now().Add(d))
	defer p.c.SetDeadline(time.Now().Add(20 * time.Second))
	hdr := make([]byte, 1)
	if _, err := p.c.Read(hdr); err != nil {
		return nil, false
	}
	if hdr[0] != 0 {
		p.t.Fatalf("expected a framebuffer update, got server message %d", hdr[0])
	}
	rest := make([]byte, 3)
	p.read(rest)
	out := make([]protoRect, 0, binary.BigEndian.Uint16(rest[1:]))
	for i := 0; i < cap(out); i++ {
		out = append(out, p.readRect(i))
	}
	return out, true
}

// A page that looks like something somebody would read: paragraphs of real
// text, headings, tables and a couple of images
func testHTMLPage() string {
	var b strings.Builder
	b.WriteString(`<html><head><style>
body{font:15px/1.5 Georgia,serif;margin:0 auto;max-width:900px;color:#222;background:#fff}
h2{font:bold 24px Helvetica,sans-serif;border-bottom:2px solid #48c;margin-top:2em}
td{border:1px solid #bbb;padding:4px 8px;font-size:13px}
.img{height:180px;background:linear-gradient(135deg,#48c,#c84,#4c8);margin:1em 0}
a{color:#06c}</style></head><body>`)
	words := strings.Fields(`the quick brown fox jumps over a lazy dog while rendering
		proxies translate modern pages into pictures for machines that were retired
		before the web existed and still deserve to browse it today whatever their
		colour depth happens to be tonight`)
	for s := 0; s < 40; s++ {
		fmt.Fprintf(&b, "<h2>Section %d</h2>", s)
		for para := 0; para < 3; para++ {
			b.WriteString("<p>")
			for i := 0; i < 70; i++ {
				w := words[(s*37+para*11+i*7)%len(words)]
				if i%13 == 5 {
					fmt.Fprintf(&b, `<a href="#">%s</a> `, w)
					continue
				}
				fmt.Fprintf(&b, "%s ", w)
			}
			b.WriteString("</p>")
		}
		if s%3 == 0 {
			b.WriteString(`<div class="img"></div>`)
		}
		if s%4 == 1 {
			b.WriteString("<table>")
			for r := 0; r < 6; r++ {
				b.WriteString("<tr>")
				for c := 0; c < 6; c++ {
					fmt.Fprintf(&b, "<td>%d.%d.%d</td>", s, r, c)
				}
				b.WriteString("</tr>")
			}
			b.WriteString("</table>")
		}
	}
	b.WriteString(`<p id="end">end</p></body></html>`)
	return b.String()
}
