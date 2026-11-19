// WRP ISMAP / ChromeDP routines
package main

import (
	"bytes"
	"context"
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

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/lithammer/shortuuid/v4"
	"github.com/tenox7/gip"
)

type cachedImg struct {
	buf bytes.Buffer
	// Slice mode (sl=1): the whole page, kept decoded (for GIF already
	// dithered or quantized) and encoded in slices on request
	full image.Image
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

func (c *wrpCache) getFull(path string) (cachedImg, bool) {
	c.Lock()
	defer c.Unlock()
	e, ok := c.imgs[path]
	if !ok || e.full == nil {
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

func (c *wrpCache) clear() {
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
	)
	if *userAgent == "jnrbsn" {
		if ua := fetchJnrbsnUserAgent(); ua != "" {
			*userAgent = ua
		}
	}
	if *userAgent != "" {
		opts = append(opts, chromedp.UserAgent(*userAgent))
	}
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
	setBrRoot(chromedp.NewContext(actx))
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
	return nil
}

// Determine what action to take
func (rq *wrpReq) action() chromedp.Action {
	// Mouse Click
	if rq.mouseX > 0 && rq.mouseY > 0 {
		log.Printf("%s Mouse Click %d,%d\n", rq.r.RemoteAddr, rq.mouseX, rq.mouseY)
		return chromedp.MouseClickXY(float64(rq.mouseX)/float64(rq.zoom), float64(rq.mouseY)/float64(rq.zoom))
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
			return chromedp.KeyEvent("\b")
		case "Rt":
			return chromedp.KeyEvent("\r")
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
		log.Printf("%s Sending Keys: %#v\n", rq.r.RemoteAddr, rq.keys)
		return chromedp.KeyEvent(rq.keys)
	}
	// Navigate to URL
	log.Printf("%s Processing Navigate Request for %s\n", rq.r.RemoteAddr, rq.url)
	return chromedp.Navigate(rq.url)
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

// Navigate to the desired URL, returns the downloaded file if one was triggered.
func (rq *wrpReq) navigate() *dlFile {
	resetDownloadState()
	ctxErr(chromedp.Run(brCtx(), rq.action()), rq.w)
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
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			select {
			case <-ch:
				return nil
			default:
			}
			var ready bool
			if err := chromedp.Evaluate(
				`document.readyState === "complete" && Array.from(document.images).every(i => i.complete)`,
				&ready,
			).Do(ctx); err != nil {
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

// https://github.com/chromedp/chromedp/issues/979
func chromedpCaptureScreenshot(res *[]byte, h int64) chromedp.Action {
	if res == nil {
		panic("res cannot be nil") // TODO: do not panic here, return error
	}
	if h == 0 {
		return chromedp.CaptureScreenshot(res)
	}

	return chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		*res, err = page.CaptureScreenshot().Do(ctx)
		return err
	})
}

// Capture Screenshot using CDP
func (rq *wrpReq) captureScreenshot() {
	vncSuspend() // borrow the viewport from the vnc screencast
	defer vncResume()
	wrpCach.clear()
	var h int64
	var pngCap []byte
	bc := brCtx() // pin the tab for the whole capture
	chromedp.Run(bc,
		emulation.SetDeviceMetricsOverride(int64(float64(rq.width)/rq.zoom), 10, rq.zoom, false),
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
	capH := rq.height
	if rq.slices {
		capH = 0 // the whole page: the client asks for the slices it shows
	}
	height := int64(float64(rq.height) / rq.zoom)
	if capH == 0 && h > 0 {
		height = h + 30
	}
	chromedp.Run(
		bc, emulation.SetDeviceMetricsOverride(int64(float64(rq.width)/rq.zoom), height, rq.zoom, false),
		waitForRender(),
	)
	// Capture screenshot...
	ctxErr(chromedp.Run(bc, chromedpCaptureScreenshot(&pngCap, capH)), rq.w)
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
		i, err := png.Decode(bytes.NewReader(pngCap))
		if err != nil {
			log.Printf("%s Failed to decode PNG screenshot: %s\n", rq.r.RemoteAddr, err)
			fmt.Fprintf(rq.w, "<BR>Unable to decode page PNG screenshot:<BR>%s<BR>\n", err)
			return
		}
		st := time.Now()
		if rq.imgType == "gif" {
			i = paletted(gifPalette(i, rq.nColors))
		}
		wrpCach.addFull(imgPath, i, rq.imgType, int(rq.jQual))
		iW = i.Bounds().Dx()
		iH = i.Bounds().Dy()
		sSize = "slices"
		log.Printf("%s Kept the page for slices: %s, Type: %s, Res: %dx%d, Time: %vms\n", rq.r.RemoteAddr, imgPath, rq.imgType, iW, iH, time.Since(st).Milliseconds())
	case "gip":
		i, err := png.Decode(bytes.NewReader(pngCap))
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
		pngBuf := bytes.NewBuffer(pngCap)
		wrpCach.addImg(imgPath, *pngBuf)
		cfg, _, _ := image.DecodeConfig(pngBuf)
		sSize = fmt.Sprintf("%.0f KB", float32(len(pngBuf.Bytes()))/1024.0)
		iW = cfg.Width
		iH = cfg.Height
		log.Printf("%s Got PNG image: %s, Size: %s, Res: %dx%d\n", rq.r.RemoteAddr, imgPath, sSize, iW, iH)
	case "gif":
		i, err := png.Decode(bytes.NewReader(pngCap))
		if err != nil {
			log.Printf("%s Failed to decode PNG screenshot: %s\n", rq.r.RemoteAddr, err)
			fmt.Fprintf(rq.w, "<BR>Unable to decode page PNG screenshot:<BR>%s<BR>\n", err)
			return
		}
		st := time.Now()
		var gifBuf bytes.Buffer
		err = gif.Encode(&gifBuf, gifPalette(i, rq.nColors), &gif.Options{})
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
		i, err := png.Decode(bytes.NewReader(pngCap))
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
	log.Printf("%s WrpReq from ISMAP: %+v\n", r.RemoteAddr, rq)
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
	if q.Has("y") || q.Has("h") {
		y, _ := strconv.Atoi(q.Get("y"))
		h := i.Bounds().Dy()
		if q.Has("h") {
			h, _ = strconv.Atoi(q.Get("h"))
		}
		i = cut(i, y, h)
		if i == nil {
			http.Error(w, "No such rows in "+r.URL.Path, http.StatusNotFound)
			log.Printf("%s No rows y=%v h=%v in %s\n", r.RemoteAddr, q.Get("y"), q.Get("h"), r.URL.Path)
			return
		}
	}
	var buf bytes.Buffer
	if err := encodeImg(&buf, i, f.typ, f.jq); err != nil {
		http.Error(w, "Unable to encode "+r.URL.Path+": "+err.Error(), http.StatusInternalServerError)
		log.Printf("%s Unable to encode %s: %s\n", r.RemoteAddr, r.URL.Path, err)
		return
	}
	switch f.typ {
	case "png":
		w.Header().Set("Content-Type", "image/png")
	case "jpg":
		w.Header().Set("Content-Type", "image/jpeg")
	default:
		w.Header().Set("Content-Type", "image/gif")
	}
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.Header().Set("Cache-Control", "max-age=0")
	w.Header().Set("Expires", "-1")
	w.Header().Set("Pragma", "no-cache")
	w.Write(buf.Bytes())
	log.Printf("%s Slice %s y=%v h=%v: %dx%d, %d bytes, %vms\n", r.RemoteAddr, r.URL.Path, q.Get("y"), q.Get("h"), i.Bounds().Dx(), i.Bounds().Dy(), buf.Len(), time.Since(st).Milliseconds())
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
