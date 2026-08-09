package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// The <select> replacement has to hold up on more than a plain select in the
// document flow: option groups, a list longer than the screen, a select the
// page has stretched invisibly under its own chrome, one near the bottom of the
// viewport, one inside a scaled or scrolled container, and one in an iframe.
// These are local pages so the cases stay exact and the test stays offline.
//
// WRP_BROWSER_TEST=1 go test -run TestSelectCases -v .
func TestSelectCases(t *testing.T) {
	if os.Getenv("WRP_BROWSER_TEST") == "" {
		t.Skip("set WRP_BROWSER_TEST=1 to run, needs a browser")
	}
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, casePage("plain"))
	}))
	defer inner.Close()
	var cases http.ServeMux
	srv := httptest.NewServer(&cases)
	defer srv.Close()
	cases.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "iframe-cross" {
			fmt.Fprintf(w, `<body style="margin:0;padding:30px;background:#fff">
				<iframe src=%q style="width:500px;height:300px;border:1px solid #888"></iframe></body>`, inner.URL)
			return
		}
		fmt.Fprint(w, casePage(name))
	})

	defGeom = geom{w: 1000, h: 700, c: 216}
	*headless = true
	*vncAddr = ""
	if err := chromedpStart(); err != nil {
		t.Fatalf("start browser: %v", err)
	}
	defer brStop()
	selPopupInstall(brCtx())

	for _, c := range []struct {
		name  string
		opts  int
		check func(*testing.T, caseResult)
	}{
		{"plain", 6, nil},
		{"optgroup", 7, func(t *testing.T, r caseResult) {
			if r.Rows != 7 {
				t.Errorf("optgroup labels should be rows too, got %d", r.Rows)
			}
		}},
		{"long", 300, func(t *testing.T, r caseResult) {
			if r.H > 700 {
				t.Errorf("list %dpx tall in a 700px viewport, should be capped", r.H)
			}
			if !r.Scrollable {
				t.Error("a capped list has to scroll or the tail is unreachable")
			}
		}},
		{"bottom", 12, func(t *testing.T, r caseResult) {
			if r.Dy > -10 {
				t.Errorf("select near the viewport floor should open upward, dy=%d", r.Dy)
			}
			if r.Top < 0 {
				t.Errorf("list starts off-screen at y=%d", r.Top)
			}
		}},
		{"overlay", 6, nil},  // opacity:0 select stretched under the page's own ui
		{"scaled", 6, nil},   // inside transform:scale(1.4)
		{"scrolled", 6, nil}, // inside an overflow:auto container, scrolled
		{"fixed", 6, nil},    // inside a position:fixed bar
		{"iframe", 6, nil},   // same-origin subframe
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cncl := context.WithTimeout(brCtx(), 40*time.Second)
			defer cncl()
			if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL+"/"+c.name),
				chromedp.WaitReady("body")); err != nil {
				t.Fatalf("navigate: %v", err)
			}
			time.Sleep(400 * time.Millisecond)
			r := runCase(t, ctx, c.name == "iframe")
			t.Logf("%-9s %3d opts | box %d rows %dx%d at %+d,%+d top=%d scroll=%v | picked %d -> %q",
				c.name, c.opts, r.Rows, r.W, r.H, r.Dx, r.Dy, r.Top, r.Scrollable, r.Pick, r.Value)
			if r.Rows != c.opts {
				t.Errorf("listbox has %d rows, want %d", r.Rows, c.opts)
			}
			if r.Dx < -3 || r.Dx > 3 {
				t.Errorf("listbox %dpx off horizontally", r.Dx)
			}
			if r.Value != r.Want {
				t.Errorf("picking row %d gave value %q, want %q", r.Pick, r.Value, r.Want)
			}
			if r.StillOpen {
				t.Error("listbox still open after picking")
			}
			if c.check != nil {
				c.check(t, r)
			}
		})
	}

	// cross origin subframe: js cannot reach into it, so judge it by the pixels
	t.Run("iframe-cross", func(t *testing.T) {
		ctx, cncl := context.WithTimeout(brCtx(), 40*time.Second)
		defer cncl()
		if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL+"/iframe-cross"),
			chromedp.WaitReady("iframe")); err != nil {
			t.Fatalf("navigate: %v", err)
		}
		time.Sleep(900 * time.Millisecond)
		var xy []float64
		// the subframe cannot be reached from js, so aim at where its select is:
		// 1px border, 30px body padding, ~22px tall select
		if err := chromedp.Run(ctx, chromedp.Evaluate(`(()=>{
			const r=document.querySelector('iframe').getBoundingClientRect();
			return[r.x+1+60,r.y+1+41]})()`, &xy)); err != nil {
			t.Fatalf("frame rect: %v", err)
		}
		before := shotBlue(t, ctx)
		clickAt(ctx, xy[0], xy[1])
		time.Sleep(800 * time.Millisecond)
		after := shotBlue(t, ctx)
		t.Logf("cross-origin iframe: highlight pixels %d -> %d", before, after)
		if before > 200 {
			t.Fatalf("%d highlight pixels before clicking", before)
		}
		if after < 400 {
			t.Errorf("only %d highlight pixels after clicking: no dropdown in the subframe", after)
		}
	})
}

