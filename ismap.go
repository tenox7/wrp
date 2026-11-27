// WRP ISMAP / ChromeDP routines
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/color/palette"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/lithammer/shortuuid/v4"
	"github.com/tenox7/gip"
)

type cachedImg struct {
	buf bytes.Buffer
	// Slice mode (sl=1): the whole page, kept decoded and encoded in slices
	// on request. A GIF page is made its colors band by band, as the slices
	// need it (conv, see pageconv.go).
	full image.Image
	conv *pageConv
	typ  string
	jq   int
}

type cachedMap struct {
	req wrpReq
}

type wrpCache struct {
	sync.Mutex
	imgs map[string]cachedImg
	maps map[string]cachedMap
}

func (c *wrpCache) addImg(path string, buf bytes.Buffer) {
	c.Lock()
	defer c.Unlock()
	c.imgs[path] = cachedImg{buf: buf}
}

func (c *wrpCache) addFull(path string, full image.Image, typ string, jq int) {
	c.Lock()
	defer c.Unlock()
	c.imgs[path] = cachedImg{full: full, typ: typ, jq: jq}
}

func (c *wrpCache) addConv(path string, conv *pageConv, jq int) {
	c.Lock()
	defer c.Unlock()
	c.imgs[path] = cachedImg{conv: conv, typ: "gif", jq: jq}
}

func (c *wrpCache) getFull(path string) (cachedImg, bool) {
	c.Lock()
	defer c.Unlock()
	e, ok := c.imgs[path]
	if !ok || (e.full == nil && e.conv == nil) {
		return cachedImg{}, false
	}
	return e, true
}

func (c *wrpCache) getImg(path string) (bytes.Buffer, bool) {
	c.Lock()
	defer c.Unlock()
	e, ok := c.imgs[path]
	if !ok {
		return bytes.Buffer{}, false
	}
	return e.buf, true
}

func (c *wrpCache) addMap(path string, req wrpReq) {
	c.Lock()
	defer c.Unlock()
	c.maps[path] = cachedMap{req: req}
}

func (c *wrpCache) getMap(path string) (wrpReq, bool) {
	c.Lock()
	defer c.Unlock()
	e, ok := c.maps[path]
	if !ok {
		return wrpReq{}, false
	}
	return e.req, true
}

// stopConvs stops the backgrounds of the kept pages (see pageconv.go), so that
// the browser has the CPUs for the next page. Their slices still come.
func (c *wrpCache) stopConvs() {
	c.Lock()
	defer c.Unlock()
	for _, e := range c.imgs {
		if e.conv != nil {
			e.conv.close()
		}
	}
}

func (c *wrpCache) clear() {
	c.stopConvs()
	c.Lock()
	defer c.Unlock()
	c.imgs = make(map[string]cachedImg)
	c.maps = make(map[string]cachedMap)
}

func chromedpStart() error {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", *headless),
		chromedp.Flag("hide-scrollbars", false),
		chromedp.Flag("enable-automation", false),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		// Chrome draws only as much of the page as its tile memory holds
		// (512 MB). A whole tall page in one picture (slices, or -height 0)
		// needs more: virtuallyfun.com, 75000 px tall, came back black
		// below the part that fit, or all black but its fixed background.
		chromedp.Flag("force-gpu-mem-available-mb", "4096"),
	)
	if *browserPath == "" {
		*browserPath = findBrowser()
	}
	if *browserPath != "" {
		log.Printf("Using browser: %s", *browserPath)
		opts = append(opts, chromedp.ExecPath(*browserPath))
	} else {
		log.Printf("No browser detected, falling back to chromedp default lookup")
	}
	if *userDataDir != "" {
		opts = append(opts, chromedp.UserDataDir(*userDataDir))
	}
	actx, acncl = chromedp.NewExecAllocator(context.Background(), opts...)
	setBrRoot(chromedp.NewContext(actx, chromedp.WithErrorf(cdpErrorf)))
	// Start the browser here rather than on the first request that needs it.
	// A context whose browser failed to start cannot be used again: chromedp
	// allocates once per context and closes the same channel on the way out,
	// so the second attempt panics inside the allocator. There is nothing to
	// limp on with either, so say what went wrong and stop.
	if err := chromedp.Run(brCtx()); err != nil {
		brStop() // whatever did come up must not be left holding the profile
		if strings.Contains(err.Error(), "SingletonLock") {
			return fmt.Errorf("%v is in use by another browser, close it or use a different -profile", *userDataDir)
		}
		return err
	}
	uaInit(brCtx())
	uaApply(brCtx())
	return nil
}

// cdpErrorf logs chromedp's errors, as chromedp does, but not the CDP events
// it does not know. The browser is newer than chromedp v0.15.1, and chromedp
// only drops such an event: it is no error. One was "unhandled node event
// *dom.EventAdoptedStyleSheetsModified", from Akamai's bot check (see
// botCheck). The log names chromedp's line, not this one.
func cdpErrorf(format string, v ...any) {
	if strings.HasPrefix(format, "unhandled ") {
		return
	}
	log.Output(2, fmt.Sprintf("ERROR: "+format, v...))
}

