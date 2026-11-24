package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"text/template"
)

func TestKeysAnswer(t *testing.T) {
	for _, c := range []struct {
		f    focusInfo
		nav  bool
		want string
	}{
		{focusInfo{Kind: "none"}, false, "focus none\n"},
		{focusInfo{Kind: "none", Value: "x"}, true, "focus none\npage new\n"},
		{focusInfo{Kind: "text", Value: "a\tb\nc"}, false, "focus text\nvalue a b c\n"},
		{focusInfo{Kind: "text", Value: "Zürich €5"}, false, "focus text\nvalue Z\xfcrich ?5\n"},
		{focusInfo{Kind: "password", Value: "***"}, false, "focus password\nvalue ***\n"},
	} {
		if got := keysAnswer(c.f, c.nav); got != c.want {
			t.Errorf("keysAnswer(%+v, %v) = %q, want %q", c.f, c.nav, got, c.want)
		}
	}
	rq := wrpReq{url: "http://x/", keys: "pässwörd\r"}
	if s := fmt.Sprintf("%+v", rq.logged()); strings.Contains(s, "ss") || !strings.Contains(s, "(9 keys)") {
		t.Errorf("logged: %s", s)
	}
}

// Keys typed straight into a page, with no new picture: the box that has
// the keyboard, its text, a password as stars, Tab to the next box, and a
// key that starts a new page.
//
// WRP_BROWSER_TEST=1 go test -run TestKeysServer -v .
func TestKeysServer(t *testing.T) {
	if os.Getenv("WRP_BROWSER_TEST") == "" {
		t.Skip("set WRP_BROWSER_TEST=1 to run, needs a browser")
	}
	var mux http.ServeMux
	site := httptest.NewServer(&mux)
	defer site.Close()
	mux.HandleFunc("/box", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body><input id=q autofocus><input type=password id=p></body></html>`)
	})
	mux.HandleFunc("/text", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body><p>No box here.</p><script>
			document.onkeydown = e => { if (e.key === "g") location = "/box"; };
			</script></body></html>`)
	})

	oldTmpl := htmlTmpl
	defer func() { htmlTmpl = oldTmpl }()
	var err error
	if htmlTmpl, err = template.New("wrp.html").Parse(wrpTemplate("wrp.html")); err != nil {
		t.Fatal(err)
	}
	defGeom = geom{w: 640, h: 400, c: 2}
	*headless = true
	*vncAddr = ""
	if err := chromedpStart(); err != nil {
		t.Fatalf("start browser: %v", err)
	}
	defer brStop()

	form := func(page string) string {
		v := url.Values{"url": {site.URL + page}, "w": {"640"}, "h": {"400"},
			"m": {"ismap"}, "t": {"gif"}, "c": {"2"}, "sl": {"1"}}
		r := httptest.NewRequest("POST", "/", strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := httptest.NewRecorder()
		pageServer(rr, r)
		return rr.Body.String()
	}
	keys := func(k string) string {
		rr := httptest.NewRecorder()
		keysServer(rr, httptest.NewRequest("GET", "/keys?k="+url.QueryEscape(k), nil))
		if ct := rr.Header().Get("Content-Type"); ct != "text/plain" {
			t.Errorf("keys %q: Content-Type %q", k, ct)
		}
		return rr.Body.String()
	}

	if p := form("/box"); !strings.Contains(p, `NAME="kb" VALUE="text"`) {
		t.Errorf("the form of a page with an autofocus box: no kb text in\n%s", p)
	}
	for _, c := range []struct{ k, want string }{
		{"ab c", "focus text\nvalue ab c\n"},
		{"\b", "focus text\nvalue ab \n"},
		{"", "focus text\nvalue ab \n"},
		{"\t", "focus password\nvalue \n"},
		{"xyz", "focus password\nvalue ***\n"},
	} {
		if got := keys(c.k); got != c.want {
			t.Errorf("keys %q: got %q, want %q", c.k, got, c.want)
		}
	}
	if p := form("/text"); !strings.Contains(p, `NAME="kb" VALUE="none"`) {
		t.Errorf("the form of a page with no box: no kb none in\n%s", p)
	}
	if got, want := keys("x"), "focus none\n"; got != want {
		t.Errorf("keys x with no box: got %q, want %q", got, want)
	}
	if got, want := keys("g"), "focus text\nvalue \npage new\n"; got != want {
		t.Errorf("keys g, a shortcut to a new page: got %q, want %q", got, want)
	}
}