type caseResult struct {
	Rows, Dx, Dy, W, H, Top, Pick int
	Scrollable, StillOpen         bool
	Value, Want                   string
}

func runCase(t *testing.T, ctx context.Context, inFrame bool) caseResult {
	t.Helper()
	scope := "window"
	if inFrame {
		scope = "frames[0]"
	}
	// a point where the select really is the topmost element
	var xy []float64
	find := `(()=>{const W=` + scope + `,D=W.document;
	 const s=D.getElementById('s');
	 s.scrollIntoView({block:'nearest'});
	 const r=s.getBoundingClientRect();
	 for(let x=r.left+1;x<r.right;x+=2)for(let y=r.top+1;y<r.bottom;y+=2){
	  const e=D.elementFromPoint(x,y);
	  if(e&&e.closest&&e.closest('select')===s)return[x+` + frameOff(inFrame, "x") +
		`,y+` + frameOff(inFrame, "y") + `]}
	 return[]})()`
	if err := chromedp.Run(ctx, chromedp.Evaluate(find, &xy)); err != nil || len(xy) != 2 {
		t.Fatalf("no reachable point on the select: %v %v", err, xy)
	}
	clickAt(ctx, xy[0], xy[1])
	time.Sleep(500 * time.Millisecond)

	var raw json.RawMessage
	probe := `(()=>{const W=` + scope + `;const b=W.__wrpSel&&W.__wrpSel.box();
	 if(!b)return{rows:-1};
	 const s=W.__wrpSel.sel(),r=s.getBoundingClientRect(),g=b.getBoundingClientRect();
	 return{rows:b.children.length,dx:Math.round(g.left-r.left),dy:Math.round(g.top-r.bottom),
	  w:Math.round(g.width),h:Math.round(g.height),top:Math.round(g.top),
	  scrollable:b.scrollHeight>b.clientHeight+1}})()`
	if err := chromedp.Run(ctx, chromedp.Evaluate(probe, &raw)); err != nil {
		t.Fatalf("probe: %v", err)
	}
	var out caseResult
	var p struct {
		Rows, Dx, Dy, W, H, Top int
		Scrollable              bool
	}
	json.Unmarshal(raw, &p)
	if p.Rows < 0 {
		t.Fatal("no listbox opened")
	}
	out = caseResult{Rows: p.Rows, Dx: p.Dx, Dy: p.Dy, W: p.W, H: p.H, Top: p.Top, Scrollable: p.Scrollable}

	// pick option 2 with a real click. Its row is not children[2] once group
	// labels are in the list, so ask the script which row belongs to it.
	out.Pick = 2
	var ry []float64
	row := `(()=>{const W=` + scope + `;const e=W.__wrpSel.row(2);
	 e.scrollIntoView({block:'nearest'});const r=e.getBoundingClientRect();
	 return[r.x+8+` + frameOff(inFrame, "x") + `,r.y+r.height/2+` + frameOff(inFrame, "y") + `]})()`
	if err := chromedp.Run(ctx, chromedp.Evaluate(row, &ry)); err != nil || len(ry) != 2 {
		t.Fatalf("row rect: %v", err)
	}
	clickAt(ctx, ry[0], ry[1])
	time.Sleep(500 * time.Millisecond)

	var res json.RawMessage
	chromedp.Run(ctx, chromedp.Evaluate(`(()=>{const W=`+scope+`,D=W.document;
	 const s=D.getElementById('s');
	 return{value:s.value,want:s.options[2].value||s.options[2].textContent,
	  stillOpen:!!(W.__wrpSel&&W.__wrpSel.box()),
	  echo:(D.getElementById('out')||{}).textContent||''}})()`, &res))
	var f struct {
		Value, Want, Echo string
		StillOpen         bool
	}
	json.Unmarshal(res, &f)
	out.Value, out.Want, out.StillOpen = f.Value, f.Want, f.StillOpen
	if f.Echo != "CHANGED:"+f.Value {
		t.Errorf("page change handler saw %q, expected CHANGED:%s", f.Echo, f.Value)
	}
	return out
}

