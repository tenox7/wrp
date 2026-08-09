// WRP VNC server - live interactive view of the browser via CDP screencast.
// Tight encoding only: jpeg passthrough for 16/32 bpp clients, dithered or
// quantized tight basic for 8 bpp. Framebuffer = nav bar strip + browser
// viewport: page scrolling happens in the browser (wheel/keys), viewport size
// follows the VNC client via SetDesktopSize (TigerVNC remote resize). The nav
// bar is a separate rect so browser frames pass through chrome untranscoded.
package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"log"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

const (
	rfbEncodingTight    = 7
	rfbEncodingDesk     = -223 // DesktopSize
	rfbEncodingExtDesk  = -308 // ExtendedDesktopSize
	rfbEncodingCursor   = -239 // RichCursor
	rfbEncodingFence    = -312
	rfbEncodingContUpd  = -313
	rfbEncodingQemuKey  = -258
	rfbFenceRequest     = 1 << 31
	rfbFenceBlockBefore = 1 << 0
	rfbTightJpeg        = 0x09
	rfbSecNone          = 1
	rfbSecVNCAuth       = 2
	vncMaxDim           = 4096    // sane viewport limit, wider rects get split
	vncTightMax         = 2048    // a tight rect may not be wider than this
	vncMinDim           = 16      // smallest viewport dimension we ask the browser for
	vncMaxClients       = 16      // refuse further connections beyond this
	vncMaxCut           = 1 << 20 // largest clipboard transfer we buffer
	vncPalFrames        = 45      // frames between colormap palette rebuilds
	vncScrollHint       = 25      // percent of the frame that must be dirty to look for a scroll
	vncCopyGain         = 50      // and percent left to repaint above which the copy is not worth it
	vncCopyMax          = 24      // consecutive CopyRects before an exact repaint
	vncReconcile        = 5 * time.Second
	vncSlowFrame        = 400 * time.Millisecond // frame ack slower than this, drop quality
	vncFastFrame        = 150 * time.Millisecond // faster than this, climb back
	vncMinQual          = 25                     // jpeg quality floor when adapting
	vncDblClick         = 500 * time.Millisecond
	vncHandshakeTO      = 30 * time.Second  // whole handshake, before we know the client
	vncWriteTO          = 60 * time.Second  // per framebuffer update, catches stalled clients
	vncActTO            = 10 * time.Second  // per cdp call on the input path
	vncTabTO            = 20 * time.Second  // and on the tab management path
	vncTabH             = 20                // tab bar height
	vncNavH             = 20                // nav bar height
	vncChromeH          = vncTabH + vncNavH // total ui strip above the page
	vncTabW             = 160               // max tab width
	vncBtnW             = 22
	vncZoomX            = 172 // zoom percent label
	vncURLX             = 204 // url field start
)

// True color pixel format negotiated with a client
type vncPixFmt struct {
	bpp              byte
	bigEnd           bool
	rMax, gMax, bMax uint16
	rSh, gSh, bSh    byte
}

// Matches what ServerInit advertises: 32 bpp, rgb 888 shifts 16/8/0
var vncDefaultPF = vncPixFmt{32, false, 255, 255, 255, 16, 8, 0}

// For 8 bit colormap clients: classic bgr233
var vncBGR233 = vncPixFmt{8, false, 7, 7, 3, 0, 3, 6}

type vncClient struct {
	conn net.Conn
	wmu  sync.Mutex // guards conn writes: frame writer vs inline resize ack
	poke chan struct{}
	done chan struct{}
	// guarded by vncSrv mutex
	pending    bool
	sentSeq    uint64
	sentBar    uint64
	annW, annH int // framebuffer size announced to this client, includes nav bar
	ackW, ackH int // size this client asked for, acked by the writer once reached
	extDesk    bool
	extInit    bool // owe the client its initial ExtendedDesktopSize announce
	deskSize   bool
	needCursor bool
	pf         vncPixFmt
	indexed    bool // colormap client: we control the palette via SetColourMapEntries
	palSize    int  // colormap entries this client has, from its depth
	warnedFit  bool
	enc        vncEnc // best encoding this client offered
	hasDeskNm  bool
	resetEnc   bool // encoding changed: writer must restart its zlib stream
	forceFull  bool // non incremental request: resend every tile, not just dirty ones
	hasCopyR   bool
	hasFence   bool
	hasContUpd bool
	sentCut    string    // last selection pushed to this client as ServerCutText
	contUpd    bool      // client asked for updates without polling for them
	fencePend  bool      // waiting for the client to echo our fence, ie still drawing
	fenceSent  time.Time // when, so the echo measures the round trip
	adaptQual  int       // jpeg quality after link adaptation, 0 until measured
	jpegQual   int       // from the jpeg quality level pseudo encoding, 0 = our default
	// frame writer goroutine only: persistent zlib streams, last colormap, last
	// tile hashes this client actually received
	zbuf     [2]bytes.Buffer
	zw       [2]*zlib.Writer
	lastCmap []byte
	tiles    []uint64
	samp     *vncSamp // sample plane of what this client actually has
	copies   int      // CopyRects sent since the last exact repaint
	sentName string
	lut      []int16 // memoized palette lookups for the current palette
	palVer   uint64
	lbBuf    *image.RGBA // reused letterbox buffer for clients that cannot resize
	// read loop only
	buttons  byte
	mods     int64
	lastBtn  byte
	lastClkX int
	lastClkY int
	lastClk  time.Time
	clicks   int
}

// One browser tab: its own chromedp context, brCtx() points at the active one
type vncTab struct {
	ctx     context.Context
	cncl    context.CancelFunc
	listCtx context.Context // context the screencast listener is bound to
	id      target.ID
	title   string
	url     string
}

type vncState struct {
	sync.Mutex
	clients    map[*vncClient]bool
	tabs       []*vncTab
	active     int
	frame      []byte          // current browser frame as jpeg, for tight passthrough
	frameImg   *image.RGBA     // same frame decoded once, shared by every client
	frameTiles []uint64        // tile hashes of frameImg
	frameSamp  *vncSamp        // subsampled frameImg, for scroll detection
	fgw, fgh   int             // tile grid dimensions
	pals       map[int]*vncPal // adaptive palettes by size, shared by colormap clients
	palSeq     uint64          // frames since the palettes were last rebuilt
	palVer     uint64
	fw, fh     int // dimensions of current frame
	seq        uint64
	deskName   string // page title, pushed to viewers that asked for DesktopName
	barJpg     []byte // nav bar strip as jpeg
	barImg     *image.RGBA
	barW       int
	barSeq     uint64
	urlText    string  // url shown in the bar
	edit       string  // url being edited
	focused    bool    // keyboard focus on the url field
	vw, vh     int     // desired framebuffer size (viewport is vh-vncNavH)
	zoom       float64 // page zoom, applied as css zoom in the browser
	zoomScript page.ScriptIdentifier
	menu       vncMenu // server drawn right click menu, composited into frames
	clipboard  string  // last ClientCutText, pasted on ctrl+v
	selection  string  // last page selection we pushed out as ServerCutText
	suspend    int     // nested http capture count, screencast is off while > 0
	casting    bool
	inputCh    chan chromedp.Action
	coalCh     chan struct{} // a coalesced pointer event is waiting
	coal       vncCoal
	ackCh      chan vncAck
	syncCh     chan struct{} // a target event wants the tab list reconciled
}

// Motion and wheel are the two inputs a viewer produces faster than chrome can
// consume them: one cdp round trip each, and a single flick of the mouse is
// hundreds of motion events. Dispatching them all put clicks seconds behind the
// pointer, because a click queues behind every move that preceded it. Motion
// coalesces to the newest position - a press carries its own coordinates, so
// dropping stale moves changes nothing - and wheel deltas add up, so a fast
// scroll keeps its full distance in one event.
type vncCoal struct {
	move     chromedp.Action // newest pointer position, nil if none pending
	whl      bool
	wx, wy   float64 // where the accumulated wheel happened
	wdx, wdy float64
	wmod     input.Modifier
}

// Screencast frame acknowledgement, handled off the cdp event goroutine
type vncAck struct {
	ctx context.Context
	id  int64
}

var vncSrv vncState

// Open connections, including ones still in handshake
var vncConns atomic.Int64

func vncClamp(v int) int {
	if v < vncMinDim {
		return vncMinDim
	}
	if v > vncMaxDim {
		return vncMaxDim
	}
	return v
}

// Server state, without anything that needs a browser attached
func vncInit() {
	sw, sh, sz := sessVncGeom() // saved framebuffer, takes its own lock
	vncSrv.Lock()
	vncSrv.vw = vncClamp(int(defGeom.w))
	vncSrv.vh = int(defGeom.h)
	if vncSrv.vh < 10 {
		vncSrv.vh = 768
	}
	vncSrv.vh = vncClamp(vncSrv.vh) + vncChromeH
	vncSrv.zoom = 1.0
	if sw > 0 { // already includes the bars, do not add them again
		vncSrv.vw, vncSrv.vh = sw, sh
	}
	if sz > 0 {
		vncSrv.zoom = sz
	}
	vncSrv.clients = make(map[*vncClient]bool)
	vncSrv.tabs = []*vncTab{{ctx: brCtx(), cncl: brCncl()}}
	vncSrv.inputCh = make(chan chromedp.Action, 256)
	vncSrv.coalCh = make(chan struct{}, 1)
	vncSrv.ackCh = make(chan vncAck, 64)
	vncSrv.syncCh = make(chan struct{}, 1)
	vncSrv.Unlock()
	vncPublishPlaceholder() // takes the lock itself
}

// Accept loop, split out so tests can drive the protocol without a browser
func vncServe(ln net.Listener) {
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				log.Printf("VNC: accept: %v", err)
				return
			}
			// count accepted conns, not handshaked ones: a burst would all
			// be mid handshake and slip past a check on vncSrv.clients
			if n := vncConns.Add(1); n > vncMaxClients {
				log.Printf("%s VNC rejected, %d connections already open", c.RemoteAddr(), n-1)
				vncConns.Add(-1)
				c.Close()
				continue
			}
			go func() {
				defer vncConns.Add(-1)
				vncHandle(c)
			}()
		}
	}()
}

func vncStart() {
	vncInit()
	ln, err := net.Listen("tcp", *vncAddr)
	if err != nil { // http mode still works, don't take the whole proxy down
		log.Printf("VNC: listen: %v, VNC server disabled", err)
		return
	}
	vncWorkers()
	log.Printf("Started VNC server on %v, initial framebuffer %vx%v", *vncAddr, vncSrv.vw, vncSrv.vh)
	if !sessRestoreTabs() && *vncPage != "" {
		vncSetURL(*vncPage)
		vncDispatch(vncNav(*vncPage))
	}
	vncServe(ln)
}

// Background goroutines the server needs once a browser is attached, split out
// so a test can bring the server up on a listener of its own
func vncWorkers() {
	go vncTabPoller()
	go vncSelPoller()
	go vncWatchTargets()
	go func() { // ordered browser input dispatch
		run := func(a chromedp.Action) {
			if err := vncRun(brCtx(), vncActTO, a); err != nil {
				log.Printf("VNC: input dispatch: %v", err)
				vncCheckBrowser()
			}
		}
		for {
			// discrete events keep their order and are never dropped; the
			// coalesced pointer state is at most two actions behind
			select {
			case a := <-vncSrv.inputCh:
				run(a)
			case <-vncSrv.coalCh:
				for _, a := range vncTakeCoal() {
					run(a)
				}
			}
		}
	}()
	// Chrome will not send the next frame until the last one is acked, so
	// pacing the acks caps the frame rate at the source: no frame is ever
	// produced just to be thrown away.
	go func() {
		// on an absolute schedule: timing the gap from when the last ack was
		// sent makes every period that gap plus a cdp round trip, so the real
		// rate sits under the one asked for and wanders
		var next time.Time
		for a := range vncSrv.ackCh {
			if *vncFPS > 0 {
				now := time.Now()
				if next.Before(now) { // first frame, or chrome fell behind
					next = now
				}
				if d := next.Sub(now); d > 0 {
					time.Sleep(d)
				}
				next = next.Add(time.Second / time.Duration(*vncFPS))
			}
			// an ack that never comes back stops the screencast for good, so
			// this worker gives up on it like everything else on the vnc path
			vncRun(a.ctx, vncActTO, page.ScreencastFrameAck(a.id))
		}
	}()
}

func vncBgColor() color.RGBA {
	var r, g, b uint8
	n, err := fmt.Sscanf(*bgColor, "#%02x%02x%02x", &r, &g, &b)
	if err != nil || n != 3 {
		return color.RGBA{0xF0, 0xF0, 0xF0, 0xFF}
	}
	return color.RGBA{r, g, b, 0xFF}
}