// Determine what action to take
func (rq *wrpReq) action() chromedp.Action {
	// Mouse Click
	if rq.mouseX > 0 && rq.mouseY > 0 {
		log.Printf("%s Mouse Click %d,%d\n", rq.r.RemoteAddr, rq.mouseX, rq.mouseY)
		x, y := float64(rq.mouseX)/rq.zoom, float64(rq.mouseY)/rq.zoom
		if !rq.full() {
			return waitNav(chromedp.MouseClickXY(x, y))
		}
		// a whole page is in a viewport as tall as the screen (fullHeight)
		return waitNav(chromedp.ActionFunc(func(ctx context.Context) error {
			var top float64
			if err := wrpEval(ctx, fmt.Sprintf(clickJS, y), &top); err != nil {
				log.Printf("%s Not scrolled to the click: %v\n", rq.r.RemoteAddr, err)
			}
			return chromedp.MouseClickXY(x, y-top).Do(ctx)
		}))
	}
	// Buttons
	if len(rq.buttons) > 0 {
		log.Printf("%s Button %v\n", rq.r.RemoteAddr, rq.buttons)
		switch rq.buttons {
		case "Bk":
			return navigateBack()
		case "St":
			return chromedp.Stop()
		case "Re":
			return chromedp.Reload()
		case "Bs":
			return waitNav(chromedp.KeyEvent("\b"))
		case "Rt":
			return waitNav(chromedp.KeyEvent("\r"))
		case "<":
			return chromedp.KeyEvent("\u0302")
		case "^":
			return chromedp.KeyEvent("\u0304")
		case "v":
			return chromedp.KeyEvent("\u0301")
		case ">":
			return chromedp.KeyEvent("\u0303")
		case "Up":
			return chromedp.KeyEvent("\u0308")
		case "Dn":
			return chromedp.KeyEvent("\u0307")
		case "All": // Select all
			return chromedp.KeyEvent("a", chromedp.KeyModifiers(input.ModifierCtrl))
		}
	}
	// Keys
	if len(rq.keys) > 0 {
		log.Printf("%s Sending Keys: %d\n", rq.r.RemoteAddr, utf8.RuneCountInString(rq.keys)) // not the keys: they may be a password
		return waitNav(chromedp.KeyEvent(rq.keys))
	}
	// Navigate to URL
	log.Printf("%s Processing Navigate Request for %s\n", rq.r.RemoteAddr, rq.url)
	return chromedp.Navigate(rq.url)
}

// waitNav runs a (a click, keys), and when that starts a new page in the tab
// (a link, Return in a form), waits until the new page has loaded, at most
// *delay. The capture measures the page first: a page that had not come yet
// measured 10 px, and the picture was a thin strip. Some pages change their
// own address first and then go to the link (DuckDuckGo's results): a change
// within the page ends the wait only when no new page follows it within a
// quarter of a second. A click on the page itself costs a quarter of a second
// too: that long a new page may take to be asked for.
func waitNav(a chromedp.Action) chromedp.ActionFunc {
	return func(ctx context.Context) error {
		tree, err := page.GetFrameTree().Do(ctx)
		if err != nil {
			return a.Do(ctx)
		}
		top, cur := tree.Frame.ID, tree.Frame.URL // the URL without its #fragment
		const (
			navStart = iota + 1 // something began: maybe a new page, maybe not
			navDoc              // a new page (document) began in the tab
			navQuiet            // a change within the page ended, or a load ended
			navLoad             // the page's load event
		)
		ev := make(chan int, 64)
		lctx, lcancel := context.WithCancel(ctx)
		defer lcancel()
		chromedp.ListenTarget(lctx, func(e interface{}) {
			n := 0
			switch e := e.(type) {
			case *page.EventFrameRequestedNavigation:
				if e.FrameID == top {
					n = navStart
					u, _, _ := strings.Cut(e.URL, "#")
					if u != cur && (e.Disposition == "" || e.Disposition == page.ClientNavigationDispositionCurrentTab) {
						n = navDoc
					}
				}
			case *page.EventFrameStartedNavigating:
				if e.FrameID == top {
					n = navDoc
					if e.NavigationType == page.FrameStartedNavigatingNavigationTypeSameDocument ||
						e.NavigationType == page.FrameStartedNavigatingNavigationTypeHistorySameDocument {
						n = navStart
					}
				}
			case *page.EventFrameStartedLoading:
				if e.FrameID == top {
					n = navStart
				}
			case *page.EventFrameNavigated:
				if e.Frame.ParentID == "" {
					n = navDoc
				}
			case *page.EventNavigatedWithinDocument:
				if e.FrameID == top {
					n = navQuiet
				}
			case *page.EventFrameStoppedLoading:
				if e.FrameID == top {
					n = navQuiet
				}
			case *page.EventLoadEventFired:
				n = navLoad
			}
			if n != 0 {
				select {
				case ev <- n:
				default:
				}
			}
		})
		if err := a.Do(ctx); err != nil {
			return err
		}
		first := time.After(250 * time.Millisecond)
		end := time.After(*delay)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		var grace <-chan time.Time
		started, doc := false, false
		for {
			select {
			case n := <-ev:
				started = true
				switch n {
				case navDoc:
					doc, grace = true, nil
				case navQuiet:
					if !doc {
						grace = time.After(250 * time.Millisecond)
					}
				case navLoad:
					if doc {
						return nil
					}
				}
			case <-first:
				if !started {
					return nil // the same page: the capture waits for it to render
				}
			case <-grace:
				return nil // a change within the page, and no new page after it
			case <-end:
				return nil
			case <-ctx.Done():
				return nil
			case <-tick.C:
				dlTrack.Lock()
				dl := dlTrack.ev != nil
				dlTrack.Unlock()
				if dl {
					return nil // a download, not a page: navigate() waits for it
				}
			}
		}
	}
}