func frameOff(inFrame bool, axis string) string {
	if !inFrame {
		return "0"
	}
	return "parent.document.querySelector('iframe').getBoundingClientRect()." + axis
}

func shotBlue(t *testing.T, ctx context.Context) int {
	t.Helper()
	fb := captureRGBA(t, ctx)
	return testBlue(fb, 0)
}

func casePage(name string) string {
	opts := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "<option value=v%d>Option number %d</option>", i, i)
		}
		return b.String()
	}
	sel := `<select id=s style="font-size:16px">` + opts(6) + `</select>`
	body, style := "", "body{font:16px sans-serif;margin:0;padding:30px;background:#fff}"
	switch name {
	case "optgroup":
		sel = `<select id=s style="font-size:16px">
			<optgroup label="Group A"><option value=a1>Alpha</option><option value=a2>Beta</option></optgroup>
			<optgroup label="Group B"><option value=b1>Gamma</option><option value=b2>Delta</option>
			<option value=b3>Epsilon</option></optgroup></select>`
	case "long":
		sel = `<select id=s style="font-size:16px">` + opts(300) + `</select>`
	case "bottom":
		body = `<div style="height:600px"></div>`
		sel = `<select id=s style="font-size:16px">` + opts(12) + `</select>`
	case "overlay":
		// the amazon pattern: an invisible select stretched over the page's own ui
		body = `<div style="position:relative;width:400px">
			<div style="position:absolute;inset:0;background:#eee;border:1px solid #999;
			 padding:6px;pointer-events:none">All &#9662;</div>`
		sel = `<select id=s style="position:relative;opacity:0;width:400px;height:34px;font-size:16px">` +
			opts(6) + `</select></div>`
	case "scaled":
		body = `<div style="transform:scale(1.4);transform-origin:0 0;padding:10px">`
		sel += `</div>`
	case "scrolled":
		body = `<div style="height:200px;overflow:auto;border:1px solid #999;width:400px"
			 id=sc><div style="height:120px"></div>`
		sel += `<div style="height:400px"></div></div>
			<script>document.getElementById('sc').scrollTop=60</script>`
	case "fixed":
		body = `<div style="position:fixed;top:0;left:0;right:0;height:50px;background:#ddd;padding:10px">`
		sel += `</div><div style="height:2000px"></div>`
	case "iframe":
		return `<body style="margin:0;padding:30px"><iframe src="/plain"
			style="width:500px;height:300px;border:1px solid #888"></iframe></body>`
	}
	return `<html><head><style>` + style + `</style></head><body>` + body + sel +
		`<p id=out>none</p>
		<script>document.getElementById('s').addEventListener('change',function(e){
		document.getElementById('out').textContent='CHANGED:'+e.target.value})</script>
		</body></html>`
}

func clickAt(ctx context.Context, x, y float64) {
	chromedp.Run(ctx, chromedp.ActionFunc(func(cx context.Context) error {
		input.DispatchMouseEvent(input.MouseMoved, x, y).Do(cx)
		input.DispatchMouseEvent(input.MousePressed, x, y).WithButton(input.Left).WithClickCount(1).Do(cx)
		return input.DispatchMouseEvent(input.MouseReleased, x, y).WithButton(input.Left).WithClickCount(1).Do(cx)
	}))
}

func captureRGBA(t *testing.T, ctx context.Context) *image.RGBA {
	t.Helper()
	var b []byte
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(cx context.Context) error {
		var e error
		b, e = page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatPng).Do(cx)
		return e
	})); err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := image.NewRGBA(img.Bounds())
	draw.Draw(out, img.Bounds(), img, img.Bounds().Min, draw.Src)
	return out
}