func vncPublishPlaceholder() {
	fb := image.NewRGBA(image.Rect(0, 0, vncSrv.vw, vncSrv.vh-vncChromeH))
	draw.Draw(fb, fb.Bounds(), image.NewUniform(vncBgColor()), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, fb, &jpeg.Options{Quality: int(*defJpgQual)}); err != nil {
		return
	}
	vncPublishJPEG(buf.Bytes())
}

// Publish a browser frame to all connected clients. Decoding and tile hashing
// happen once here rather than per client, and the colormap quantization only
// runs when somebody is actually attached at 8 bpp.
func vncPublishJPEG(jpg []byte) {
	vncSrv.Lock()
	if vncSrv.suspend > 0 {
		vncSrv.Unlock()
		return
	}
	var sizes []int // palette sizes in use, usually just the one
	for c := range vncSrv.clients {
		if c.indexed && !slices.Contains(sizes, c.palSize) {
			sizes = append(sizes, c.palSize)
		}
	}
	rebuild := vncSrv.palSeq == 0 || vncSrv.seq-vncSrv.palSeq >= vncPalFrames
	vncSrv.Unlock()

	img := vncDecodeRGBA(jpg)
	if img == nil {
		log.Printf("VNC: bad frame")
		return
	}
	tiles, samp, gw, gh := vncHashFrame(img)
	w, h := img.Bounds().Dx(), img.Bounds().Dy()

	// only the dirty rects get mapped through a palette later, so all that is
	// needed here is keeping the palettes themselves reasonably current
	pals := map[int]*vncPal{}
	if len(sizes) > 0 && rebuild {
		vncSrv.Lock()
		ver := vncSrv.palVer + 1
		vncSrv.palVer = ver
		vncSrv.Unlock()
		for _, n := range sizes {
			pals[n] = vncBuildPal(img, n, ver)
		}
	}

	vncSrv.Lock()
	if vncSrv.suspend > 0 {
		vncSrv.Unlock()
		return
	}
	vncSrv.frame, vncSrv.frameImg, vncSrv.frameTiles, vncSrv.frameSamp = jpg, img, tiles, samp
	vncSrv.fgw, vncSrv.fgh = gw, gh
	vncSrv.fw, vncSrv.fh = w, h
	vncSrv.seq++
	if len(pals) > 0 {
		if vncSrv.pals == nil {
			vncSrv.pals = map[int]*vncPal{}
		}
		for n, p := range pals {
			vncSrv.pals[n] = p
		}
		vncSrv.palSeq = vncSrv.seq
	}
	barW := vncSrv.barW
	for c := range vncSrv.clients {
		vncPoke(c)
	}
	vncSrv.Unlock()
	if barW != w {
		vncDrawBar(w)
	}
}

func vncPoke(c *vncClient) {
	select {
	case c.poke <- struct{}{}:
	default:
	}
}

// ---- nav bar ----

var vncBtnLabels = []string{"<", ">", "R", "X", "H", "-", "+"}

func vncBorder(img *image.RGBA, x0, y0, x1, y1 int, c color.Color) {
	for x := x0; x < x1; x++ {
		img.Set(x, y0, c)
		img.Set(x, y1-1, c)
	}
	for y := y0; y < y1; y++ {
		img.Set(x0, y, c)
		img.Set(x1-1, y, c)
	}
}

func vncTabBoxW(w, n int) int {
	if n < 1 {
		n = 1
	}
	tw := (w - vncBtnW - 2) / n
	if tw > vncTabW {
		tw = vncTabW
	}
	if tw < 30 {
		tw = 30
	}
	return tw
}

func vncDrawBar(w int) {
	if w < 1 {
		return
	}
	vncSrv.Lock()
	text := vncSrv.urlText
	if vncSrv.focused {
		text = vncSrv.edit + "_"
	}
	pct := fmt.Sprintf("%d%%", int(vncSrv.zoom*100+0.5))
	titles := make([]string, len(vncSrv.tabs))
	for i, t := range vncSrv.tabs {
		titles[i] = vncShortName(t.title)
		if titles[i] == "" {
			titles[i] = vncShortName(t.url)
		}
		if titles[i] == "" {
			titles[i] = "New Tab"
		}
	}
	active := vncSrv.active
	vncSrv.Unlock()
	img := image.NewRGBA(image.Rect(0, 0, w, vncChromeH))
	draw.Draw(img, image.Rect(0, 0, w, vncTabH), image.NewUniform(color.RGBA{0xA0, 0xA0, 0xA0, 0xFF}), image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(0, vncTabH, w, vncChromeH), image.NewUniform(color.RGBA{0xC0, 0xC0, 0xC0, 0xFF}), image.Point{}, draw.Src)
	for x := 0; x < w; x++ {
		img.Set(x, vncChromeH-1, color.Black)
	}
	dr := font.Drawer{Dst: img, Src: image.NewUniform(color.Black), Face: basicfont.Face7x13}
	// tab row
	tw := vncTabBoxW(w, len(titles))
	for i, ti := range titles {
		bx := i * tw
		bg := color.RGBA{0xC0, 0xC0, 0xC0, 0xFF}
		if i == active {
			bg = color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}
		}
		draw.Draw(img, image.Rect(bx+1, 2, bx+tw-1, vncTabH), image.NewUniform(bg), image.Point{}, draw.Src)
		vncBorder(img, bx+1, 2, bx+tw-1, vncTabH, color.Black)
		m := (tw - 12) / 7
		if i == active {
			m = (tw - 22) / 7
		}
		if r := []rune(ti); len(r) > m && m > 0 {
			ti = string(r[:m])
		}
		dr.Dot = fixed.P(bx+5, 15)
		dr.DrawString(ti)
		if i == active { // close zone
			dr.Dot = fixed.P(bx+tw-11, 15)
			dr.DrawString("x")
		}
	}
	px := len(titles) * tw
	draw.Draw(img, image.Rect(px+2, 2, px+vncBtnW, vncTabH), image.NewUniform(color.RGBA{0xE0, 0xE0, 0xE0, 0xFF}), image.Point{}, draw.Src)
	vncBorder(img, px+2, 2, px+vncBtnW, vncTabH, color.Black)
	dr.Dot = fixed.P(px+9, 15)
	dr.DrawString("+")
	// controls row
	for i, l := range vncBtnLabels {
		bx := 2 + i*(vncBtnW+2)
		draw.Draw(img, image.Rect(bx, vncTabH+2, bx+vncBtnW, vncChromeH-2), image.NewUniform(color.RGBA{0xE0, 0xE0, 0xE0, 0xFF}), image.Point{}, draw.Src)
		vncBorder(img, bx, vncTabH+2, bx+vncBtnW, vncChromeH-2, color.Black)
		dr.Dot = fixed.P(bx+8, vncTabH+14)
		dr.DrawString(l)
	}
	dr.Dot = fixed.P(vncZoomX, vncTabH+14)
	dr.DrawString(pct)
	if w >= vncURLX+40 { // url field only when it fits, everything else just clips
		draw.Draw(img, image.Rect(vncURLX, vncTabH+2, w-3, vncChromeH-2), image.NewUniform(color.White), image.Point{}, draw.Src)
		vncBorder(img, vncURLX, vncTabH+2, w-3, vncChromeH-2, color.Black)
		if max := (w - 3 - vncURLX - 8) / 7; len(text) > max { // show the tail
			text = text[len(text)-max:]
		}
		dr.Dot = fixed.P(vncURLX+4, vncTabH+14)
		dr.DrawString(text)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		return
	}
	vncSrv.Lock()
	vncSrv.barJpg = buf.Bytes()
	vncSrv.barImg = img
	vncSrv.barW = w
	vncSrv.barSeq++
	for c := range vncSrv.clients {
		vncPoke(c)
	}
	vncSrv.Unlock()
}

// ---- context menu ----

// Chrome's own context menu is a native OS window, so the screencast never sees
// it and right click would otherwise do nothing visible. This one is drawn by
// us into the frame and composited before encoding.
type vncMenu struct {
	open  bool
	x, y  int // top left, in frame coordinates
	items []string
	link  string // href under the pointer when it opened, "" if none
}

const (
	vncMenuRow = 16
	vncMenuPad = 6
)

func (m *vncMenu) size() (w, h int) {
	for _, it := range m.items {
		if n := len(it)*7 + vncMenuPad*2; n > w {
			w = n
		}
	}
	return w, len(m.items)*vncMenuRow + 2
}

// Open at a page position, listing link actions only when there is a link
func vncOpenMenu(x, y int) {
	var href string
	if brCtx() != nil {
		js := fmt.Sprintf(`(function(){var e=document.elementFromPoint(%d,%d);`+
			`if(!e)return "";var a=e.closest("a");return a&&a.href?a.href:""})()`, x, y)
		vncRun(brCtx(), vncActTO, chromedp.Evaluate(js, &href))
	}
	items := []string{"Back", "Forward", "Reload"}
	if href != "" {
		items = append(items, "Open in new tab", "Copy link")
	}
	vncSrv.Lock()
	vncSrv.menu = vncMenu{open: true, x: x, y: y, items: items, link: href}
	for c := range vncSrv.clients {
		vncPoke(c)
	}
	vncSrv.Unlock()
	log.Printf("VNC: context menu at %d,%d link=%q", x, y, href)
}

func vncCloseMenu() {
	vncSrv.Lock()
	was := vncSrv.menu.open
	vncSrv.menu = vncMenu{}
	if was {
		for c := range vncSrv.clients {
			vncPoke(c)
		}
	}
	vncSrv.Unlock()
}

// Returns true when the click was inside the menu and has been dealt with
func vncMenuClick(x, y int) bool {
	vncSrv.Lock()
	m := vncSrv.menu
	vncSrv.Unlock()
	if !m.open {
		return false
	}
	w, h := m.size()
	if x < m.x || x >= m.x+w || y < m.y || y >= m.y+h {
		vncCloseMenu() // a click anywhere else just dismisses it
		return true
	}
	i := (y - m.y - 1) / vncMenuRow
	if i < 0 || i >= len(m.items) {
		vncCloseMenu()
		return true
	}
	log.Printf("VNC: context menu %s", m.items[i])
	vncCloseMenu()
	switch m.items[i] {
	case "Back":
		vncDispatch(vncHistory(-1))
	case "Forward":
		vncDispatch(vncHistory(1))
	case "Reload":
		vncDispatch(vncReload())
	case "Open in new tab":
		go vncOpenTabAt(m.link)
	case "Copy link":
		vncPushCutText(m.link)
	}
	return true
}

func vncOpenTabAt(url string) {
	vncTabMu.Lock()
	i, err := vncAddTab()
	if err != nil {
		vncTabMu.Unlock()
		log.Printf("VNC: open in new tab: %v", err)
		return
	}
	vncActivate(i)
	vncTabMu.Unlock()
	vncDispatch(vncNav(url))
}

// Hand text to every client as its clipboard
func vncPushCutText(s string) {
	if s == "" {
		return
	}
	vncSrv.Lock()
	vncSrv.selection = s
	var cs []*vncClient
	for c := range vncSrv.clients {
		c.sentCut = s
		cs = append(cs, c)
	}
	vncSrv.Unlock()
	msg := vncCutTextMsg(s)
	for _, c := range cs {
		c.wmu.Lock()
		c.conn.SetWriteDeadline(time.Now().Add(vncWriteTO))
		c.conn.Write(msg)
		c.wmu.Unlock()
	}
}

// Paint the menu into a frame that has already been copied for the purpose
func vncDrawMenu(img *image.RGBA, m vncMenu) {
	w, h := m.size()
	b := img.Bounds()
	x0, y0 := m.x, m.y
	if x0+w > b.Dx() { // keep it on screen
		x0 = max(0, b.Dx()-w)
	}
	if y0+h > b.Dy() {
		y0 = max(0, b.Dy()-h)
	}
	r := image.Rect(x0, y0, min(x0+w, b.Dx()), min(y0+h, b.Dy()))
	draw.Draw(img, r, image.NewUniform(color.RGBA{0xE8, 0xE8, 0xE8, 0xFF}), image.Point{}, draw.Src)
	vncBorder(img, r.Min.X, r.Min.Y, r.Max.X, r.Max.Y, color.Black)
	dr := font.Drawer{Dst: img, Src: image.NewUniform(color.Black), Face: basicfont.Face7x13}
	for i, it := range m.items {
		ty := y0 + 1 + i*vncMenuRow
		if ty+vncMenuRow > r.Max.Y {
			break
		}
		if i > 0 { // separator
			for x := r.Min.X + 1; x < r.Max.X-1; x++ {
				img.Set(x, ty, color.RGBA{0xB0, 0xB0, 0xB0, 0xFF})
			}
		}
		dr.Dot = fixed.P(x0+vncMenuPad, ty+12)
		dr.DrawString(it)
	}
}

