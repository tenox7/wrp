package main

import (
	"bytes"
	"log"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The picture test: the sample page as a slice, in the type and colors asked
// for, and the client's notes in the log
func TestPicTestServer(t *testing.T) {
	var lg bytes.Buffer
	log.SetOutput(&lg)
	defer log.SetOutput(os.Stderr)
	get := func(u, accept, note string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		rq := httptest.NewRequest("GET", u, nil)
		if accept != "" {
			rq.Header.Set("Accept", accept)
		}
		if note != "" {
			rq.Header.Set(noteHeader, note)
		}
		picTestServer(rr, rq)
		return rr
	}
	for _, c := range []struct {
		u, accept, typ, magic string
		fast                  bool
	}{
		{"/archaic-test?w=632&h=351&c=2p", "image/x-abm, image/x-abm3;q=0.5, image/gif", "image/x-abm", "ABM1", false},
		{"/archaic-test?w=632&h=351&c=2p", "image/x-abm, image/x-abm3, image/gif", "image/x-abm3", "ABM3", true},
		{"/archaic-test?w=632&h=351&c=216n", "image/x-abm, image/x-ab8, image/gif;q=0.5", "image/x-ab8", "AB81", false},
		{"/archaic-test?w=632&h=351&c=256d", "", "image/gif", "GIF8", false},
		{"/archaic-test?w=99999&h=3&c=2", "image/x-abm", "image/x-abm", "ABM1", false},
	} {
		rr := get(c.u, c.accept, "")
		b := rr.Body.Bytes()
		if rr.Code != 200 || rr.Header().Get("Content-Type") != c.typ || len(b) < 16 || string(b[:4]) != c.magic {
			t.Errorf("%s, Accept %q: status %d, %s, %d bytes", c.u, c.accept, rr.Code, rr.Header().Get("Content-Type"), len(b))
			continue
		}
		if (rr.Header().Get(fastLenHeader) != "") != c.fast {
			t.Errorf("%s, Accept %q: %s %q", c.u, c.accept, fastLenHeader, rr.Header().Get(fastLenHeader))
		}
		if n, _ := strconv.Atoi(rr.Header().Get("Content-Length")); n != len(b) {
			t.Errorf("%s: Content-Length %d, %d bytes", c.u, n, len(b))
		}
		if c.magic == "ABM1" || c.magic == "ABM3" { // the size asked for, or the page's
			w, h := int(b[4])<<8|int(b[5]), int(b[6])<<8|int(b[7])
			if strings.Contains(c.u, "w=632") && (w != 632 || h != 351) || strings.Contains(c.u, "w=99999") && (w != 1280 || h != 16) {
				t.Errorf("%s: a picture of %dx%d", c.u, w, h)
			}
		}
	}
	// the sample page is a page: ABM3 is much smaller than ABM, as on the web
	a, b := get("/archaic-test?w=632&h=351&c=2p", "image/x-abm", "").Body.Len(), get("/archaic-test?w=632&h=351&c=2p", "image/x-abm3", "").Body.Len()
	if a < 3000 || a > 20000 || b*100 > a*80 {
		t.Errorf("the sample page in 2p: ABM %d bytes, ABM3 %d: want 3 to 20 KB, and ABM3 20%% less or better", a, b)
	}
	// notes: in the log, with a picture or alone; only printable ASCII
	lg.Reset()
	get("/archaic-test?w=64&h=32&c=2", "image/x-abm", "picture test starts")
	if rr := get("/archaic-test", "", "the result:\r\nFAKE line \x01\xff"+strings.Repeat("x", 300)); rr.Code != 200 || rr.Body.String() != "ok\n" {
		t.Errorf("a note alone: status %d, %q", rr.Code, rr.Body.String())
	}
	out := lg.String()
	if !strings.Contains(out, " Archaic: picture test starts\n") || !strings.Contains(out, " Picture test: 64x32, 2p: abm, ") ||
		!strings.Contains(out, "client takes abm") || !strings.Contains(out, " Archaic: the result:FAKE line xxx") || strings.Count(out, "\n") != 3 {
		t.Errorf("the log:\n%s", out)
	}
	if n := strings.Count(out, "x"); n > 230 {
		t.Errorf("a long note is not cut: %d x", n)
	}
}

func TestAcceptOwn(t *testing.T) {
	for accept, want := range map[string]string{
		"":                                  "",
		"text/html, image/gif":              "",
		"text/html, image/x-abm, image/gif": "abm gif",
		"text/html, image/x-abm, image/x-abm3;q=0.5, image/x-ab8, image/gif;q=0.5": "abm abm3;q=0.5 ab8 gif;q=0.5",
	} {
		rq := httptest.NewRequest("GET", "/", nil)
		rq.Header.Set("Accept", accept)
		if got := acceptOwn(rq); got != want {
			t.Errorf("Accept %q: %q, want %q", accept, got, want)
		}
	}
}