// Go one page back in the tab's history. chromedp.NavigateBack waits for a
// load event, and Chrome sends none when it takes the page from its
// back-forward cache: the request never ended, and as it held browserMu,
// nothing else could use the browser either. This waits only until the page
// is there, at most 10 seconds; captureScreenshot then waits for the render.
func navigateBack() chromedp.ActionFunc {
	return func(ctx context.Context) error {
		cur, entries, err := page.GetNavigationHistory().Do(ctx)
		if err != nil {
			return err
		}
		if cur <= 0 || cur >= int64(len(entries)) {
			return nil // nothing to go back to
		}
		tree, err := page.GetFrameTree().Do(ctx)
		if err != nil {
			return err
		}
		done := make(chan struct{}, 1)
		lctx, lcancel := context.WithTimeout(ctx, 10*time.Second)
		defer lcancel()
		chromedp.ListenTarget(lctx, func(ev interface{}) {
			switch e := ev.(type) {
			case *page.EventFrameNavigated: // also for a page from the cache
				if e.Frame.ParentID != "" {
					return
				}
			case *page.EventNavigatedWithinDocument: // back to an #anchor or pushState
				if e.FrameID != tree.Frame.ID {
					return
				}
			default:
				return
			}
			select {
			case done <- struct{}{}:
			default:
			}
		})
		if err := page.NavigateToHistoryEntry(entries[cur-1].ID).Do(ctx); err != nil {
			return err
		}
		select {
		case <-done:
		case <-lctx.Done():
		}
		return nil
	}
}

// botCheck tells what the page is: "check" for Akamai's bot check (Bot
// Manager, on the Daily Mail and others), "loading" before the page's load
// event, "page" after it. The check is an empty page with a hidden box and a
// script: the script proves the browser to Akamai and then reloads the page,
// less than 2 s later. The page from the reload can be a check again
// (dailymail.co.uk goes to dailymail.com, and that is checked too). A box
// that shows (a button to hold down) is for the user: no wait for it.
const botCheck = `(() => {
	const c = document.getElementById("sec-if-cpt-container");
	if (c && getComputedStyle(c).display === "none") return "check";
	return document.readyState === "complete" ? "page" : "loading";
})()`

// waitBotCheck waits while the page is a bot check (see botCheck), until the
// page after it has loaded, at most 10 seconds. A picture of the check was
// empty (it measured 10 px): the page "stopped rendering". The log had chromedp's
// "unhandled node event *dom.EventAdoptedStyleSheetsModified", from the
// check's script (see cdpErrorf).
func (rq *wrpReq) waitBotCheck() chromedp.ActionFunc {
	return func(ctx context.Context) error {
		st := time.Now()
		seen := false
		for time.Since(st) < 10*time.Second {
			var s string
			ectx, cancel := context.WithTimeout(ctx, time.Second)
			err := chromedp.Evaluate(botCheck, &s).Do(ectx)
			cancel()
			switch {
			case !seen && (err != nil || s != "check"):
				return nil
			case seen && err == nil && s == "page":
				log.Printf("%s Bot check: the page came after %vms\n", rq.r.RemoteAddr, time.Since(st).Milliseconds())
				return nil
			case !seen:
				log.Printf("%s Bot check: waiting for the page\n", rq.r.RemoteAddr)
				seen = true
			}
			select { // an error here: the page is changing, ask again
			case <-ctx.Done():
				return nil
			case <-time.After(100 * time.Millisecond):
			}
		}
		log.Printf("%s Bot check: no page after %v\n", rq.r.RemoteAddr, time.Since(st).Round(time.Second))
		return nil
	}
}

// Navigate to the desired URL, returns the downloaded file if one was triggered.
// A bot check (see botCheck) is waited for after the action, and before it too:
// St (Stop) would stop a check that is still running, and a click would go to
// its empty page.
func (rq *wrpReq) navigate() *dlFile {
	wrpCach.stopConvs() // the browser gets the CPUs
	resetDownloadState()
	ctxErr(chromedp.Run(brCtx(), rq.waitBotCheck(), rq.action(), rq.waitBotCheck()), rq.w)
	return waitForDownload()
}

// Handle context errors
func ctxErr(err error, w io.Writer) {
	// TODO: callers should have retry logic, perhaps create another function
	// that takes ...chromedp.Action and retries with give up
	if err == nil {
		return
	}
	log.Printf("Context error: %s", err)
	fmt.Fprintf(w, "Context error: %s<BR>\n", err)
	if err.Error() != "context canceled" || brRoot().Err() == nil {
		return // one tab context went away, the browser itself is still there
	}
	vncCheckBrowser()
	fmt.Fprintln(w, "Created new context, try again")
}