// Change page zoom: delta of +-0.25, or 0 to reset to 100%
func vncZoom(delta float64) {
	vncSrv.Lock()
	z := vncSrv.zoom + delta
	if delta == 0 {
		z = 1.0
	}
	if z < 0.25 {
		z = 0.25
	}
	if z > 4 {
		z = 4
	}
	changed := z != vncSrv.zoom
	vncSrv.zoom = z
	w := vncSrv.barW
	vncSrv.Unlock()
	if !changed {
		return
	}
	log.Printf("VNC: zoom %v%%", int(z*100+0.5))
	sessMark()
	vncApplyZoom(z)
	vncDrawBar(w)
}

// Set css zoom on the current page and re-register it for future navigations
func vncApplyZoom(z float64) {
	src := fmt.Sprintf("document.documentElement.style.zoom=%g", z)
	vncDispatch(chromedp.ActionFunc(func(cx context.Context) error {
		vncSrv.Lock()
		old := vncSrv.zoomScript
		vncSrv.Unlock()
		if old != "" {
			page.RemoveScriptToEvaluateOnNewDocument(old).Do(cx)
		}
		id, err := page.AddScriptToEvaluateOnNewDocument(src).Do(cx)
		if err == nil {
			vncSrv.Lock()
			vncSrv.zoomScript = id
			vncSrv.Unlock()
		}
		return chromedp.Evaluate(src, nil).Do(cx)
	}))
}

// Chrome reports the url as the tab title until the page sets a real one, so a
// search result page would otherwise become a desktop name hundreds of chars
// long. Anything that looks like a url is cut down to scheme and host.
func vncShortName(s string) string {
	if i := strings.Index(s, "://"); i >= 0 {
		rest := s[i+3:]
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			return s[:i+3+j]
		}
		return s
	}
	host, _, ok := strings.Cut(s, "/") // google.com/search?q=... or data:text/html;...
	if !ok || strings.ContainsAny(s, " \t") {
		return s
	}
	if !strings.ContainsAny(host, ".:") || host != strings.ToLower(host) {
		return s // a real title that happens to have a slash in it
	}
	return host
}

// Update the url shown in the nav bar (from navigation events)
func vncSetURL(u string) {
	vncSrv.Lock()
	vncSrv.urlText = u
	if !vncSrv.focused {
		vncSrv.edit = u
	}
	w := vncSrv.barW
	vncSrv.Unlock()
	if w > 0 {
		vncDrawBar(w)
	}
}

// ---- screencast ----

// Bind a screencast listener to the active tab, at most once per context.
// The flag lives on the tab so it goes away when the tab does.
func vncCastListen() {
	lctx := brCtx() // drop events from tabs that are no longer active
	vncSrv.Lock()
	var t *vncTab
	if vncSrv.active < len(vncSrv.tabs) {
		t = vncSrv.tabs[vncSrv.active]
	}
	seen := t != nil && t.listCtx == lctx
	if t != nil {
		t.listCtx = lctx
	}
	vncSrv.Unlock()
	if seen {
		return
	}
	chromedp.ListenTarget(lctx, func(ev interface{}) {
		// a dialog blocks its own tab whether or not we are looking at it, and
		// the tab stays blocked once we switch back, so answer it either way
		if e, ok := ev.(*page.EventJavascriptDialogOpening); ok {
			go vncDialog(lctx, e)
			return
		}
		if lctx != brCtx() {
			return
		}
		switch e := ev.(type) {
		case *page.EventScreencastFrame:
			// chrome waits for the ack before sending more, so this must not be
			// dropped; one worker handles them instead of a goroutine per frame
			select {
			case vncSrv.ackCh <- vncAck{lctx, e.SessionID}:
			default:
				go vncRun(lctx, vncActTO, page.ScreencastFrameAck(e.SessionID))
			}
			b, err := base64.StdEncoding.DecodeString(e.Data)
			if err != nil {
				return
			}
			vncPublishJPEG(b)
		case *page.EventFrameNavigated:
			if e.Frame.ParentID == "" {
				go vncSetURL(e.Frame.URL)
			}
		}
	})
}

func vncCastStart() {
	if brCtx() == nil { // no browser, as in the protocol tests
		return
	}
	vncSrv.Lock()
	w, h := vncSrv.vw, vncSrv.vh-vncChromeH
	zoom := vncSrv.zoom
	vncSrv.Unlock()
	vncCastListen()
	bc := brCtx()
	var loc string
	err := vncRun(bc, vncTabTO,
		emulation.SetDeviceMetricsOverride(int64(w), int64(h), 1, false),
		chromedp.Location(&loc),
		page.StartScreencast().
			WithFormat(page.ScreencastFormatJpeg).
			WithQuality(*defJpgQual).
			WithMaxWidth(vncMaxDim).
			WithMaxHeight(vncMaxDim).
			WithEveryNthFrame(1),
	)
	if err != nil {
		log.Printf("VNC: start screencast: %v", err)
		return
	}
	// chrome only casts on damage: a viewport change with static content may
	// never produce a frame, so push one screenshot at the new size ourselves
	go func() {
		var shot []byte
		err := vncRun(bc, vncTabTO, chromedp.ActionFunc(func(cx context.Context) error {
			var err error
			shot, err = page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatJpeg).WithQuality(*defJpgQual).Do(cx)
			return err
		}))
		if err == nil && len(shot) > 0 {
			vncPublishJPEG(shot)
		}
	}()
	vncSetURL(loc)
	if zoom != 1 {
		vncApplyZoom(zoom)
	}
	log.Printf("VNC: screencast started at %vx%v zoom %v", w, h, zoom)
}

func vncCastStop() {
	if brCtx() == nil {
		return
	}
	vncRun(brCtx(), vncActTO, page.StopScreencast())
	log.Printf("VNC: screencast stopped")
}

// Called by http capture routines: they need the viewport for themselves.
// Nests, so overlapping http requests don't resume each other's screencast.
func vncSuspend() {
	if *vncAddr == "" {
		return
	}
	vncSrv.Lock()
	vncSrv.suspend++
	first := vncSrv.suspend == 1
	on := vncSrv.casting
	vncSrv.Unlock()
	if first && on {
		vncRun(brCtx(), vncActTO, page.StopScreencast())
	}
}

// Restore viewport and screencast after http capture, emits a fresh frame
func vncResume() {
	if *vncAddr == "" {
		return
	}
	vncSrv.Lock()
	if vncSrv.suspend > 0 {
		vncSrv.suspend--
	}
	last := vncSrv.suspend == 0
	on := vncSrv.casting
	vncSrv.Unlock()
	if last && on {
		vncCastStart()
	}
}

// Called from ctxErr and from the watchdog when a new browser context is
// created. The process behind it is new, so every tab we were tracking died
// with the old one and the list starts over at the one tab we now have.
func vncCtxReset() {
	if *vncAddr == "" {
		return
	}
	vncSrv.Lock()
	vncSrv.tabs = []*vncTab{{ctx: brCtx(), cncl: brCncl(), url: "about:blank"}}
	vncSrv.active = 0
	on := vncSrv.casting
	w := vncSrv.barW
	vncSrv.Unlock()
	if on {
		vncCastStart()
	}
	if w > 0 {
		vncDrawBar(w)
	}
}

// ---- client handling ----

func vncHandle(conn net.Conn) {
	defer conn.Close()
	log.Printf("%s VNC client connected", conn.RemoteAddr())
	conn.SetDeadline(time.Now().Add(vncHandshakeTO))
	if err := vncHandshake(conn); err != nil {
		log.Printf("%s VNC handshake: %v", conn.RemoteAddr(), err)
		return
	}
	conn.SetDeadline(time.Time{}) // a connected client may idle for hours
	c := &vncClient{conn: conn, poke: make(chan struct{}, 1), done: make(chan struct{}), pf: vncDefaultPF}
	vncSrv.Lock()
	vncSrv.clients[c] = true
	c.annW, c.annH = vncSrv.vw, vncSrv.vh
	ncli := len(vncSrv.clients)
	cast := !vncSrv.casting
	vncSrv.casting = true
	vncSrv.Unlock()
	if cast {
		vncCastStart()
	}
	defer func() {
		vncSrv.Lock()
		delete(vncSrv.clients, c)
		stop := len(vncSrv.clients) == 0 && vncSrv.casting
		if stop {
			vncSrv.casting = false
		}
		vncSrv.Unlock()
		close(c.done)
		if stop {
			vncCastStop()
		}
		log.Printf("%s VNC client disconnected", conn.RemoteAddr())
	}()
	log.Printf("%s VNC client ready, %d client(s)", conn.RemoteAddr(), ncli)
	go vncWriter(c)
	vncReadLoop(c)
}

func vncHandshake(conn net.Conn) error {
	fmt.Fprintf(conn, "RFB 003.008\n")
	buf := make([]byte, 12)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	var maj, min int
	if n, err := fmt.Sscanf(string(buf), "RFB %d.%d", &maj, &min); err != nil || n != 2 || maj != 3 {
		return fmt.Errorf("unsupported version %q", buf)
	}
	log.Printf("%s VNC client version 3.%d", conn.RemoteAddr(), min)
	if min >= 7 {
		// None first: every modern viewer picks it and never sees a password
		// prompt. VNC auth is only here for clients that refuse None.
		conn.Write([]byte{2, rfbSecNone, rfbSecVNCAuth})
		one := make([]byte, 1)
		if _, err := io.ReadFull(conn, one); err != nil {
			return err
		}
		switch one[0] {
		case rfbSecNone:
			if min >= 8 {
				conn.Write([]byte{0, 0, 0, 0}) // security result ok
			}
		case rfbSecVNCAuth:
			if err := vncAuth(conn); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported security type %d", one[0])
		}
	} else {
		// 3.3 has the server pick, and there is only one sensible pick: macOS
		// Screen Sharing always answers 3.3 and then hangs forever if offered
		// None, while every real 3.3 viewer implements vnc auth.
		conn.Write([]byte{0, 0, 0, rfbSecVNCAuth})
		if err := vncAuth(conn); err != nil {
			return err
		}
	}
	ci := make([]byte, 1)
	if _, err := io.ReadFull(conn, ci); err != nil { // ClientInit / shared flag
		return fmt.Errorf("client gave up after security none: %w", err)
	}
	if ci[0] == 0 { // exclusive access requested, drop everyone else
		log.Printf("%s VNC client asked for exclusive access", conn.RemoteAddr())
		vncSrv.Lock()
		var others []*vncClient
		for c := range vncSrv.clients {
			others = append(others, c)
		}
		vncSrv.Unlock()
		for _, c := range others {
			c.conn.Close()
		}
	}
	vncSrv.Lock()
	w, h := vncSrv.vw, vncSrv.vh
	vncSrv.Unlock()
	name := "WRP " + version
	var si bytes.Buffer
	binary.Write(&si, binary.BigEndian, uint16(w))
	binary.Write(&si, binary.BigEndian, uint16(h))
	// pixel format: 32 bpp, depth 24, little endian, true color, rgb max 255, shifts 16/8/0
	si.Write([]byte{32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0})
	binary.Write(&si, binary.BigEndian, uint32(len(name)))
	si.WriteString(name)
	_, err := conn.Write(si.Bytes())
	return err
}

// VNC authentication that accepts any password. It exists because macOS Screen
// Sharing answers RFB 3.3 and will not talk to a server offering security type
// None - it just spins on "Connecting". There is deliberately nothing to
// configure and nothing is verified: the listening address is still the only
// access control, exactly as it was when None was all we offered.
//
// The challenge has to be real, or the client has nothing to hash and never
// replies. Screen Sharing also probes once and drops at this point before
// reconnecting with credentials, so an EOF here is normal and not an error
// worth worrying about in the log.
func vncAuth(conn net.Conn) error {
	var chal [16]byte
	if _, err := rand.Read(chal[:]); err != nil {
		return err
	}
	if _, err := conn.Write(chal[:]); err != nil {
		return err
	}
	if _, err := io.ReadFull(conn, make([]byte, 16)); err != nil {
		return fmt.Errorf("challenge response: %w", err)
	}
	_, err := conn.Write([]byte{0, 0, 0, 0}) // accepted, whatever it was
	return err
}

