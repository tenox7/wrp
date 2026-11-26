package main

import (
	"bytes"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"
)

// A page like Akamai's bot check (see botCheck): a hidden box and a script
// that sets a cookie and reloads the page. It comes twice, as on the Daily
// Mail, then the real page, 3000 px tall. The picture must be the real page,
// not the empty check (40 px). A box that shows is no check to wait for.
//
// WRP_BROWSER_TEST=1 go test -run TestBotCheck -v .
func TestBotCheck(t *testing.T) {
	if os.Getenv("WRP_BROWSER_TEST") == "" {
		t.Skip("set WRP_BROWSER_TEST=1 to run, needs a browser")
	}
	var mux http.ServeMux
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n := 0
		if c, err := r.Cookie("n"); err == nil {
			n, _ = strconv.Atoi(c.Value)
		}
		if n < 2 {
			fmt.Fprintf(w, `<html><body><div id="sec-if-cpt-container" role="main" style="display: none">x</div>
				<script>setTimeout(() => { document.cookie = "n=%d"; location.reload(true) }, 500)</script></body></html>`, n+1)
			return
		}
		fmt.Fprint(w, `<body style="margin:0"><div style="height:3000px;background:#08f">the page</div></body>`)
	})
	mux.HandleFunc("/shown", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<body><div id="sec-if-cpt-container" style="display: block">hold the button</div></body>`)
	})
	srv := httptest.NewServer(&mux)
	defer srv.Close()

	defGeom = geom{w: 800, h: 600, c: 216}
	*headless = true
	*vncAddr = ""
	wrpCach.imgs = make(map[string]cachedImg)
	wrpCach.maps = make(map[string]cachedMap)
	if err := chromedpStart(); err != nil {
		t.Fatalf("start browser: %v", err)
	}
	defer brStop()
	req := func(u string) *wrpReq {
		return &wrpReq{
			url: u, width: 800, height: 0, zoom: 1, jQual: 80, imgType: "png",
			proxy: true, w: httptest.NewRecorder(), r: httptest.NewRequest("GET", "/", nil),
		}
	}

	rq := req(srv.URL + "/")
	rq.navigate()
	rq.captureScreenshot()
	var buf bytes.Buffer
	for _, b := range wrpCach.imgs {
		buf = b.buf
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Height < 3000 {
		t.Errorf("picture %dx%d: the check page, not the page after it", cfg.Width, cfg.Height)
	}

	st := time.Now()
	req(srv.URL + "/shown").navigate()
	if d := time.Since(st); d > 3*time.Second {
		t.Errorf("waited %v on a box that shows", d)
	}
}
