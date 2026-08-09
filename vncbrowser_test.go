package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"image"
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