func vncReadLoop(c *vncClient) {
	for {
		t := make([]byte, 1)
		if _, err := io.ReadFull(c.conn, t); err != nil {
			return
		}
		switch t[0] {
		case 0: // SetPixelFormat
			b := make([]byte, 19)
			if _, err := io.ReadFull(c.conn, b); err != nil {
				return
			}
			pf := vncPixFmt{
				bpp: b[3], bigEnd: b[5] != 0,
				rMax: binary.BigEndian.Uint16(b[7:]), gMax: binary.BigEndian.Uint16(b[9:]), bMax: binary.BigEndian.Uint16(b[11:]),
				rSh: b[13], gSh: b[14], bSh: b[15],
			}
			depth := b[4]
			if pf.bpp != 8 && pf.bpp != 16 && pf.bpp != 32 {
				// rfb only defines 8, 16 and 32; anything else would desync
				// every pixel we write, so keep what we already had
				log.Printf("%s VNC client asked for %d bpp which rfb does not define, ignoring", c.conn.RemoteAddr(), pf.bpp)
				break
			}
			indexed := b[6] == 0
			if indexed && pf.bpp != 8 {
				log.Printf("%s VNC client wants a %d bpp colormap, too large to drive, staying true color", c.conn.RemoteAddr(), pf.bpp)
				indexed = false
			}
			palSize := 0
			if indexed { // colormap client: we drive the palette adaptively
				palSize = min(1<<min(depth, 8), int(defGeom.c), 256)
				if palSize < 2 {
					palSize = 2
				}
				pf = vncBGR233 // only bpp and byte width matter for indices
				log.Printf("%s VNC client wants a colormap at depth %d, using %d adaptive colors", c.conn.RemoteAddr(), depth, palSize)
			}
			vncSrv.Lock()
			c.pf = pf
			c.indexed = indexed
			c.palSize = palSize
			c.lut = nil
			c.palVer = 0
			c.resetEnc = true // pixel size changed, resend in the new format
			vncSrv.Unlock()
			if pf.bpp == 8 && !indexed {
				log.Printf("%s VNC client at 8 bpp true color, dithering into its own format", c.conn.RemoteAddr())
			}
		case 2: // SetEncodings
			if !vncSetEncodings(c) {
				return
			}
		case 3: // FramebufferUpdateRequest
			b := make([]byte, 9)
			if _, err := io.ReadFull(c.conn, b); err != nil {
				return
			}
			vncSrv.Lock()
			c.pending = true
			if b[0] == 0 { // non incremental - force full resend
				c.sentSeq = 0
				c.sentBar = 0
				c.forceFull = true
			}
			vncSrv.Unlock()
			vncPoke(c)
		case 4: // KeyEvent
			b := make([]byte, 7)
			if _, err := io.ReadFull(c.conn, b); err != nil {
				return
			}
			vncKeyEvent(c, b[0] != 0, binary.BigEndian.Uint32(b[3:]))
		case 5: // PointerEvent
			b := make([]byte, 5)
			if _, err := io.ReadFull(c.conn, b); err != nil {
				return
			}
			vncPointerEvent(c, b[0], binary.BigEndian.Uint16(b[1:]), binary.BigEndian.Uint16(b[3:]))
		case 6: // ClientCutText
			b := make([]byte, 7)
			if _, err := io.ReadFull(c.conn, b); err != nil {
				return
			}
			n := int64(binary.BigEndian.Uint32(b[3:]))
			if n > vncMaxCut { // keep the rest on the wire but don't buffer it
				if _, err := io.CopyN(io.Discard, c.conn, n); err != nil {
					return
				}
				log.Printf("%s VNC cut text of %d bytes ignored, over limit", c.conn.RemoteAddr(), n)
				break
			}
			txt := make([]byte, n)
			if _, err := io.ReadFull(c.conn, txt); err != nil {
				return
			}
			vncSrv.Lock()
			vncSrv.clipboard = string(txt) // latin-1 on the wire, per the spec
			vncSrv.Unlock()
		case 150: // EnableContinuousUpdates
			b := make([]byte, 9)
			if _, err := io.ReadFull(c.conn, b); err != nil {
				return
			}
			vncSrv.Lock()
			c.contUpd = b[0] != 0
			c.pending = c.pending || c.contUpd
			vncSrv.Unlock()
			log.Printf("%s VNC continuous updates %v", c.conn.RemoteAddr(), b[0] != 0)
			if b[0] == 0 {
				c.wmu.Lock()
				c.conn.SetWriteDeadline(time.Now().Add(vncWriteTO))
				c.conn.Write([]byte{150})
				c.wmu.Unlock()
			}
			vncPoke(c)
		case 248: // ClientFence
			if !vncFence(c) {
				return
			}
		case 255: // QEMU client message
			b := make([]byte, 11)
			if _, err := io.ReadFull(c.conn, b); err != nil {
				return
			}
			if b[0] != 0 { // only the extended key event is defined
				break
			}
			vncQemuKey(c, binary.BigEndian.Uint16(b[1:]) != 0,
				binary.BigEndian.Uint32(b[3:]), binary.BigEndian.Uint32(b[7:]))
		case 251: // SetDesktopSize
			if !vncSetDesktopSize(c) {
				return
			}
		default:
			log.Printf("%s VNC unknown client message type %d, disconnecting", c.conn.RemoteAddr(), t[0])
			return
		}
	}
}

func vncSetEncodings(c *vncClient) bool {
	b := make([]byte, 3)
	if _, err := io.ReadFull(c.conn, b); err != nil {
		return false
	}
	n := binary.BigEndian.Uint16(b[1:])
	encs := make([]byte, 4*int(n))
	if _, err := io.ReadFull(c.conn, encs); err != nil {
		return false
	}
	var tight, zlb, hex, cpr, ext, desk, cur, dnm, fnc, cup, qmk bool
	qual := 0
	for i := 0; i < len(encs); i += 4 {
		e := int32(binary.BigEndian.Uint32(encs[i:]))
		switch e {
		case rfbEncodingTight:
			tight = true
		case rfbEncodingZlib:
			zlb = true
		case rfbEncodingCopyRect:
			cpr = true
		case rfbEncodingHextile:
			hex = true
		case rfbEncodingExtDesk:
			ext = true
		case rfbEncodingDesk:
			desk = true
		case rfbEncodingCursor:
			cur = true
		case rfbEncodingDeskName:
			dnm = true
		case rfbEncodingFence:
			fnc = true
		case rfbEncodingContUpd:
			cup = true
		case rfbEncodingQemuKey:
			qmk = true
		}
		if e >= -32 && e <= -23 { // jpeg quality level 0..9
			qual = int(e+32)*10 + 10
		}
	}
	// best first: tight, then zlib, then hextile, then raw which everybody has
	enc := encRaw
	switch {
	case tight:
		enc = encTight
	case zlb:
		enc = encZlib
	case hex:
		enc = encHextile
	}
	vncSrv.Lock()
	// TigerVNC only sends SetDesktopSize after it saw an ExtendedDesktopSize from us
	c.extInit = ext && !c.extDesk
	c.extDesk = ext
	c.deskSize = desk
	c.needCursor = cur
	c.hasDeskNm = dnm
	c.hasCopyR = cpr
	c.hasFence = fnc
	c.hasContUpd = cup
	c.jpegQual = qual
	c.enc = enc
	c.resetEnc = true // writer drops its zlib stream and resends everything
	c.sentSeq = 0
	c.sentBar = 0
	vncSrv.Unlock()
	// the way to advertise continuous update support is to send an
	// EndOfContinuousUpdates before the client has enabled anything
	if cup {
		c.wmu.Lock()
		c.conn.SetWriteDeadline(time.Now().Add(vncWriteTO))
		c.conn.Write([]byte{150})
		c.wmu.Unlock()
	}
	vncPoke(c)
	log.Printf("%s VNC client encodings: %v, copyrect=%v, resize ext=%v basic=%v, cursor=%v, jpegqual=%v, fence=%v contupd=%v qemukeys=%v",
		c.conn.RemoteAddr(), enc, cpr, ext, desk, cur, qual, fnc, cup, qmk)
	return true
}

// Fence: the client either asks us to echo one back, or is echoing ours. Our
// own fences are how we tell it has finished drawing the last update.
func vncFence(c *vncClient) bool {
	b := make([]byte, 8)
	if _, err := io.ReadFull(c.conn, b); err != nil {
		return false
	}
	flags := binary.BigEndian.Uint32(b[3:])
	payload := make([]byte, b[7])
	if _, err := io.ReadFull(c.conn, payload); err != nil {
		return false
	}
	if flags&rfbFenceRequest == 0 { // an echo of ours: it has caught up
		vncSrv.Lock()
		c.fencePend = false
		if !c.fenceSent.IsZero() {
			c.adaptQuality(time.Since(c.fenceSent))
		}
		vncSrv.Unlock()
		vncPoke(c)
		return true
	}
	c.wmu.Lock()
	c.conn.SetWriteDeadline(time.Now().Add(vncWriteTO))
	_, err := c.conn.Write(vncFenceMsg(flags&^rfbFenceRequest, payload))
	c.wmu.Unlock()
	return err == nil
}

func vncFenceMsg(flags uint32, payload []byte) []byte {
	var b bytes.Buffer
	b.Write([]byte{248, 0, 0, 0})
	binary.Write(&b, binary.BigEndian, flags)
	b.WriteByte(byte(len(payload)))
	b.Write(payload)
	return b.Bytes()
}

// TigerVNC remote resize: viewport follows the client window size.
//
// The ack is not sent from here. It carries the new framebuffer size, and this
// runs on the read goroutine: acking inline can shrink the client's framebuffer
// after the writer has picked a frame but before it puts the rects on the wire,
// and the client then sees rects wider than the buffer it just resized ("Rect
// too big"). The writer owns every size change, so a size only ever reaches the
// client in the same write as rects that fit it.
func vncSetDesktopSize(c *vncClient) bool {
	b := make([]byte, 7)
	if _, err := io.ReadFull(c.conn, b); err != nil {
		return false
	}
	w := vncClamp(int(binary.BigEndian.Uint16(b[1:])))
	h := vncClamp(int(binary.BigEndian.Uint16(b[3:])))
	if h < vncMinDim+vncChromeH {
		h = vncMinDim + vncChromeH
	}
	if _, err := io.CopyN(io.Discard, c.conn, int64(b[5])*16); err != nil { // screen layouts
		return false
	}
	log.Printf("%s VNC client requests desktop size %vx%v", c.conn.RemoteAddr(), w, h)
	sessMark()
	vncSrv.Lock()
	vncSrv.vw, vncSrv.vh = w, h
	c.ackW, c.ackH = w, h
	c.sentSeq = 0 // full frame after resize
	c.sentBar = 0
	on := vncSrv.casting
	vncSrv.Unlock()
	vncPoke(c)
	if on {
		vncCastStart() // apply new viewport, emits fresh frame
	}
	return true
}

// ---- tabs ----

// Serializes tab list changes and active tab switches
var vncTabMu sync.Mutex

// Make tab i active: swap the active browser context, move the screencast.
// Callers hold vncTabMu.
//
// Takes browserMu as well, so a tab cannot change under an http capture that is
// midway between navigating and reading the page back.
func vncActivate(i int) {
	browserMu.Lock()
	defer browserMu.Unlock()
	vncSrv.Lock()
	if i < 0 || i >= len(vncSrv.tabs) {
		vncSrv.Unlock()
		return
	}
	if vncSrv.active >= len(vncSrv.tabs) {
		vncSrv.active = len(vncSrv.tabs) - 1
	}
	old := vncSrv.tabs[vncSrv.active]
	nt := vncSrv.tabs[i]
	vncSrv.active = i
	sessMark()
	on := vncSrv.casting
	w := vncSrv.barW
	vncSrv.Unlock()
	if on && old != nt {
		vncRun(old.ctx, vncActTO, page.StopScreencast())
	}
	setBrCtx(nt.ctx, nt.cncl)
	if on {
		vncCastStart()
	}
	vncDrawBar(w)
}

func vncSwitchTab(i int) {
	vncTabMu.Lock()
	defer vncTabMu.Unlock()
	log.Printf("VNC: switch to tab %d", i)
	vncActivate(i)
}

// Open a fresh tab at the start page and make it active
func vncNewTab() {
	vncTabMu.Lock()
	i, err := vncAddTab()
	if err != nil {
		vncTabMu.Unlock()
		log.Printf("VNC: new tab: %v", err)
		return
	}
	log.Printf("VNC: new tab %d", i)
	vncActivate(i)
	vncTabMu.Unlock()
	if *vncPage != "" && *vncPage != "about:blank" {
		vncDispatch(vncNav(*vncPage))
	}
}

// Callers hold vncTabMu. Create the target in the running browser explicitly:
// headless chrome/brave refuse a plain createTarget ("no browser is open"),
// but accept it as a new window, which headless has no real windows anyway.
func vncAddTab() (int, error) {
	var tid target.ID
	err := vncRun(brCtx(), vncTabTO, chromedp.ActionFunc(func(cx context.Context) error {
		id, err := target.CreateTarget("about:blank").WithNewWindow(true).Do(cx)
		tid = id
		return err
	}))
	if err != nil {
		return 0, err
	}
	nctx, ncncl := chromedp.NewContext(brRoot(), chromedp.WithTargetID(tid))
	if err := vncRun(nctx, vncTabTO, vncNav("about:blank")); err != nil {
		ncncl()
		return 0, err
	}
	selPopupInstall(nctx)
	t := &vncTab{ctx: nctx, cncl: ncncl, id: tid, url: "about:blank"}
	vncSrv.Lock()
	vncSrv.tabs = append(vncSrv.tabs, t)
	i := len(vncSrv.tabs) - 1
	vncSrv.Unlock()
	return i, nil
}

