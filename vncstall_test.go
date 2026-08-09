package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// The VNC input path is one goroutine feeding cdp calls to chrome in order, so
// anything that never comes back takes the whole session with it: the
// screencast keeps painting, tabs keep switching, and every click and url the
// viewer enters after that goes nowhere. These are the three ways we found to
// get there. Off by default, they need a browser:
// WRP_BROWSER_TEST=1 go test -run TestBrowserStall -v .
func stallBrowser(t *testing.T) {
	t.Helper()
	if os.Getenv("WRP_BROWSER_TEST") == "" {
		t.Skip("set WRP_BROWSER_TEST=1 to run, needs a browser")
	}
	defGeom = geom{w: 800, h: 600, c: 216}
	*headless = true
	if err := chromedpStart(); err != nil {
		t.Fatalf("start browser: %v", err)
	}
	t.Cleanup(brStop)
}

// A page whose subresource never finishes: chrome renders it, but the load
// event it is waiting on is never coming.
func stallServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/hang", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body>text<img src="/drip"></body></html>`)
	})
	mux.HandleFunc("/drip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 60; i++ { // trickle, so chrome never times the request out
			w.Write([]byte{0})
			w.(http.Flusher).Flush()
			time.Sleep(500 * time.Millisecond)
		}
	})
	mux.HandleFunc("/alert", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body style="margin:0"><div style="width:700px;height:500px"
			onclick="alert('boom')">click me</div></body></html>`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// Navigating must not wait for a load event that never fires
func TestBrowserStallNavigate(t *testing.T) {
	stallBrowser(t)
	srv := stallServer(t)

	st := time.Now()
	if err := vncRun(brCtx(), vncActTO, vncNav(srv.URL+"/hang")); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if d := time.Since(st); d > 5*time.Second {
		t.Fatalf("navigate took %v, it waited for a load event that never comes", d)
	}

	// and the tab still takes input afterwards, which is the point
	var ok string
	if err := vncRun(brCtx(), vncActTO, chromedp.Evaluate(`"alive"`, &ok)); err != nil {
		t.Fatalf("browser stopped answering after the navigation: %v", err)
	}
	if ok != "alive" {
		t.Fatalf("got %q", ok)
	}
}

// A dialog blocks its renderer until somebody answers it. Nothing in a
// headless session ever would, so every later input dispatch to that tab hung.
func TestBrowserStallDialog(t *testing.T) {
	stallBrowser(t)
	srv := stallServer(t)

	vncInit()
	if err := vncRun(brCtx(), vncTabTO, vncNav(srv.URL+"/alert")); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	vncCastListen() // the dialog handler rides along with the screencast listener
	time.Sleep(500 * time.Millisecond)

	click := chromedp.ActionFunc(func(cx context.Context) error {
		if err := input.DispatchMouseEvent(input.MousePressed, 100, 100).
			WithButton(input.Left).WithClickCount(1).Do(cx); err != nil {
			return err
		}
		return input.DispatchMouseEvent(input.MouseReleased, 100, 100).
			WithButton(input.Left).WithClickCount(1).Do(cx)
	})
	if err := vncRun(brCtx(), vncActTO, click); err != nil {
		t.Fatalf("click never came back, the dialog was not answered: %v", err)
	}

	var ok string
	if err := vncRun(brCtx(), vncActTO, chromedp.Evaluate(`"alive"`, &ok)); err != nil {
		t.Fatalf("tab still blocked after the dialog: %v", err)
	}
	if ok != "alive" {
		t.Fatalf("got %q", ok)
	}
}

// The whole thing over the wire, the way the session died in practice: open a
// page that never finishes loading by typing it into the nav bar, then click.
// The click has to reach the page. Runs the input worker, so give it the
// server to itself:
// WRP_BROWSER_TEST=1 go test -run TestBrowserStallInput -v .
func TestBrowserStallInput(t *testing.T) {
	stallBrowser(t)
	srv := stallServer(t)

	vncInit()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	vncWorkers()
	vncServe(ln)
	vncCastStart()

	p := dialProto(t, ln.Addr())
	p.setEncodings(rfbEncodingRaw)

	p.click(vncURLX+20, vncTabH+10) // the url field
	for _, r := range srv.URL + "/hang" {
		p.key(uint32(r))
	}
	p.key(0xFF0D) // enter

	// the page paints as soon as chrome has it, load event or not
	if !waitFor(10*time.Second, func() bool {
		var u string
		vncRun(brCtx(), vncActTO, chromedp.Evaluate(`location.pathname`, &u))
		return u == "/hang"
	}) {
		t.Fatal("the url typed into the nav bar never opened")
	}

	// now the part that used to be dead: everything queued behind that
	// navigation, so no click ever reached the page again
	vncRun(brCtx(), vncActTO, chromedp.Evaluate(
		`document.body.onclick = () => document.title = "clicked"`, nil))
	p.click(200, vncChromeH+200)
	if !waitFor(15*time.Second, func() bool {
		var title string
		vncRun(brCtx(), vncActTO, chromedp.Evaluate(`document.title`, &title))
		return title == "clicked"
	}) {
		t.Fatal("the click never reached the page, the input worker is wedged")
	}
}

func (p *protoConn) click(x, y int) {
	m := make([]byte, 6)
	m[0], m[1] = 5, 1
	binary.BigEndian.PutUint16(m[2:], uint16(x))
	binary.BigEndian.PutUint16(m[4:], uint16(y))
	p.c.Write(m)
	m[1] = 0
	p.c.Write(m)
	time.Sleep(50 * time.Millisecond)
}

func (p *protoConn) key(ks uint32) {
	m := make([]byte, 8)
	m[0], m[1] = 4, 1
	binary.BigEndian.PutUint32(m[4:], ks)
	p.c.Write(m)
	m[1] = 0
	p.c.Write(m)
	time.Sleep(10 * time.Millisecond)
}

func waitFor(d time.Duration, f func() bool) bool {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(250 * time.Millisecond) {
		if f() {
			return true
		}
	}
	return false
}

// A script that will not give the thread back must not take the session with
// it: the call gives up and the next input still gets through.
func TestBrowserStallScript(t *testing.T) {
	stallBrowser(t)

	if err := vncRun(brCtx(), vncTabTO, vncNav("about:blank")); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	st := time.Now()
	err := vncRun(brCtx(), vncActTO, chromedp.Evaluate(
		`var t=Date.now();while(Date.now()-t<600000);`, nil))
	if err == nil {
		t.Fatal("expected the runaway script to be given up on")
	}
	d := time.Since(st)
	if d > vncActTO+5*time.Second {
		t.Fatalf("gave up after %v, should be around %v", d, vncActTO)
	}
	t.Logf("runaway script abandoned after %v: %v", d.Truncate(time.Second), err)
}