// waitForRender waits until the page has loaded and the pictures in the
// viewport have come, at most *delay (5 s at most). The pictures out of view
// don't count: the lazy ones load only when they come into view. A quiet
// network (networkAlmostIdle) counts as loaded, for pages that never end
// loading, but it does not end the wait for the pictures: Chrome tells it once,
// soon after a new page has loaded, and the pictures that a taller viewport
// has just shown were still on their way (empty boxes on a whole page).
func waitForRender() chromedp.ActionFunc {
	return func(ctx context.Context) error {
		timeout := *delay
		if timeout > 5*time.Second {
			timeout = 5 * time.Second
		}
		ch := make(chan struct{}, 1)
		lctx, lcancel := context.WithCancel(ctx)
		defer lcancel()
		chromedp.ListenTarget(lctx, func(ev interface{}) {
			if e, ok := ev.(*page.EventLifecycleEvent); ok && e.Name == "networkAlmostIdle" {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		})
		// two frames at the new size: then the viewport is that size, and
		// the lazy pictures in it have started to load
		fctx, fcancel := context.WithTimeout(ctx, time.Second)
		chromedp.Evaluate(`new Promise(r => requestAnimationFrame(() => requestAnimationFrame(() => r(true))))`, nil,
			func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }).Do(fctx)
		fcancel()
		idle := false
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			select {
			case <-ch:
				idle = true
			default:
			}
			var ready bool
			if err := chromedp.Evaluate(fmt.Sprintf(`(%v || document.readyState === "complete") &&
				Array.from(document.images).every(i => {
					const r = i.getBoundingClientRect();
					return i.complete || r.bottom <= 0 || r.top >= innerHeight;
				})`, idle), &ready).Do(ctx); err != nil {
				return nil
			}
			if ready {
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return nil
	}
}

// maxCapH: Chrome makes a screenshot at most 16384 device px tall (its
// largest surface). In a taller one, the rows past that show the top of
// the page again.
const maxCapH = 16384

// captureTall takes the top w x h CSS px of the page at zoom, also the part
// below the viewport (see fullHeight), in clipped parts of at most maxCapH
// device px, and joins them into one picture of the type png.Decode gives for
// one screenshot (RGBA). The parts wait as PNG and are decoded one at a time,
// so the memory is that of one screenshot.
func captureTall(ctx context.Context, w, h int64, zoom float64) (image.Image, error) {
	// a part is a whole number of device px, so the parts join exactly
	ph := int64(maxCapH / zoom)
	for ph > 1 && math.Abs(float64(ph)*zoom-math.Round(float64(ph)*zoom)) > 1e-6 {
		ph--
	}
	var parts [][]byte
	pw, sum := 0, 0
	for y := int64(0); y < h; y += ph {
		n := min(ph, h-y)
		var b []byte
		err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			b, err = page.CaptureScreenshot().WithFromSurface(true).WithCaptureBeyondViewport(true).
				WithClip(&page.Viewport{Y: float64(y), Width: float64(w), Height: float64(n), Scale: 1}).
				Do(ctx)
			return err
		}))
		if err != nil {
			return nil, err
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		parts = append(parts, b)
		pw = max(pw, cfg.Width)
		sum += cfg.Height
	}
	if len(parts) == 1 {
		return png.Decode(bytes.NewReader(parts[0]))
	}
	o := image.NewRGBA(image.Rect(0, 0, pw, sum))
	y := 0
	for _, b := range parts {
		i, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		draw.Draw(o, image.Rect(0, y, pw, y+i.Bounds().Dy()), i, i.Bounds().Min, draw.Src)
		y += i.Bounds().Dy()
	}
	return o, nil
}

// wrpEval runs js in the main frame in WRP's own world, the one selPopupJS
// has: the page's scripts see what it does to the DOM, and nothing else. It
// waits for a promise. The value goes to res, when res is not nil.
func wrpEval(ctx context.Context, js string, res any) error {
	tree, err := page.GetFrameTree().Do(ctx)
	if err != nil {
		return err
	}
	// the world of that name in the frame: Chrome makes it once a page
	id, err := page.CreateIsolatedWorld(tree.Frame.ID).WithWorldName(selWorld).Do(ctx)
	if err != nil {
		return err
	}
	v, exc, err := runtime.Evaluate(js).WithContextID(id).
		WithReturnByValue(true).WithAwaitPromise(true).Do(ctx)
	if err != nil {
		return err
	}
	if exc != nil {
		return exc
	}
	if res != nil && v != nil && len(v.Value) > 0 {
		return json.Unmarshal(v.Value, res)
	}
	return nil
}

// pageModeJS tells WRP's world in the page what the capture is (%v: full).
// A whole page has no scrollbar (SetScrollbarsHidden), and selPopupJS's list
// may go down to the end of the page (__wrpFull). A fixed height shows the
// scrollbar always, also on a short page; a page that sets overflow on html or
// body keeps its own.
const pageModeJS = `(function(full){
var d=document.documentElement,b=document.body;
window.__wrpFull=full;
if(!d)return;
if(full){if(window.__wrpSb){d.style.overflowY='';window.__wrpSb=false}return}
function o(e){return e?getComputedStyle(e).overflowY:'visible'}
if(o(d)==='visible'&&o(b)==='visible'){d.style.overflowY='scroll';window.__wrpSb=true}})(%v)`