func vncCloseTab(i int) {
	vncTabMu.Lock()
	defer vncTabMu.Unlock()
	vncSrv.Lock()
	if i < 0 || i >= len(vncSrv.tabs) {
		vncSrv.Unlock()
		return
	}
	t := vncSrv.tabs[i]
	last := len(vncSrv.tabs) == 1
	vncSrv.Unlock()
	log.Printf("VNC: close tab %d (%s)", i, t.url)
	if last { // always keep one tab alive
		if _, err := vncAddTab(); err != nil {
			log.Printf("VNC: close tab: %v", err)
			return
		}
	}
	vncSrv.Lock()
	vncSrv.tabs = append(vncSrv.tabs[:i], vncSrv.tabs[i+1:]...)
	a := vncSrv.active
	if a >= i && a > 0 {
		a--
	}
	vncSrv.active = a
	vncSrv.Unlock()
	vncRun(t.ctx, vncTabTO, page.Close())
	vncDropTab(t)
	vncActivate(a)
}

// Release a tab's chromedp context. Every tab hangs off brRoot() so this only
// tears down that one tab, but cancelling the root itself would kill the
// browser, so the first tab keeps its context until shutdown.
func vncDropTab(t *vncTab) {
	if t.cncl == nil || t.ctx == brRoot() {
		return
	}
	t.cncl()
}

func vncTabClick(x int) {
	vncSrv.Lock()
	n := len(vncSrv.tabs)
	active := vncSrv.active
	w := vncSrv.barW
	vncSrv.Unlock()
	if n == 0 || w == 0 {
		return
	}
	tw := vncTabBoxW(w, n)
	if x >= n*tw { // the + button
		if x < n*tw+vncBtnW+2 {
			go vncNewTab()
		}
		return
	}
	i := x / tw
	if i == active && x > (i+1)*tw-14 {
		go vncCloseTab(i)
		return
	}
	if i != active {
		go vncSwitchTab(i)
	}
}

// Watch browser targets: adopt tabs opened by pages (window.open, target=_blank),
// refresh titles for the tab bar, reap externally closed tabs
// Reconcile the tab list when chrome says something changed, with a slow tick
// as a backstop in case an event is ever missed.
func vncTabPoller() {
	for {
		select {
		case <-vncSrv.syncCh:
		case <-time.After(vncReconcile):
		}
		vncSrv.Lock()
		on := vncSrv.casting
		vncSrv.Unlock()
		vncCheckBrowser() // the tick doubles as the browser watchdog
		if on {
			vncTabSync()
		}
	}
}

// Ask chrome to report target lifecycle instead of us listing them every second
func vncWatchTargets() {
	root := brRoot()
	chromedp.ListenBrowser(root, func(ev interface{}) {
		switch ev.(type) {
		case *target.EventTargetCreated, *target.EventTargetDestroyed,
			*target.EventTargetInfoChanged, *target.EventTargetCrashed:
			select {
			case vncSrv.syncCh <- struct{}{}:
			default: // a sync is already queued, it will pick this up too
			}
		}
	})
	if err := vncRun(root, vncTabTO, target.SetDiscoverTargets(true)); err != nil {
		log.Printf("VNC: target discovery unavailable, falling back to polling: %v", err)
	}
}

// The page selection has no cdp event, so it stays on a timer
func vncSelPoller() {
	for range time.Tick(time.Second) {
		vncSrv.Lock()
		on := vncSrv.casting
		vncSrv.Unlock()
		if on {
			vncSyncSelection()
		}
	}
}

// Push the page's text selection to clients as ServerCutText, so selecting in
// the browser makes the text pasteable locally. Chrome has no clipboard in
// headless mode, so the selection is the closest useful thing.
func vncSyncSelection() {
	var sel string
	err := vncRun(brCtx(), vncActTO, chromedp.Evaluate(`(document.getSelection()||'').toString().slice(0,65536)`, &sel))
	if err != nil {
		return
	}
	if sel == "" {
		return
	}
	vncSrv.Lock()
	vncSrv.selection = sel
	// tracked per client, so one that connects later still gets the current
	// selection instead of waiting for it to change again
	var cs []*vncClient
	for c := range vncSrv.clients {
		if c.sentCut != sel {
			c.sentCut = sel
			cs = append(cs, c)
		}
	}
	vncSrv.Unlock()
	if len(cs) == 0 {
		return
	}
	msg := vncCutTextMsg(sel)
	for _, c := range cs {
		c.wmu.Lock()
		c.conn.SetWriteDeadline(time.Now().Add(vncWriteTO))
		_, err := c.conn.Write(msg)
		c.wmu.Unlock()
		if err != nil {
			c.conn.Close()
		}
	}
}

// ServerCutText: latin-1 on the wire, so anything else degrades to '?'
func vncCutTextMsg(s string) []byte {
	var b bytes.Buffer
	b.Write([]byte{3, 0, 0, 0})
	l1 := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xFF {
			r = '?'
		}
		l1 = append(l1, byte(r))
	}
	binary.Write(&b, binary.BigEndian, uint32(len(l1)))
	b.Write(l1)
	return b.Bytes()
}

func vncTabSync() {
	vncTabMu.Lock()
	defer vncTabMu.Unlock()
	infos, err := vncTargets()
	if err != nil {
		return
	}
	vncSrv.Lock()
	for _, t := range vncSrv.tabs { // resolve target ids lazily
		if t.id == "" {
			if tg := chromedp.FromContext(t.ctx).Target; tg != nil {
				t.id = tg.TargetID
			}
		}
	}
	known := make(map[target.ID]*vncTab)
	for _, t := range vncSrv.tabs {
		if t.id != "" {
			known[t.id] = t
		}
	}
	alive := make(map[target.ID]bool)
	changed := false
	var adopt []*target.Info
	var reaped []*vncTab
	for _, ti := range infos {
		if ti.Type != "page" {
			continue
		}
		alive[ti.TargetID] = true
		if t, ok := known[ti.TargetID]; ok {
			if t.title != ti.Title || t.url != ti.URL {
				t.title, t.url = ti.Title, ti.URL
				changed = true
			}
			continue
		}
		adopt = append(adopt, ti)
	}
	for i := 0; i < len(vncSrv.tabs) && len(vncSrv.tabs) > 1; { // reap closed tabs
		t := vncSrv.tabs[i]
		if t.id == "" || alive[t.id] {
			i++
			continue
		}
		log.Printf("VNC: tab %d (%s) is gone, removing", i, t.url)
		reaped = append(reaped, t)
		vncSrv.tabs = append(vncSrv.tabs[:i], vncSrv.tabs[i+1:]...)
		if vncSrv.active >= len(vncSrv.tabs) {
			vncSrv.active = len(vncSrv.tabs) - 1
		} else if vncSrv.active > i {
			vncSrv.active--
		}
		changed = true
	}
	act := vncSrv.tabs[vncSrv.active]
	ai := vncSrv.active
	w := vncSrv.barW
	if nm := vncShortName(act.title); nm != "" && nm != vncSrv.deskName { // viewer title bar
		vncSrv.deskName = nm
		for c := range vncSrv.clients {
			vncPoke(c)
		}
	}
	vncSrv.Unlock()
	for _, t := range reaped {
		vncDropTab(t)
	}
	if brCtx() != act.ctx { // the active tab was reaped
		vncActivate(ai)
	}
	for _, ti := range adopt {
		nctx, ncncl := chromedp.NewContext(brRoot(), chromedp.WithTargetID(ti.TargetID))
		selPopupInstall(nctx)
		t := &vncTab{ctx: nctx, cncl: ncncl, id: ti.TargetID, title: ti.Title, url: ti.URL}
		vncSrv.Lock()
		vncSrv.tabs = append(vncSrv.tabs, t)
		i := len(vncSrv.tabs) - 1
		vncSrv.Unlock()
		changed = true
		log.Printf("VNC: adopted tab %d (%s)", i, ti.URL)
		if ti.OpenerID != "" { // the page opened it, bring to front like a browser would
			vncActivate(i)
		}
	}
	if changed {
		sessMark()
		vncDrawBar(w)
	}
}

// ---- browser calls ----

// Every cdp call the VNC path makes goes through here, because none of them
// have a deadline of their own. Chrome answers input dispatch from the
// renderer, so a modal dialog or a script stuck in a loop leaves the call
// outstanding for as long as the page feels like it, and the input worker is a
// single goroutine: one call that never returns and the session goes deaf to
// every click and keystroke behind it, while the screencast keeps running and
// makes it look alive.
func vncRun(ctx context.Context, to time.Duration, acts ...chromedp.Action) error {
	if ctx == nil {
		return errors.New("no browser")
	}
	if err := ctx.Err(); err != nil {
		return err // dead context, running on it would allocate a browser again
	}
	// chromedp ties a tab to whichever context first runs on it, so a deadline
	// must never be the one to open it: the tab would go down with the timer.
	// An empty Run does the opening, on the context that owns the tab.
	if c := chromedp.FromContext(ctx); c == nil || c.Target == nil {
		if err := chromedp.Run(ctx); err != nil {
			return err
		}
	}
	tctx, cncl := context.WithTimeout(ctx, to)
	defer cncl()
	return chromedp.Run(tctx, acts...)
}

// chromedp's navigation actions do not return until the load event fires, and
// a page holding one unfinished subresource - a tracker pixel, a stalled ad
// frame, a streaming endpoint - never fires it. Chrome commits the navigation
// either way and the url bar follows frameNavigated, so waiting buys the VNC
// path nothing and costs it everything.
func vncNav(u string) chromedp.Action {
	return chromedp.ActionFunc(func(cx context.Context) error {
		_, _, errText, _, err := page.Navigate(u).Do(cx)
		if err != nil {
			return err
		}
		if errText != "" {
			return fmt.Errorf("page load error %s", errText)
		}
		return nil
	})
}

func vncTargets() ([]*target.Info, error) {
	ctx := brCtx()
	if ctx == nil {
		return nil, errors.New("no browser")
	}
	tctx, cncl := context.WithTimeout(ctx, vncTabTO)
	defer cncl()
	return chromedp.Targets(tctx)
}

func vncReload() chromedp.Action {
	return chromedp.ActionFunc(func(cx context.Context) error {
		return page.Reload().Do(cx)
	})
}

// Step through session history, d is -1 for back and 1 for forward
func vncHistory(d int64) chromedp.Action {
	return chromedp.ActionFunc(func(cx context.Context) error {
		cur, ents, err := page.GetNavigationHistory().Do(cx)
		if err != nil {
			return err
		}
		i := cur + d
		if i < 0 || i >= int64(len(ents)) {
			return nil // end of the history, not an error worth logging
		}
		return page.NavigateToHistoryEntry(ents[i].ID).Do(cx)
	})
}

// A dialog blocks its renderer until something answers it, and headless has
// nobody to show it to, so an unanswered alert takes the tab out entirely:
// every later input dispatch to it hangs. Accept what only reports something,
// refuse what asks for a decision we can't put to the viewer.
func vncDialog(ctx context.Context, e *page.EventJavascriptDialogOpening) {
	ok := e.Type == page.DialogTypeAlert || e.Type == page.DialogTypeBeforeunload
	log.Printf("VNC: %s dialog %q, answering %v", e.Type, e.Message, ok)
	err := vncRun(ctx, vncActTO, chromedp.ActionFunc(func(cx context.Context) error {
		return page.HandleJavaScriptDialog(ok).Do(cx)
	}))
	if err != nil {
		log.Printf("VNC: dialog: %v", err)
	}
}

// ---- browser recovery ----

var vncRestartMu sync.Mutex

// When the browser process goes away chromedp cancels every context hanging
// off it, so from then on every call fails instantly and the session never
// recovers on its own. Called from the paths that talk to chrome, so a viewer
// that keeps clicking gets a working browser back instead of a dead one.
func vncCheckBrowser() {
	root := brRoot()
	if root == nil || root.Err() == nil {
		return
	}
	vncRestartMu.Lock()
	defer vncRestartMu.Unlock()
	if r := brRoot(); r != nil && r.Err() == nil {
		return // another goroutine got here first
	}
	log.Printf("VNC: lost the browser (%v), starting a new one", root.Err())
	nctx, ncncl := chromedp.NewContext(actx)
	setBrRoot(nctx, ncncl)
	if err := vncRun(nctx, vncTabTO, vncNav("about:blank")); err != nil {
		// cancel it: a context whose browser never started panics the
		// allocator on the next try, and a dead one is what makes the
		// watchdog come back here with a fresh context instead
		ncncl()
		log.Printf("VNC: browser restart failed: %v", err)
		return
	}
	setupDownloads()
	selPopupInstall(nctx)
	if *vncAddr != "" {
		vncWatchTargets() // the old listener died with the old browser
	}
	vncCtxReset()
	log.Printf("Browser restarted")
}

