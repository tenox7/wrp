// The picture test, for a client that chooses its picture type by time
// (Archaic, see picfmt.go).
//
// Which type is on the screen first depends on the client's CPU and on
// its line, and the client knows neither. So at its start it asks for a
// test picture, once in each kind (the type that is fast to decode, then
// the small one), and times them:
//
//	GET /archaic-test?w=632&h=351&c=2p
//
// The answer is the top left w x h pixels of a sample page (testpage.png:
// text, links, a table, pictures; made from testpage.htm), in the
// colors c (as the C menu: 2p, 216n, 256d ...), sent as a slice is: in
// the type that the request's Accept asks for, with X-Fast-Length.
//
// The client tells WRP what it does, for the log, in a request header:
//
//	X-Archaic-Note: picture test: ABM 3.5 s, ABM3 2.2 s: ABM3 is faster here
//
// on a test or slice request, or alone on GET /archaic-test (no w). So the
// log shows when a test starts and how it ends.
package main

import (
	"bytes"
	_ "embed"
	"image"
	"image/draw"
	"image/png"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

//go:embed testpage.png
var picTestPNG []byte

var picTestPage struct {
	sync.Once
	img image.Image
	err error
}

const noteHeader = "X-Archaic-Note"

// logNote logs the client's note, if the request has one: only its
// printable ASCII, 200 characters at most
func logNote(r *http.Request) {
	n := r.Header.Get(noteHeader)
	if n == "" {
		return
	}
	b := make([]byte, 0, len(n))
	for i := 0; i < len(n) && len(b) < 200; i++ {
		if n[i] >= ' ' && n[i] < 127 {
			b = append(b, n[i])
		}
	}
	log.Printf("%s Archaic: %s\n", r.RemoteAddr, b)
}

func picTestServer(w http.ResponseWriter, r *http.Request) {
	st := time.Now()
	logNote(r)
	q := r.URL.Query()
	if !q.Has("w") { // a note alone
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok\n"))
		return
	}
	picTestPage.Do(func() { picTestPage.img, picTestPage.err = png.Decode(bytes.NewReader(picTestPNG)) })
	if picTestPage.err != nil {
		http.Error(w, "No test page: "+picTestPage.err.Error(), http.StatusInternalServerError)
		return
	}
	b := picTestPage.img.Bounds()
	wd, _ := strconv.Atoi(q.Get("w"))
	ht, _ := strconv.Atoi(q.Get("h"))
	wd, ht = min(max(wd, 16), b.Dx()), min(max(ht, 16), b.Dy())
	part := image.NewRGBA(image.Rect(0, 0, wd, ht))
	draw.Draw(part, part.Bounds(), picTestPage.img, b.Min, draw.Src)
	n, d := colorChoice(q.Get("c"), "")
	conv := newPageConv(part, n, d)
	i := conv.rows(0, ht)
	conv.close()
	what, err := sendSlice(w, r, i, "gif", int(*defJpgQual))
	if err != nil {
		http.Error(w, "Unable to encode the test picture: "+err.Error(), http.StatusInternalServerError)
		log.Printf("%s Picture test: %s\n", r.RemoteAddr, err)
		return
	}
	log.Printf("%s Picture test: %dx%d, %s: %s, %vms\n", r.RemoteAddr, wd, ht, colorVal(n, d), what, time.Since(st).Milliseconds())
}