// fullJS: a whole page in a viewport as tall as the screen (see fullHeight).
// The page is scrolled down a screen a frame, so its lazy pictures (Chrome's,
// and the page's own on scroll) come, and back to its top. Then it waits for
// the pictures, until %d ms (-s) are over. The value is the bottom of its
// html, its flow: a short page ends above the bottom of the screen.
const fullJS = `new Promise(function(r){
var end=Date.now()+%d,d=document.documentElement,s=document.scrollingElement||d,y=0;
function frames(f){requestAnimationFrame(function(){requestAnimationFrame(f)})}
function go(t){scrollTo({top:t,left:0,behavior:'instant'})}
function pass(){if(y<s.scrollHeight&&Date.now()<end){go(y);y+=innerHeight;frames(pass);return}go(0);wait()}
function wait(){if(Date.now()<end&&!Array.from(document.images).every(function(i){
 return i.complete||!i.getClientRects().length})){setTimeout(wait,100);return}
 frames(function(){r(Math.ceil(d.getBoundingClientRect().bottom))})}
pass()})`

// clickJS: a click at y (CSS px) of a whole page picture: the page is scrolled
// to have it in the middle of the screen. The value is how far it scrolled.
const clickJS = `new Promise(function(r){scrollTo({top:Math.max(0,%g-innerHeight/2),left:0,behavior:'instant'});` +
	`requestAnimationFrame(function(){requestAnimationFrame(function(){r(scrollY)})})})`

// full: a picture of the whole page (h=0, or slices), not of the screen
func (rq *wrpReq) full() bool {
	return rq.height == 0 || rq.slices
}

// screenH: the height of the screen a whole page is shown on (CSS px). It is
// the form's h, also with slices, or with h=0 3/4 of the width (4:3).
func (rq *wrpReq) screenH() int64 {
	if rq.height > 0 {
		return max(10, int64(float64(rq.height)/rq.zoom))
	}
	return max(10, int64(float64(rq.width)/rq.zoom)*3/4)
}

// contentH: the height of the page (CSS px), at least the viewport's
func contentH(ctx context.Context) (int64, error) {
	var c int64
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, _, _, _, _, s, err := page.GetLayoutMetrics().Do(ctx)
		if err == nil {
			c = int64(math.Ceil(s.Height))
		}
		return err
	}))
	return c, err
}

// fullHeight: the height of a whole page (CSS px) that grew past a viewport
// as tall as itself. A 100vh part grows with the viewport, so such a page
// never fits, and the picture cut off its end. Its pictures also came late,
// or never: the 100vh part had pushed them below the viewport. The page now
// stays in a viewport as tall as the screen (sh), so its 100vh is a screen,
// and captureTall takes the page below the viewport too. h: its height in a
// 10 px viewport.
func fullHeight(ctx context.Context, w, sh, h int64, zoom float64) (int64, error) {
	var flow int64
	err := chromedp.Run(ctx,
		emulation.SetDeviceMetricsOverride(w, sh, zoom, false),
		chromedp.ActionFunc(func(ctx context.Context) error {
			fctx, cancel := context.WithTimeout(ctx, *delay+2*time.Second)
			defer cancel()
			return wrpEval(fctx, fmt.Sprintf(fullJS, delay.Milliseconds()), &flow)
		}),
	)
	if err != nil {
		return max(sh, h+30), err
	}
	c, err := contentH(ctx)
	if err != nil {
		return max(sh, h+30), err
	}
	if c > sh {
		return c, nil // the page goes on below the screen
	}
	// the page fits in the screen: a short one ends where its content does,
	// a 100vh one fills the screen
	return min(sh, max(h+30, flow)), nil
}