// ---- input ----

func vncDispatch(a chromedp.Action) {
	select {
	case vncSrv.inputCh <- a:
	default: // browser can't keep up, drop
	}
}

func vncCoalesce(f func(*vncCoal)) {
	vncSrv.Lock()
	f(&vncSrv.coal)
	vncSrv.Unlock()
	select {
	case vncSrv.coalCh <- struct{}{}:
	default: // already signalled, the worker will see the newer state
	}
}

// Replace the pending pointer position rather than queueing another one
func vncDispatchMove(a chromedp.Action) {
	vncCoalesce(func(c *vncCoal) { c.move = a })
}

// Add to the pending wheel rather than queueing another one. Position comes
// from the newest event; chrome only needs somewhere sane to apply the scroll.
func vncDispatchWheel(x, y, dx, dy float64, mod input.Modifier) {
	vncCoalesce(func(c *vncCoal) {
		c.whl, c.wx, c.wy, c.wmod = true, x, y, mod
		c.wdx, c.wdy = c.wdx+dx, c.wdy+dy
	})
}

// Take whatever pointer state accumulated, wheel before move so the pointer
// ends up where the viewer last put it
func vncTakeCoal() []chromedp.Action {
	vncSrv.Lock()
	c := vncSrv.coal
	vncSrv.coal = vncCoal{}
	vncSrv.Unlock()
	var out []chromedp.Action
	if c.whl {
		out = append(out, input.DispatchMouseEvent(input.MouseWheel, c.wx, c.wy).
			WithDeltaX(c.wdx).WithDeltaY(c.wdy).WithModifiers(c.wmod))
	}
	if c.move != nil {
		out = append(out, c.move)
	}
	return out
}

var vncKeysyms = map[uint32]string{
	0xFF08: kb.Backspace, 0xFF09: kb.Tab, 0xFF0D: kb.Enter, 0xFF8D: kb.Enter,
	0xFF1B: kb.Escape, 0xFFFF: kb.Delete, 0xFF63: kb.Insert,
	0xFF51: kb.ArrowLeft, 0xFF52: kb.ArrowUp, 0xFF53: kb.ArrowRight, 0xFF54: kb.ArrowDown,
	0xFF55: kb.PageUp, 0xFF56: kb.PageDown, 0xFF50: kb.Home, 0xFF57: kb.End,
	0xFF13: kb.Pause, 0xFF14: kb.ScrollLock, 0xFF61: kb.PrintScreen, 0xFF7F: kb.NumLock,
	0xFFE5: kb.CapsLock,
	// function keys
	0xFFBE: kb.F1, 0xFFBF: kb.F2, 0xFFC0: kb.F3, 0xFFC1: kb.F4,
	0xFFC2: kb.F5, 0xFFC3: kb.F6, 0xFFC4: kb.F7, 0xFFC5: kb.F8,
	0xFFC6: kb.F9, 0xFFC7: kb.F10, 0xFFC8: kb.F11, 0xFFC9: kb.F12,
	// keypad, both the navigation names and the digits
	0xFF95: kb.Home, 0xFF96: kb.ArrowLeft, 0xFF97: kb.ArrowUp, 0xFF98: kb.ArrowRight,
	0xFF99: kb.ArrowDown, 0xFF9A: kb.PageUp, 0xFF9B: kb.PageDown, 0xFF9C: kb.End,
	0xFF9E: kb.Insert, 0xFF9F: kb.Delete, 0xFF80: " ", 0xFF89: kb.Tab,
	0xFFAA: "*", 0xFFAB: "+", 0xFFAD: "-", 0xFFAE: ".", 0xFFAF: "/",
	0xFFB0: "0", 0xFFB1: "1", 0xFFB2: "2", 0xFFB3: "3", 0xFFB4: "4",
	0xFFB5: "5", 0xFFB6: "6", 0xFFB7: "7", 0xFFB8: "8", 0xFFB9: "9",
}

// XT set 1 scancode to DOM code, with extended (0xe0 prefixed) keys carrying
// the 0x80 bit as the QEMU extended key event defines them. This is the whole
// point of that extension: the physical key, independent of the layout the
// client has loaded.
var vncScanCodes = map[uint32]string{
	0x01: "Escape", 0x02: "Digit1", 0x03: "Digit2", 0x04: "Digit3", 0x05: "Digit4",
	0x06: "Digit5", 0x07: "Digit6", 0x08: "Digit7", 0x09: "Digit8", 0x0a: "Digit9",
	0x0b: "Digit0", 0x0c: "Minus", 0x0d: "Equal", 0x0e: "Backspace", 0x0f: "Tab",
	0x10: "KeyQ", 0x11: "KeyW", 0x12: "KeyE", 0x13: "KeyR", 0x14: "KeyT",
	0x15: "KeyY", 0x16: "KeyU", 0x17: "KeyI", 0x18: "KeyO", 0x19: "KeyP",
	0x1a: "BracketLeft", 0x1b: "BracketRight", 0x1c: "Enter", 0x1d: "ControlLeft",
	0x1e: "KeyA", 0x1f: "KeyS", 0x20: "KeyD", 0x21: "KeyF", 0x22: "KeyG",
	0x23: "KeyH", 0x24: "KeyJ", 0x25: "KeyK", 0x26: "KeyL", 0x27: "Semicolon",
	0x28: "Quote", 0x29: "Backquote", 0x2a: "ShiftLeft", 0x2b: "Backslash",
	0x2c: "KeyZ", 0x2d: "KeyX", 0x2e: "KeyC", 0x2f: "KeyV", 0x30: "KeyB",
	0x31: "KeyN", 0x32: "KeyM", 0x33: "Comma", 0x34: "Period", 0x35: "Slash",
	0x36: "ShiftRight", 0x37: "NumpadMultiply", 0x38: "AltLeft", 0x39: "Space",
	0x3a: "CapsLock",
	0x3b: "F1", 0x3c: "F2", 0x3d: "F3", 0x3e: "F4", 0x3f: "F5", 0x40: "F6",
	0x41: "F7", 0x42: "F8", 0x43: "F9", 0x44: "F10", 0x57: "F11", 0x58: "F12",
	0x45: "NumLock", 0x46: "ScrollLock",
	0x47: "Numpad7", 0x48: "Numpad8", 0x49: "Numpad9", 0x4a: "NumpadSubtract",
	0x4b: "Numpad4", 0x4c: "Numpad5", 0x4d: "Numpad6", 0x4e: "NumpadAdd",
	0x4f: "Numpad1", 0x50: "Numpad2", 0x51: "Numpad3", 0x52: "Numpad0",
	0x53: "NumpadDecimal",
	// extended set
	0x9c: "NumpadEnter", 0x9d: "ControlRight", 0xb5: "NumpadDivide", 0xb8: "AltRight",
	0xc7: "Home", 0xc8: "ArrowUp", 0xc9: "PageUp", 0xcb: "ArrowLeft",
	0xcd: "ArrowRight", 0xcf: "End", 0xd0: "ArrowDown", 0xd1: "PageDown",
	0xd2: "Insert", 0xd3: "Delete", 0xdb: "MetaLeft", 0xdc: "MetaRight",
	0xdd: "ContextMenu",
}

// Keysym to the text it produces, or "" for keys with no printable form.
// Latin-1 comes through directly, everything else via the unicode keysym range.
func vncKeyText(ks uint32) string {
	switch {
	case ks >= 0x20 && ks <= 0x7E, ks >= 0xA0 && ks <= 0xFF:
		return string(rune(ks))
	case ks >= 0x01000000 && ks <= 0x0110FFFF: // unicode keysyms
		if r := rune(ks - 0x01000000); r >= 0x20 {
			return string(r)
		}
	}
	return ""
}

func vncKeyEvent(c *vncClient, down bool, ks uint32) {
	if vncModKeysym(c, down, ks) {
		return
	}
	if !down {
		return
	}
	if ks == 0xFF1B { // escape dismisses the context menu before anything else
		vncSrv.Lock()
		open := vncSrv.menu.open
		vncSrv.Unlock()
		if open {
			vncCloseMenu()
			return
		}
	}
	// shortcuts first: ctrl+v has to reach the paste handler even when the url
	// field has focus, otherwise it would just type a "v" into it
	if vncShortcut(c, ks) {
		return
	}
	vncSrv.Lock()
	focused := vncSrv.focused
	vncSrv.Unlock()
	if focused {
		vncEditKey(ks)
		return
	}
	keys := vncKeysyms[ks]
	if keys == "" {
		keys = vncKeyText(ks)
	}
	if keys == "" {
		return
	}
	if c.mods != 0 {
		vncDispatch(chromedp.KeyEvent(keys, chromedp.KeyModifiers(input.Modifier(c.mods))))
		return
	}
	vncDispatch(chromedp.KeyEvent(keys))
}

// QEMU extended key event: the scancode gives us the physical key, so the DOM
// code is right even on a layout we know nothing about, and press and release
// stay separate instead of being fused into one chromedp.KeyEvent.
func vncQemuKey(c *vncClient, down bool, ks, sc uint32) {
	code := vncScanCodes[sc]
	if code == "" { // unknown scancode, the keysym path still works
		vncKeyEvent(c, down, ks)
		return
	}
	if vncModKeysym(c, down, ks) {
		return
	}
	if !down { // releases never trigger our own shortcuts or url editing
		vncQemuDispatch(c, false, ks, code)
		return
	}
	if vncShortcut(c, ks) {
		return
	}
	vncSrv.Lock()
	focused := vncSrv.focused
	vncSrv.Unlock()
	if focused {
		vncEditKey(ks)
		return
	}
	vncQemuDispatch(c, true, ks, code)
}

// Send one half of a key event, taking the text from the keysym and the code
// from the scancode
func vncQemuDispatch(c *vncClient, down bool, ks uint32, code string) {
	keys := vncKeysyms[ks]
	if keys == "" {
		keys = vncKeyText(ks)
	}
	r := []rune(keys)
	if len(r) != 1 {
		return
	}
	mods := input.Modifier(c.mods)
	for _, p := range kb.Encode(r[0]) {
		isDown := p.Type == input.KeyDown || p.Type == input.KeyRawDown
		if isDown != down {
			continue
		}
		p.Code = code
		p.Modifiers = mods
		vncDispatch(chromedp.ActionFunc(func(cx context.Context) error { return p.Do(cx) }))
	}
}

// Track shift/ctrl/alt/meta state. Returns true when the keysym was a modifier.
func vncModKeysym(c *vncClient, down bool, ks uint32) bool {
	var mod int64
	switch ks {
	case 0xFFE1, 0xFFE2:
		mod = int64(input.ModifierShift)
	case 0xFFE3, 0xFFE4:
		mod = int64(input.ModifierCtrl)
	case 0xFFE9, 0xFFEA:
		mod = int64(input.ModifierAlt)
	case 0xFFE7, 0xFFE8, 0xFFEB, 0xFFEC:
		mod = int64(input.ModifierMeta)
	default:
		return false
	}
	if down {
		c.mods |= mod
	} else {
		c.mods &^= mod
	}
	return true
}

// Browser level shortcuts, handled by us rather than passed to the page.
// Returns true when the key was consumed.
func vncShortcut(c *vncClient, ks uint32) bool {
	alt := c.mods&int64(input.ModifierAlt) != 0
	if alt {
		switch ks {
		case 0xFF51: // alt+left, back
			vncDispatch(vncHistory(-1))
			return true
		case 0xFF53: // alt+right, forward
			vncDispatch(vncHistory(1))
			return true
		}
	}
	if c.mods&int64(input.ModifierCtrl) == 0 {
		return false
	}
	switch ks {
	case '+', '=':
		vncZoom(0.25)
	case '-':
		vncZoom(-0.25)
	case '0':
		vncZoom(0)
	case 'l', 'L', 0x0C: // focus the url field, ctrl sends the control code on some clients
		vncFocusURL()
	case 't', 'T', 0x14:
		go vncNewTab()
	case 'w', 'W', 0x17:
		vncSrv.Lock()
		i := vncSrv.active
		vncSrv.Unlock()
		go vncCloseTab(i)
	case 'r', 'R', 0x12:
		vncDispatch(vncReload())
	case 0xFF09, 0x09: // ctrl+tab, next tab
		vncSrv.Lock()
		n, i := len(vncSrv.tabs), vncSrv.active
		vncSrv.Unlock()
		if n > 1 {
			go vncSwitchTab((i + 1) % n)
		}
	case 'v', 'V', 0x16:
		vncPaste()
	default:
		return false
	}
	return true
}

