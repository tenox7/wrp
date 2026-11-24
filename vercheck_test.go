package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"text/template"
)

func TestVersionNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"4.13.0", "4.12.0", true},
		{"4.12.0", "4.12.0", false},
		{"4.11.9", "4.12.0", false},
		{"4.10.0", "4.9.2", true},
		{"5.0", "4.12.0", true},
		{"4.12.0.1", "4.12.0", true},
		{"4.12", "4.12.0", false},
		{"v4.12.1", "4.12.0", true},
		{"", "4.12.0", false},
	} {
		if got := versionNewer(c.a, c.b); got != c.want {
			t.Errorf("versionNewer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// a fake GitHub: the log line, what the page shows, and the next check
func TestVersionCheck(t *testing.T) {
	tag, code := "", http.StatusOK
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code != http.StatusOK {
			http.Error(w, "no", code)
			return
		}
		fmt.Fprintf(w, `{"tag_name":%q,"html_url":"https://github.com/tenox7/wrp/releases/tag/%s"}`, tag, tag)
	}))
	defer gh.Close()
	old, oldTmpl := *verCheck, htmlTmpl
	defer func() {
		*verCheck, htmlTmpl = old, oldTmpl
		verNew.ver, verNew.page = "", ""
	}()
	*verCheck = gh.URL
	var err error
	if htmlTmpl, err = template.New("wrp.html").Parse(wrpTemplate("wrp.html")); err != nil {
		t.Fatal(err)
	}
	page := func() string {
		rr := httptest.NewRecorder()
		rq := wrpReq{r: httptest.NewRequest("GET", "/?t=gif&c=216", nil), w: rr}
		rq.parseForm()
		rq.printUI(uiParams{})
		return rr.Body.String()
	}
	for _, c := range []struct {
		tag   string
		code  int
		log   string
		retry bool
		show  string // the page shows WRP 99.1.0, with a link to this tag's page
	}{
		{"99.1.0", 200, "Update available: WRP 99.1.0 (this is " + version + "): https://github.com/tenox7/wrp/releases/tag/99.1.0", false, "99.1.0"},
		{"", 500, "Version check: HTTP 500 from ", true, "99.1.0"}, // an error: the page still shows it
		{version, 200, "Version check: WRP " + version + " is the newest", false, ""},
		{"v99.1.0", 200, "Update available: WRP 99.1.0 ", false, "v99.1.0"},
		{"1.0", 200, "Version check: WRP " + version + " is the newest", false, ""},
		{"99.1.0<b>", 200, "Version check: the newest release has an odd tag: ", true, ""},
	} {
		tag, code = c.tag, c.code
		msg, wait := verCheckOnce()
		if !strings.HasPrefix(msg, c.log) {
			t.Errorf("tag %q, HTTP %d: log %q, want %q...", c.tag, c.code, msg, c.log)
		}
		if retry := wait == verCheckRetry; retry != c.retry || (!retry && wait != verCheckEvery) {
			t.Errorf("tag %q, HTTP %d: next check in %v", c.tag, c.code, wait)
		}
		body := page()
		link := `<A HREF="/?url=` + url.QueryEscape("https://github.com/tenox7/wrp/releases/tag/"+c.show) + `&w=`
		if c.show == "" && strings.Contains(body, "Update available") {
			t.Errorf("tag %q, HTTP %d: the page shows an update", c.tag, c.code)
		}
		if c.show != "" && !(strings.Contains(body, `">Update available: WRP 99.1.0</A> |`) && strings.Contains(body, link)) {
			t.Errorf("tag %q, HTTP %d: the page does not show the update, with %s", c.tag, c.code, link)
		}
		if strings.Contains(body, "<b>") {
			t.Errorf("tag %q: on the page", c.tag)
		}
	}
}