// Capture Screenshot using CDP
func (rq *wrpReq) captureScreenshot() {
	vncSuspend() // borrow the viewport from the vnc screencast
	defer vncResume()
	wrpCach.clear()
	var h int64
	var pngCap []byte
	full := rq.full() // slices too: the client asks for the slices it shows
	w := int64(float64(rq.width) / rq.zoom)
	bc := brCtx() // pin the tab for the whole capture
	chromedp.Run(bc,
		// the whole page has no scrollbar: there is nothing to scroll. Set
		// before the page is measured, so it is measured at its full width.
		emulation.SetScrollbarsHidden(full),
		chromedp.ActionFunc(func(ctx context.Context) error {
			if err := wrpEval(ctx, fmt.Sprintf(pageModeJS, full), nil); err != nil {
				log.Printf("%s Page mode not set: %v\n", rq.r.RemoteAddr, err)
			}
			return nil
		}),
		emulation.SetDeviceMetricsOverride(w, 10, rq.zoom, false),
		chromedp.Location(&rq.url),
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, _, _, _, _, s, err := page.GetLayoutMetrics().Do(ctx)
			if err == nil {
				h = int64(math.Ceil(s.Height))
			}
			return nil
		}),
	)
	if rq.proxy {
		rq.url = strings.Replace(rq.url, "https://", "http://", 1)
	}
	log.Printf("%s Landed on: %s, Height: %v\n", rq.r.RemoteAddr, rq.url, h)
	sessSaveURL(rq.url)
	height := int64(float64(rq.height) / rq.zoom)
	if full && h > 0 {
		height = h + 30 // the whole page in the viewport: its lazy pictures come
	}
	var foc focusInfo // for the form's kb: what has the keyboard
	chromedp.Run(
		bc, emulation.SetDeviceMetricsOverride(w, height, rq.zoom, false),
		waitForRender(),
		chromedp.ActionFunc(func(ctx context.Context) error {
			foc = focusNow(ctx)
			return nil
		}),
	)
	// a whole page that fits its viewport is taken as it is
	var c int64
	if full {
		c, _ = contentH(bc)
	}
	if c > height {
		sh := rq.screenH()
		var err error
		if height, err = fullHeight(bc, w, sh, h, rq.zoom); err != nil {
			log.Printf("%s Whole page not measured: %v\n", rq.r.RemoteAddr, err)
		}
		log.Printf("%s Page grew past its viewport (%d px): %d px in a %d px viewport\n", rq.r.RemoteAddr, c, height, sh)
		h = height
	}
	// Capture screenshot. Chrome can't take a viewport taller than maxCapH
	// device px in one: captureTall takes it in parts and joins them (tall).
	// It takes a whole page from below the viewport too.
	var tall image.Image
	if full || float64(height)*rq.zoom > maxCapH {
		var err error
		tall, err = captureTall(bc, w, height, rq.zoom)
		ctxErr(err, rq.w)
	} else {
		ctxErr(chromedp.Run(bc, chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			pngCap, err = page.CaptureScreenshot().Do(ctx)
			return err
		})), rq.w)
	}
	decode := func() (image.Image, error) {
		if tall != nil {
			return tall, nil
		}
		return png.Decode(bytes.NewReader(pngCap))
	}
	seq := shortuuid.New()
	var imgExt string
	if rq.imgType == "gip" {
		imgExt = "gif"
	} else {
		imgExt = rq.imgType
	}
	imgPath := fmt.Sprintf("/img/%s.%s", seq, imgExt)
	mapPath := fmt.Sprintf("/map/%s.map", seq)
	wrpCach.addMap(mapPath, *rq)
	var sSize string
	var iW, iH int
	imgType := rq.imgType
	if rq.slices {
		imgType = "slices"
	}
	switch imgType {
	case "slices":
		i, err := decode()
		if err != nil {
			log.Printf("%s Failed to decode PNG screenshot: %s\n", rq.r.RemoteAddr, err)
			fmt.Fprintf(rq.w, "<BR>Unable to decode page PNG screenshot:<BR>%s<BR>\n", err)
			return
		}
		st := time.Now()
		if rq.imgType == "gif" { // made its colors as the slices need it
			c := newPageConv(i, rq.nColors, rq.dither)
			c.name = rq.r.RemoteAddr + " " + imgPath
			wrpCach.addConv(imgPath, c, int(rq.jQual))
			go c.background()
		} else {
			wrpCach.addFull(imgPath, i, rq.imgType, int(rq.jQual))
		}
		iW = i.Bounds().Dx()
		iH = i.Bounds().Dy()
		sSize = "slices"
		log.Printf("%s Kept the page for slices: %s, Type: %s, Res: %dx%d, Time: %vms\n", rq.r.RemoteAddr, imgPath, rq.imgType, iW, iH, time.Since(st).Milliseconds())
	case "gip":
		i, err := decode()
		if err != nil {
			log.Printf("%s Failed to decode PNG screenshot: %s\n", rq.r.RemoteAddr, err)
			fmt.Fprintf(rq.w, "<BR>Unable to decode page PNG screenshot:<BR>%s<BR>\n", err)
			return
		}
		st := time.Now()
		var gipBuf bytes.Buffer
		err = gip.Encode(&gipBuf, i, nil)
		if err != nil {
			log.Printf("%s Failed to encode GIP: %s\n", rq.r.RemoteAddr, err)
			fmt.Fprintf(rq.w, "<BR>Unable to encode GIP:<BR>%s<BR>\n", err)
			return
		}
		wrpCach.addImg(imgPath, gipBuf)
		sSize = fmt.Sprintf("%.0f KB", float32(len(gipBuf.Bytes()))/1024.0)
		iW = i.Bounds().Max.X
		iH = i.Bounds().Max.Y
		log.Printf("%s Encoded GIP image: %s, Size: %s, Res: %dx%d, Time: %vms\n", rq.r.RemoteAddr, imgPath, sSize, iW, iH, time.Since(st).Milliseconds())
	case "png":
		if tall != nil { // png sends the screenshot as it is: one PNG
			var b bytes.Buffer
			if err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&b, tall); err == nil {
				pngCap = b.Bytes()
			}
		}
		pngBuf := bytes.NewBuffer(pngCap)
		wrpCach.addImg(imgPath, *pngBuf)
		cfg, _, _ := image.DecodeConfig(pngBuf)
		sSize = fmt.Sprintf("%.0f KB", float32(len(pngBuf.Bytes()))/1024.0)
		iW = cfg.Width
		iH = cfg.Height
		log.Printf("%s Got PNG image: %s, Size: %s, Res: %dx%d\n", rq.r.RemoteAddr, imgPath, sSize, iW, iH)
	case "gif":
		i, err := decode()
		if err != nil {
			log.Printf("%s Failed to decode PNG screenshot: %s\n", rq.r.RemoteAddr, err)
			fmt.Fprintf(rq.w, "<BR>Unable to decode page PNG screenshot:<BR>%s<BR>\n", err)
			return
		}
		st := time.Now()
		var gifBuf bytes.Buffer
		err = gif.Encode(&gifBuf, gifPalette(i, rq.nColors, rq.dither), &gif.Options{})
		if err != nil {
			log.Printf("%s Failed to encode GIF: %s\n", rq.r.RemoteAddr, err)
			fmt.Fprintf(rq.w, "<BR>Unable to encode GIF:<BR>%s<BR>\n", err)
			return
		}
		wrpCach.addImg(imgPath, gifBuf)
		sSize = fmt.Sprintf("%.0f KB", float32(len(gifBuf.Bytes()))/1024.0)
		iW = i.Bounds().Max.X
		iH = i.Bounds().Max.Y
		log.Printf("%s Encoded GIF image: %s, Size: %s, Colors: %d, Res: %dx%d, Time: %vms\n", rq.r.RemoteAddr, imgPath, sSize, rq.nColors, iW, iH, time.Since(st).Milliseconds())
	case "jpg":
		i, err := decode()
		if err != nil {
			log.Printf("%s Failed to decode PNG screenshot: %s\n", rq.r.RemoteAddr, err)
			fmt.Fprintf(rq.w, "<BR>Unable to decode page PNG screenshot:<BR>%s<BR>\n", err)
			return
		}
		st := time.Now()
		var jpgBuf bytes.Buffer
		err = jpeg.Encode(&jpgBuf, i, &jpeg.Options{Quality: int(rq.jQual)})
		if err != nil {
			log.Printf("%s Failed to encode JPG: %s\n", rq.r.RemoteAddr, err)
			fmt.Fprintf(rq.w, "<BR>Unable to encode JPG:<BR>%s<BR>\n", err)
			return
		}
		wrpCach.addImg(imgPath, jpgBuf)
		sSize = fmt.Sprintf("%.0f KB", float32(len(jpgBuf.Bytes()))/1024.0)
		iW = i.Bounds().Max.X
		iH = i.Bounds().Max.Y
		log.Printf("%s Encoded JPG image: %s, Size: %s, Quality: %d, Res: %dx%d, Time: %vms\n", rq.r.RemoteAddr, imgPath, sSize, *defJpgQual, iW, iH, time.Since(st).Milliseconds())
	}
	if rq.proxy {
		rq.w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(rq.w, "<HTML><HEAD>%s<TITLE>%s</TITLE></HEAD><BODY BGCOLOR=\"%s\">"+
			"<A HREF=\"%s\"><IMG SRC=\"%s\" BORDER=\"0\" WIDTH=\"%d\" HEIGHT=\"%d\" ISMAP></A>"+
			"</BODY></HTML>", rq.baseTag(), rq.url, *bgColor, mapPath, imgPath, iW, iH)
	} else {
		rq.printUI(uiParams{
			bgColor:    *bgColor,
			pageHeight: fmt.Sprintf("%d PX", h),
			imgSize:    sSize,
			imgURL:     imgPath,
			mapURL:     mapPath,
			imgWidth:   iW,
			imgHeight:  iH,
			focus:      foc.Kind,
		})
	}
	log.Printf("%s Done with capture for %s\n", rq.r.RemoteAddr, rq.url)
}