// Put the client's clipboard into the page, or into the url field when it has
// focus. Chrome has no clipboard of its own in headless, so we insert directly.
func vncPaste() {
	vncSrv.Lock()
	txt := vncSrv.clipboard
	focused := vncSrv.focused
	if focused {
		vncSrv.edit += txt
	}
	w := vncSrv.barW
	vncSrv.Unlock()
	if txt == "" {
		return
	}
	if focused {
		vncDrawBar(w)
		return
	}
	vncDispatch(chromedp.ActionFunc(func(cx context.Context) error {
		return input.InsertText(txt).Do(cx)
	}))
}

// Focus clears the field right away, so what you see is what enter will use.
// Escape puts the old url back.
func vncFocusURL() {
	vncSrv.Lock()
	vncSrv.focused = true
	vncSrv.edit = ""
	w := vncSrv.barW
	vncSrv.Unlock()
	vncDrawBar(w)
}

// Keystroke into the url field
func vncEditKey(ks uint32) {
	vncSrv.Lock()
	switch {
	case ks == 0xFF0D || ks == 0xFF8D: // enter - navigate
		u := vncSrv.edit
		if u == "" { // nothing typed, just drop focus
			vncSrv.focused = false
			vncSrv.edit = vncSrv.urlText
			break
		}
		if len(u) > 1 && !strings.HasPrefix(u, "http") {
			u = *searchEng + url.QueryEscape(u)
		}
		vncSrv.focused = false
		vncSrv.urlText = u
		vncSrv.edit = u
		w := vncSrv.barW
		vncSrv.Unlock()
		log.Printf("VNC: navigate to %s", u)
		vncDispatch(vncNav(u))
		vncDrawBar(w)
		return
	case ks == 0xFF1B: // escape - cancel edit
		vncSrv.focused = false
		vncSrv.edit = vncSrv.urlText
	case ks == 0xFF08 || ks == 0xFFFF: // backspace / delete
		if r := []rune(vncSrv.edit); len(r) > 0 {
			vncSrv.edit = string(r[:len(r)-1])
		}
	case (ks >= 0x20 && ks <= 0x7E) || (ks >= 0xA0 && ks <= 0xFF):
		vncSrv.edit += string(rune(ks))
	}
	w := vncSrv.barW
	vncSrv.Unlock()
	vncDrawBar(w)
}

func vncPointerEvent(c *vncClient, mask byte, x, y uint16) {
	diff := mask ^ c.buttons
	c.buttons = mask
	if int(y) < vncChromeH {
		// forward button releases so chrome doesn't end up with a stuck button
		for _, bt := range []struct {
			bit byte
			b   input.MouseButton
		}{{0x01, input.Left}, {0x02, input.Middle}, {0x04, input.Right}} {
			if diff&bt.bit != 0 && mask&bt.bit == 0 {
				vncDispatch(input.DispatchMouseEvent(input.MouseReleased, float64(x), 0).
					WithButton(bt.b).WithClickCount(1))
			}
		}
		if diff&0x01 != 0 && mask&0x01 != 0 { // left press in the ui strip
			if int(y) < vncTabH {
				vncTabClick(int(x))
			} else {
				vncBarClick(int(x))
			}
		}
		return
	}
	fx, fy := float64(x), float64(int(y)-vncChromeH)
	// the menu sits above the page, so it gets first refusal on clicks
	if diff&0x01 != 0 && mask&0x01 != 0 && vncMenuClick(int(x), int(fy)) {
		return
	}
	if diff&0x04 != 0 && mask&0x04 != 0 { // right press opens the menu
		go vncOpenMenu(int(x), int(fy))
		return
	}
	if diff&0x04 != 0 { // and swallow the matching release
		return
	}
	if diff&0x01 != 0 && mask&0x01 != 0 { // click into the page takes focus away
		vncSrv.Lock()
		focused := vncSrv.focused
		vncSrv.focused = false
		w := vncSrv.barW
		vncSrv.Unlock()
		if focused {
			vncDrawBar(w)
		}
	}
	// wheel: buttons 4/5 vertical and 6/7 horizontal, each a press+release pair
	for _, wh := range []struct {
		bit    byte
		dx, dy float64
	}{{0x08, 0, -120}, {0x10, 0, 120}, {0x20, -120, 0}, {0x40, 120, 0}} {
		if diff&wh.bit != 0 && mask&wh.bit != 0 {
			vncDispatchWheel(fx, fy, wh.dx, wh.dy, input.Modifier(c.mods))
		}
	}
	btns := []struct {
		bit byte
		b   input.MouseButton
	}{{0x01, input.Left}, {0x02, input.Middle}, {0x04, input.Right}}
	moved := true
	for _, bt := range btns {
		if diff&bt.bit == 0 {
			continue
		}
		moved = false
		typ := input.MouseReleased
		if mask&bt.bit != 0 {
			typ = input.MousePressed
			c.clicks = c.clickCount(bt.bit, int(x), int(y))
		}
		vncDispatch(input.DispatchMouseEvent(typ, fx, fy).
			WithButton(bt.b).WithClickCount(int64(c.clicks)).WithModifiers(input.Modifier(c.mods)))
	}
	if !moved {
		return
	}
	held := input.None
	for _, bt := range btns {
		if mask&bt.bit != 0 {
			held = bt.b
			break
		}
	}
	vncDispatchMove(input.DispatchMouseEvent(input.MouseMoved, fx, fy).WithButton(held))
}

// Chrome needs a real click count for double click to select a word or for
// dblclick handlers to fire; VNC only gives us button transitions.
func (c *vncClient) clickCount(bit byte, x, y int) int {
	now := time.Now()
	near := abs(x-c.lastClkX) <= 4 && abs(y-c.lastClkY) <= 4
	if bit == c.lastBtn && near && now.Sub(c.lastClk) < vncDblClick {
		c.clicks++
		if c.clicks > 3 {
			c.clicks = 1
		}
	} else {
		c.clicks = 1
	}
	c.lastBtn, c.lastClkX, c.lastClkY, c.lastClk = bit, x, y, now
	return c.clicks
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func vncBarClick(x int) {
	for i := range vncBtnLabels {
		bx := 2 + i*(vncBtnW+2)
		if x < bx || x >= bx+vncBtnW {
			continue
		}
		log.Printf("VNC: nav button %s", vncBtnLabels[i])
		switch i {
		case 0:
			vncDispatch(vncHistory(-1))
		case 1:
			vncDispatch(vncHistory(1))
		case 2:
			vncDispatch(vncReload())
		case 3:
			vncDispatch(chromedp.Stop())
		case 4: // home
			vncDispatch(vncNav(*vncPage))
		case 5:
			vncZoom(-0.25)
		case 6:
			vncZoom(0.25)
		}
		return
	}
	if x >= vncZoomX && x < vncURLX { // click the percent label to reset zoom
		vncZoom(0)
		return
	}
	if x < vncURLX {
		return
	}
	vncFocusURL()
}

// ---- frame delivery ----

func vncWriter(c *vncClient) {
	for {
		select {
		case <-c.done:
			return
		case <-c.poke:
		}
		vncSrv.Lock()
		frame, seq, fw, fh := vncSrv.frame, vncSrv.seq, vncSrv.fw, vncSrv.fh
		bar, barSeq, barW := vncSrv.barJpg, vncSrv.barSeq, vncSrv.barW
		img, tiles, gw, gh := vncSrv.frameImg, vncSrv.frameTiles, vncSrv.fgw, vncSrv.fgh
		samp := vncSrv.frameSamp
		barImg, name, menu := vncSrv.barImg, vncSrv.deskName, vncSrv.menu
		pal := vncSrv.pals[c.palSize]
		if c.resetEnc { // note: the zlib streams deliberately survive this
			c.resetEnc = false
			c.lastCmap = nil
			c.tiles, c.samp, c.copies = nil, nil, 0
		}
		if c.forceFull { // client asked for a full repaint, forget what it has
			c.forceFull = false
			c.tiles, c.samp, c.copies = nil, nil, 0
		}
		nameChanged := c.hasDeskNm && name != "" && name != c.sentName
		// with continuous updates the client stops asking, we just push. The
		// outstanding fence is what stops us pushing faster than it can draw.
		want := c.pending || c.contUpd
		if c.contUpd && c.fencePend {
			want = false
		}
		if !want || frame == nil || (c.sentSeq == seq && c.sentBar == barSeq && !c.extInit && !nameChanged) {
			vncSrv.Unlock()
			continue
		}
		if fw != vncSrv.vw || fh != vncSrv.vh-vncChromeH {
			// stale frame from before a resize: announcing it would snap the
			// client back to the old size, wait for the fresh one instead
			vncSrv.Unlock()
			continue
		}
		resize := fw != c.annW || fh != c.annH-vncChromeH
		announce := resize && (c.extDesk || c.deskSize)
		// this frame is the answer to this client's own SetDesktopSize
		ackReq := c.extDesk && c.ackW == fw && c.ackH == fh+vncChromeH
		if ackReq {
			c.ackW, c.ackH = 0, 0
		}
		if announce {
			c.annW, c.annH = fw, fh+vncChromeH
		}
		fit := resize && !announce
		aw, ah, ext := c.annW, c.annH, c.extDesk
		cursor, pf, indexed := c.needCursor, c.pf, c.indexed
		extInit := c.extInit
		sendBar := bar != nil && (c.sentBar != barSeq || c.sentSeq == 0)
		c.needCursor = false
		c.extInit = false
		c.pending = false
		c.sentSeq = seq
		if sendBar {
			c.sentBar = barSeq
		}
		if nameChanged {
			c.sentName = name
		}
		vncSrv.Unlock()
		if fit { // client can't resize - letterbox into its framebuffer
			if !c.warnedFit {
				c.warnedFit = true
				log.Printf("%s VNC client has no resize support, letterboxing %vx%v into %vx%v", c.conn.RemoteAddr(), fw, fh, aw, ah-vncChromeH)
			}
			// reuse the pad buffer, this runs on every frame for such a client
			c.lbBuf = vncLetterbox(c.lbBuf, img, aw, ah-vncChromeH)
			if c.lbBuf == nil {
				continue
			}
			frame = nil // the passthrough jpeg no longer matches the framebuffer
			img = c.lbBuf
			tiles, samp, gw, gh = vncHashFrame(img)
			fw, fh = aw, ah-vncChromeH
		}
		if img == nil {
			continue
		}
		if menu.open { // overlay the menu, on a copy so the shared frame stays clean
			if img != c.lbBuf {
				c.lbBuf = vncLetterbox(c.lbBuf, img, fw, fh)
				img = c.lbBuf
			}
			vncDrawMenu(img, menu)
			tiles, samp, gw, gh = vncHashFrame(img)
			frame = nil
		}
		var rects [][]byte
		var cmap []byte
		if cursor {
			rects = append(rects, vncRectCursor(pf, indexed))
		}
		if nameChanged {
			rects = append(rects, vncRectDeskName(name))
		}
		if indexed && pal != nil && !bytes.Equal(pal.cmap, c.lastCmap) {
			cmap = pal.cmap
			c.lastCmap = pal.cmap
		}
		if sendBar && barImg != nil && barW == fw {
			rects = append(rects, c.encodeRects(barImg, pal, barImg.Bounds(), 0, 0, bar)...)
		}
		// A copy is matched with a tolerance, so afterwards the client holds a
		// slightly older jpeg generation of the same content and the server's
		// model of it is approximate. Every vncCopyMax copies one exact repaint
		// washes that out, which bounds how long any of it can survive.
		wash := c.copies >= vncCopyMax
		if wash {
			c.copies, c.tiles = 0, nil
		}
		// a scroll moves content the client already has, so hand it a CopyRect
		// and only encode what the move could not account for
		dirty := vncDirtyRects(tiles, c.tiles, gw, gh, fw, fh)
		// letterboxed clients hash their own padded buffer and the menu does not
		// scroll with the page, so neither gets a copy. Detection only runs when
		// most of the frame moved, which is the only thing a scroll looks like.
		if !wash && c.hasCopyR && c.samp != nil && !fit && !menu.open &&
			vncRectsArea(dirty)*100 >= fw*fh*vncScrollHint {
			if dy := vncDetectShift(samp, c.samp); dy != 0 {
				sd := vncShiftDirty(samp, c.samp, dy, fw, fh)
				if vncRectsArea(sd)*100 < fw*fh*vncCopyGain { // has to actually save something
					sy, dsty, ch := dy, 0, fh-dy
					if dy < 0 {
						sy, dsty, ch = 0, -dy, fh+dy
					}
					rects = append(rects, vncRectCopy(0, dsty+vncChromeH, fw, ch, 0, sy+vncChromeH))
					dirty = sd
					frame = nil // the client's framebuffer is no longer a whole frame
					c.copies++
				}
			}
		}
		whole := len(dirty) == 1 && dirty[0].Dx() == fw && dirty[0].Dy() == fh
		for _, r := range dirty {
			var pass []byte
			if whole {
				pass = frame // reuse chrome's jpeg untranscoded when we can
			}
			rects = append(rects, c.encodeRects(img, pal, r, r.Min.X, r.Min.Y+vncChromeH, pass)...)
		}
		c.tiles = append(c.tiles[:0], tiles...)
		c.samp = c.samp.copyOf(samp)
		c.wmu.Lock()
		c.conn.SetWriteDeadline(time.Now().Add(vncWriteTO))
		var err error
		if cmap != nil {
			_, err = c.conn.Write(cmap)
		}
		if err == nil && extInit && !announce && !ackReq {
			err = vncSendExtDeskSize(c.conn, 0, 0, aw, ah)
		}
		// the size goes out ahead of the rects, in the same write, so the client
		// never holds a framebuffer the rects that follow do not fit
		if err == nil && (announce || ackReq) {
			reason := 0
			if ackReq { // reason 1: this client asked for it, result 0: success
				reason = 1
			}
			if ext {
				err = vncSendExtDeskSize(c.conn, reason, 0, fw, fh+vncChromeH)
			} else {
				err = vncSendDeskSize(c.conn, fw, fh+vncChromeH)
			}
		}
		if err == nil {
			err = vncSendUpdate(c.conn, rects)
		}
		// ask the client to tell us when it has drawn this, so the next frame
		// waits for a slow link instead of queueing up behind it
		if err == nil && c.contUpd && c.hasFence {
			vncSrv.Lock()
			c.fencePend = true
			c.fenceSent = time.Now()
			vncSrv.Unlock()
			_, err = c.conn.Write(vncFenceMsg(rfbFenceRequest|rfbFenceBlockBefore, nil))
		}
		c.wmu.Unlock()
		if err != nil {
			log.Printf("%s VNC send: %v", c.conn.RemoteAddr(), err)
			c.conn.Close()
			return
		}
	}
}

// Encode a framebuffer rect, splitting it if the encoding cannot take it whole.
// x,y are the framebuffer coordinates of r.Min.
func (c *vncClient) encodeRects(img *image.RGBA, pal *vncPal, r image.Rectangle, x, y int, jpg []byte) [][]byte {
	if c.enc != encTight || c.pf.bpp < 16 || r.Dx() <= vncTightMax {
		return [][]byte{c.encodeRect(img, pal, r, x, y, jpg)}
	}
	var out [][]byte // a passthrough jpeg covers the whole rect, so it can't be reused
	for x0 := r.Min.X; x0 < r.Max.X; x0 += vncTightMax {
		s := image.Rect(x0, r.Min.Y, min(x0+vncTightMax, r.Max.X), r.Max.Y)
		out = append(out, c.encodeRect(img, pal, s, x+(x0-r.Min.X), y, nil))
	}
	return out
}

// Encode one framebuffer rect in this client's negotiated encoding. jpg, when
// non nil, is chrome's own jpeg for exactly this rect and goes out untranscoded.
func (c *vncClient) encodeRect(img *image.RGBA, pal *vncPal, r image.Rectangle, x, y int, jpg []byte) []byte {
	w, h := r.Dx(), r.Dy()
	if c.enc == encTight && c.pf.bpp >= 16 { // tight jpeg needs 16 or 32 bpp
		if jpg == nil {
			jpg = vncJpegRegion(img, r, c.jpegQuality())
		}
		if jpg != nil {
			return vncRectJpeg(x, y, w, h, jpg)
		}
	}
	pix := c.pixels(img, pal, r)
	switch c.enc {
	case encTight: // low bpp: tight basic, copy filter, through the zlib stream
		return c.rectTightIdx(x, y, pix, w, h)
	case encZlib:
		return c.rectZlib(x, y, pix, w, h)
	case encHextile:
		return c.rectHextile(x, y, pix, w, h)
	}
	return c.rectRaw(x, y, pix, w, h)
}

// Configured jpeg quality, lowered while the link cannot keep up
func (c *vncClient) jpegQuality() int {
	q := int(*defJpgQual)
	if c.jpegQual > 0 {
		q = c.jpegQual
	}
	if c.adaptQual > 0 && c.adaptQual < q {
		return c.adaptQual
	}
	return q
}

// How long the client took to acknowledge the last frame is a decent proxy for
// how much room the link has. Callers hold the vncSrv lock.
func (c *vncClient) adaptQuality(rtt time.Duration) {
	base := int(*defJpgQual)
	if c.jpegQual > 0 {
		base = c.jpegQual
	}
	if c.adaptQual == 0 {
		c.adaptQual = base
	}
	switch {
	case rtt > vncSlowFrame:
		c.adaptQual = max(c.adaptQual-10, vncMinQual)
	case rtt < vncFastFrame:
		c.adaptQual = min(c.adaptQual+5, base)
	}
}

// Pad a frame into a larger framebuffer for clients that cannot resize. dst is
// reused when it already has the right size, this runs once per frame.
func vncLetterbox(dst, img *image.RGBA, w, h int) *image.RGBA {
	if img == nil {
		return nil
	}
	if dst == nil || dst.Bounds().Dx() != w || dst.Bounds().Dy() != h {
		dst = image.NewRGBA(image.Rect(0, 0, w, h))
	}
	draw.Draw(dst, dst.Bounds(), image.NewUniform(vncBgColor()), image.Point{}, draw.Src)
	draw.Draw(dst, img.Bounds(), img, img.Bounds().Min, draw.Src)
	return dst
}

// FramebufferUpdate from pre-built rects
func vncSendUpdate(conn net.Conn, rects [][]byte) error {
	var b bytes.Buffer
	b.Write([]byte{0, 0})
	binary.Write(&b, binary.BigEndian, uint16(len(rects)))
	for _, r := range rects {
		b.Write(r)
	}
	_, err := conn.Write(b.Bytes())
	return err
}

func vncCompact(l int, b *bytes.Buffer) {
	switch {
	case l < 1<<7:
		b.WriteByte(byte(l))
	case l < 1<<14:
		b.Write([]byte{byte(l&0x7F | 0x80), byte(l >> 7)})
	default:
		b.Write([]byte{byte(l&0x7F | 0x80), byte((l>>7)&0x7F | 0x80), byte(l >> 14)})
	}
}

// Tight jpeg rect
func vncRectJpeg(x, y, w, h int, jpg []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint16(x))
	binary.Write(&b, binary.BigEndian, uint16(y))
	binary.Write(&b, binary.BigEndian, uint16(w))
	binary.Write(&b, binary.BigEndian, uint16(h))
	binary.Write(&b, binary.BigEndian, int32(rfbEncodingTight))
	b.WriteByte(rfbTightJpeg << 4)
	vncCompact(len(jpg), &b)
	b.Write(jpg)
	return b.Bytes()
}

