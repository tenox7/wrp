package main

// Keys straight from the client's window, as the user types them: Archaic
// sends each group of keys to /keys, and WRP types them into the page with
// no new picture. Return goes with the form (k=), for the new picture.
//
//	GET /keys?k=KEYS
//
// KEYS are runes, as for k= in the form: \b is Backspace, \t Tab, and so
// on (chromedp's kb runes). The answer is plain text, a line each, in
// Latin-1 (? for the other letters):
//
//	focus KIND	what has the keyboard on the page now: text (a box
//			for text), password, or none (no box: Archaic's
//			Space and b then scroll its own frame)
//	value TEXT	the box's text, its last 200 letters (a password as
//			stars). Not with focus none
//	page new	the keys started a new page (a shortcut, a link):
//			the client asks for its picture
//
// WRP's form tells the client the same (focus) in its hidden field kb, and
// that this WRP takes keys at /keys. The keys are not logged: they may be
// a password.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// What has the keyboard: the active element, also inside shadow roots and
// frames of the same site. A frame of another site can't be looked into:
// it has the keyboard, so it is taken as a box.
const focusJS = `(() => {
	let e = document.activeElement;
	for (let i = 0; e && i < 20; ++i) {
		if (e.shadowRoot && e.shadowRoot.activeElement) {
			e = e.shadowRoot.activeElement;
			continue;
		}
		if (e.tagName === "IFRAME" || e.tagName === "FRAME") {
			let d = null;
			try { d = e.contentDocument; } catch (x) {}
			if (!d)
				return {kind: "text", value: ""};
			if (!d.activeElement || d.activeElement === d.body)
				return {kind: "none", value: ""};
			e = d.activeElement;
			continue;
		}
		break;
	}
	if (!e)
		return {kind: "none", value: ""};
	let k = "none", v = "";
	if (e.tagName === "TEXTAREA") {
		k = "text";
		v = e.value;
	} else if (e.tagName === "INPUT") {
		const t = (e.type || "text").toLowerCase();
		if (t === "password") {
			k = "password";
			v = "*".repeat(Math.min(e.value.length, 200));
		} else if (["text", "search", "email", "url", "tel", "number"].includes(t)) {
			k = "text";
			v = e.value;
		}
	} else if (e.isContentEditable) {
		k = "text";
		v = e.innerText;
	}
	v = String(v || "").replace(/\s+/g, " ");
	return {kind: k, value: v.length > 200 ? v.slice(-200) : v};
})()`

type focusInfo struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// focusNow: what has the keyboard on the page now (focusJS). An error, or
// a page that is not there: none.
func focusNow(ctx context.Context) focusInfo {
	var f focusInfo
	if err := chromedp.Evaluate(focusJS, &f).Do(ctx); err != nil {
		return focusInfo{Kind: "none"}
	}
	switch f.Kind {
	case "text", "password":
	default:
		f = focusInfo{Kind: "none"}
	}
	return f
}

// the tab's document: a new one after a new page (not a change of the
// address within the page)
func loaderID(ctx context.Context) cdp.LoaderID {
	t, err := page.GetFrameTree().Do(ctx)
	if err != nil || t.Frame == nil {
		return ""
	}
	return t.Frame.LoaderID
}

// s in Latin-1 on one line: ? for the letters it has not, a space for a
// line break or tab, no other control characters
func latin1(s string) string {
	b := make([]byte, 0, len(s))
	for _, c := range s {
		switch {
		case c == '\n' || c == '\r' || c == '\t':
			b = append(b, ' ')
		case c < ' ' || (c >= 0x7f && c < 0xa0):
		case c < 0x100:
			b = append(b, byte(c))
		default:
			b = append(b, '?')
		}
	}
	return string(b)
}

func keysAnswer(f focusInfo, newPage bool) string {
	var s strings.Builder
	fmt.Fprintf(&s, "focus %s\n", f.Kind)
	if f.Kind != "none" {
		fmt.Fprintf(&s, "value %s\n", latin1(f.Value))
	}
	if newPage {
		s.WriteString("page new\n")
	}
	return s.String()
}

func keysServer(w http.ResponseWriter, r *http.Request) {
	k := r.FormValue("k")
	log.Printf("%s Keys to the page: %d\n", r.RemoteAddr, utf8.RuneCountInString(k))
	browserMu.Lock()
	defer browserMu.Unlock()
	var f focusInfo
	var l0, l1 cdp.LoaderID
	acts := []chromedp.Action{
		chromedp.ActionFunc(func(ctx context.Context) error {
			l0 = loaderID(ctx)
			return nil
		}),
	}
	if k != "" {
		acts = append(acts, waitNav(chromedp.KeyEvent(k)))
	}
	acts = append(acts, chromedp.ActionFunc(func(ctx context.Context) error {
		l1 = loaderID(ctx)
		f = focusNow(ctx)
		return nil
	}))
	if err := chromedp.Run(brCtx(), acts...); err != nil {
		log.Printf("%s Keys: %v\n", r.RemoteAddr, err)
		f = focusInfo{Kind: "none"}
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "max-age=0")
	w.Header().Set("Expires", "-1")
	w.Header().Set("Pragma", "no-cache")
	fmt.Fprint(w, keysAnswer(f, l0 != l1))
}

// the request, for the log: the keys only counted, as they may be a
// password
func (rq *wrpReq) logged() wrpReq {
	c := *rq
	if c.keys != "" {
		c.keys = fmt.Sprintf("(%d keys)", utf8.RuneCountInString(c.keys))
	}
	return c
}