func mapServer(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s ISMAP Request for %s [%+v]\n", r.RemoteAddr, r.URL.Path, r.URL.RawQuery)
	rq, ok := wrpCach.getMap(r.URL.Path)
	rq.r = r
	rq.w = w
	if !ok {
		fmt.Fprintf(w, "Unable to find map %s\n", r.URL.Path)
		log.Printf("Unable to find map %s\n", r.URL.Path)
		return
	}
	n, err := fmt.Sscanf(r.URL.RawQuery, "%d,%d", &rq.mouseX, &rq.mouseY)
	if err != nil || n != 2 {
		fmt.Fprintf(w, "n=%d, err=%s\n", n, err)
		log.Printf("%s ISMAP n=%d, err=%s\n", r.RemoteAddr, n, err)
		return
	}
	log.Printf("%s WrpReq from ISMAP: %+v\n", r.RemoteAddr, rq.logged())
	if len(rq.url) < 4 {
		rq.printUI(uiParams{})
		return
	}
	browserMu.Lock()
	defer browserMu.Unlock()
	if dl := rq.navigate(); dl != nil {
		if rq.proxy {
			writeDownload(w, dl)
		} else {
			http.Redirect(w, r, cacheDownload(dl), http.StatusFound)
		}
		return
	}
	if rq.proxy {
		bc := brCtx()
		chromedp.Run(bc, waitForRender())
		var loc string
		chromedp.Run(bc, chromedp.Location(&loc))
		loc = strings.Replace(loc, "https://", "http://", 1)
		http.Redirect(w, r, loc, http.StatusFound)
		return
	}
	rq.captureScreenshot()
}