// The Zlib encoding and tight's basic stream 0 are separate zlib streams on the
// client, so they need separate deflaters here. Neither may ever be restarted
// mid connection: rfb has no way to tell a client to reset its inflater, and a
// second stream header arrives as a data error.
const (
	zsZlib = iota
	zsTight
)

func (c *vncClient) deflate(s int, data []byte) []byte {
	if c.zw[s] == nil {
		c.zw[s] = zlib.NewWriter(&c.zbuf[s])
	}
	c.zw[s].Write(data)
	c.zw[s].Flush()
	comp := append([]byte(nil), c.zbuf[s].Bytes()...)
	c.zbuf[s].Reset()
	return comp
}

func vncRectHdr(x, y, w, h int) *bytes.Buffer {
	b := &bytes.Buffer{}
	binary.Write(b, binary.BigEndian, uint16(x))
	binary.Write(b, binary.BigEndian, uint16(y))
	binary.Write(b, binary.BigEndian, uint16(w))
	binary.Write(b, binary.BigEndian, uint16(h))
	binary.Write(b, binary.BigEndian, int32(rfbEncodingTight))
	return b
}

// Tight basic rect, copy filter: raw index bytes for colormap clients
func (c *vncClient) rectTightIdx(x, y int, idx []byte, w, h int) []byte {
	b := vncRectHdr(x, y, w, h)
	b.WriteByte(0x00) // basic compression, stream 0, copy filter
	comp := c.deflate(zsTight, idx)
	vncCompact(len(comp), b)
	b.Write(comp)
	return b.Bytes()
}

// SetColourMapEntries: give the colormap client our quantized palette
func vncColorMapMsg(pal color.Palette) []byte {
	var b bytes.Buffer
	b.Write([]byte{1, 0, 0, 0}) // msg, pad, first colour 0
	binary.Write(&b, binary.BigEndian, uint16(len(pal)))
	for _, cl := range pal {
		r, g, bl, _ := cl.RGBA()
		binary.Write(&b, binary.BigEndian, uint16(r))
		binary.Write(&b, binary.BigEndian, uint16(g))
		binary.Write(&b, binary.BigEndian, uint16(bl))
	}
	return b.Bytes()
}

// Classic arrow cursor, X = black, . = white, space = transparent, hotspot 0,0
var vncCursorArt = []string{
	"X           ",
	"XX          ",
	"X.X         ",
	"X..X        ",
	"X...X       ",
	"X....X      ",
	"X.....X     ",
	"X......X    ",
	"X.......X   ",
	"X........X  ",
	"X.....XXXXX ",
	"X..X..X     ",
	"X.X X..X    ",
	"XX  X..X    ",
	"X    X..X   ",
	"     X..X   ",
	"      X..X  ",
	"      X..X  ",
	"       XX   ",
}

func (pf vncPixFmt) put(b []byte, r, g, bl uint32) []byte {
	v := ((r*uint32(pf.rMax)+127)/255)<<pf.rSh | ((g*uint32(pf.gMax)+127)/255)<<pf.gSh | ((bl*uint32(pf.bMax)+127)/255)<<pf.bSh
	for i := 0; i < int(pf.bpp)/8; i++ {
		if pf.bigEnd {
			b = append(b, byte(v>>(int(pf.bpp)-8-i*8)))
		} else {
			b = append(b, byte(v>>(i*8)))
		}
	}
	return b
}

// RichCursor rect: client renders the cursor locally, screencast has none.
// Colormap clients use the fixed palette slots 254 (black) and 255 (white).
func vncRectCursor(pf vncPixFmt, indexed bool) []byte {
	w, h := len(vncCursorArt[0]), len(vncCursorArt)
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint16(0)) // hotspot
	binary.Write(&b, binary.BigEndian, uint16(0))
	binary.Write(&b, binary.BigEndian, uint16(w))
	binary.Write(&b, binary.BigEndian, uint16(h))
	binary.Write(&b, binary.BigEndian, int32(rfbEncodingCursor))
	var pix []byte
	for _, row := range vncCursorArt {
		for _, ch := range row {
			switch {
			case indexed && ch == 'X':
				pix = append(pix, 254)
			case indexed:
				pix = append(pix, 255)
			case ch == 'X':
				pix = pf.put(pix, 0, 0, 0)
			default:
				pix = pf.put(pix, 255, 255, 255)
			}
		}
	}
	b.Write(pix)
	for _, row := range vncCursorArt { // mask, msb first
		var m, n byte
		for _, ch := range row {
			m <<= 1
			if ch != ' ' {
				m |= 1
			}
			if n++; n == 8 {
				b.WriteByte(m)
				m, n = 0, 0
			}
		}
		if n > 0 {
			b.WriteByte(m << (8 - n))
		}
	}
	return b.Bytes()
}

// ExtendedDesktopSize rect: x=reason, y=result, one screen
func vncSendExtDeskSize(conn net.Conn, reason, result, w, h int) error {
	var b bytes.Buffer
	b.Write([]byte{0, 0, 0, 1})
	binary.Write(&b, binary.BigEndian, uint16(reason))
	binary.Write(&b, binary.BigEndian, uint16(result))
	binary.Write(&b, binary.BigEndian, uint16(w))
	binary.Write(&b, binary.BigEndian, uint16(h))
	binary.Write(&b, binary.BigEndian, int32(rfbEncodingExtDesk))
	b.Write([]byte{1, 0, 0, 0}) // one screen
	binary.Write(&b, binary.BigEndian, uint32(1))
	binary.Write(&b, binary.BigEndian, uint16(0))
	binary.Write(&b, binary.BigEndian, uint16(0))
	binary.Write(&b, binary.BigEndian, uint16(w))
	binary.Write(&b, binary.BigEndian, uint16(h))
	binary.Write(&b, binary.BigEndian, uint32(0))
	_, err := conn.Write(b.Bytes())
	return err
}

func vncSendDeskSize(conn net.Conn, w, h int) error {
	var b bytes.Buffer
	b.Write([]byte{0, 0, 0, 1})
	binary.Write(&b, binary.BigEndian, uint16(0))
	binary.Write(&b, binary.BigEndian, uint16(0))
	binary.Write(&b, binary.BigEndian, uint16(w))
	binary.Write(&b, binary.BigEndian, uint16(h))
	binary.Write(&b, binary.BigEndian, int32(rfbEncodingDesk))
	_, err := conn.Write(b.Bytes())
	return err
}