// TODO: merge this with html mode IMGZ
func imgServerMap(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s IMG Request for %s\n", r.RemoteAddr, r.URL.Path)
	logNote(r)
	if f, ok := wrpCach.getFull(r.URL.Path); ok {
		sliceServer(w, r, f)
		return
	}
	imgBuf, ok := wrpCach.getImg(r.URL.Path)
	if !ok || imgBuf.Bytes() == nil {
		fmt.Fprintf(w, "Unable to find image %s\n", r.URL.Path)
		log.Printf("%s Unable to find image %s\n", r.RemoteAddr, r.URL.Path)
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, ".gif"):
		w.Header().Set("Content-Type", "image/gif")
	case strings.HasSuffix(r.URL.Path, ".png"):
		w.Header().Set("Content-Type", "image/png")
	case strings.HasSuffix(r.URL.Path, ".jpg"):
		w.Header().Set("Content-Type", "image/jpeg")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(imgBuf.Bytes())))
	w.Header().Set("Cache-Control", "max-age=0")
	w.Header().Set("Expires", "-1")
	w.Header().Set("Pragma", "no-cache")
	w.Write(imgBuf.Bytes())
	w.(http.Flusher).Flush()
}

// Slice mode: rows y up to y+h of the page (?y=Y&h=H), or the whole page
// when the request has neither
func sliceServer(w http.ResponseWriter, r *http.Request, f cachedImg) {
	st := time.Now()
	i := f.full
	q := r.URL.Query()
	if q.Has("y") || q.Has("h") || f.conv != nil {
		y, _ := strconv.Atoi(q.Get("y"))
		var h int
		if f.conv != nil {
			h = f.conv.h
		} else {
			h = i.Bounds().Dy()
		}
		if q.Has("h") {
			h, _ = strconv.Atoi(q.Get("h"))
		}
		if f.conv != nil {
			i = f.conv.rows(y, h)
		} else {
			i = cut(i, y, h)
		}
		if i == nil {
			http.Error(w, "No such rows in "+r.URL.Path, http.StatusNotFound)
			log.Printf("%s No rows y=%v h=%v in %s\n", r.RemoteAddr, q.Get("y"), q.Get("h"), r.URL.Path)
			return
		}
	}
	// the picture as the client takes it (see picfmt.go)
	what, err := sendSlice(w, r, i, f.typ, f.jq)
	if err != nil {
		http.Error(w, "Unable to encode "+r.URL.Path+": "+err.Error(), http.StatusInternalServerError)
		log.Printf("%s Unable to encode %s: %s\n", r.RemoteAddr, r.URL.Path, err)
		return
	}
	if f.conv != nil {
		f.conv.start() // after the first slice, the background may take the CPUs
	}
	log.Printf("%s Slice %s y=%v h=%v: %dx%d, %s, %vms\n", r.RemoteAddr, r.URL.Path, q.Get("y"), q.Get("h"), i.Bounds().Dx(), i.Bounds().Dy(), what, time.Since(st).Milliseconds())
}

// Rows y up to y+h of a picture, as a picture of its own that starts at
// 0,0 (gif.Encode wants that), or nil if it has no such rows. A paletted
// picture shares its rows with the page; others are copied.
func cut(i image.Image, y, h int) image.Image {
	b := i.Bounds()
	if y < 0 {
		y = 0
	}
	if y+h > b.Dy() {
		h = b.Dy() - y
	}
	if h <= 0 {
		return nil
	}
	r := image.Rect(b.Min.X, b.Min.Y+y, b.Max.X, b.Min.Y+y+h)
	if p, ok := i.(*image.Paletted); ok {
		s := p.SubImage(r).(*image.Paletted)
		return &image.Paletted{Pix: s.Pix, Stride: s.Stride, Rect: image.Rect(0, 0, r.Dx(), r.Dy()), Palette: s.Palette}
	}
	o := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(o, o.Bounds(), i, r.Min, draw.Src)
	return o
}

// A picture for gif.Encode that needs no conversion for each slice: the
// 2 color dither (gray, 0 or 255) gets a black and white palette
func paletted(i image.Image) image.Image {
	switch p := i.(type) {
	case *image.Paletted:
		return p
	case *image.Gray:
		b := p.Bounds()
		o := image.NewPaletted(image.Rect(0, 0, b.Dx(), b.Dy()), color.Palette{color.Black, color.White})
		for y := 0; y < b.Dy(); y++ {
			s := p.Pix[y*p.Stride : y*p.Stride+b.Dx()]
			d := o.Pix[y*o.Stride : y*o.Stride+b.Dx()]
			for x, v := range s {
				if v >= 128 {
					d[x] = 1
				}
			}
		}
		return o
	}
	b := i.Bounds()
	o := image.NewPaletted(image.Rect(0, 0, b.Dx(), b.Dy()), palette.Plan9)
	draw.FloydSteinberg.Draw(o, o.Bounds(), i, b.Min)
	return o
}

func encodeImg(w io.Writer, i image.Image, typ string, jq int) error {
	switch typ {
	case "gif":
		return gif.Encode(w, i, &gif.Options{NumColors: 256})
	case "gip":
		return gip.Encode(w, i, nil)
	case "jpg":
		return jpeg.Encode(w, i, &jpeg.Options{Quality: jq})
	}
	return png.Encode(w, i)
}
