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
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

const (
	rfbEncodingTight   = 7
	rfbEncodingDesk    = -223 // DesktopSize
	rfbEncodingExtDesk = -308 // ExtendedDesktopSize
	rfbEncodingCursor  = -239 // RichCursor
	rfbTightJpeg       = 0x09
	vncMaxDim          = 2048 // tight rects can't be wider, also sane viewport limit
	vncMinDim          = 64
	vncNavH            = 20 // nav bar height
	vncBtnW            = 22
	vncZoomX           = 172 // zoom percent label
	vncURLX            = 204 // url field start
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
	extDesk    bool
	extInit    bool // owe the client its initial ExtendedDesktopSize announce
	deskSize   bool
	needCursor bool
	pf         vncPixFmt
	indexed    bool // colormap client: we control the palette via SetColourMapEntries
	warnedFit  bool
	// frame writer goroutine only: persistent tight zlib stream and last sent colormap
	zbuf     bytes.Buffer
	zw       *zlib.Writer
	lastCmap []byte
	// read loop only
	buttons byte
	mods    int64
}

type vncState struct {
	sync.Mutex
	clients    map[*vncClient]bool
	frame      []byte // current browser frame as jpeg
	fw, fh     int    // dimensions of current frame
	seq        uint64
	barJpg     []byte // nav bar strip as jpeg
	barW       int
	barSeq     uint64
	urlText    string  // url shown in the bar
	edit       string  // url being edited
	focused    bool    // keyboard focus on the url field
	selAll     bool    // focus just landed: first keystroke replaces the url
	vw, vh     int     // desired framebuffer size (viewport is vh-vncNavH)
	zoom       float64 // page zoom, applied as css zoom in the browser
	zoomScript page.ScriptIdentifier
	suspended  bool
	casting    bool
	inputCh    chan chromedp.Action
	castCtx    interface{} // browser ctx the screencast listener is bound to
}

var vncSrv vncState

func vncClamp(v int) int {
	if v < vncMinDim {
		return vncMinDim
	}
	if v > vncMaxDim {
		return vncMaxDim
	}
	return v
}

func vncStart() {
	vncSrv.vw = vncClamp(int(defGeom.w))
	vncSrv.vh = int(defGeom.h)
	if vncSrv.vh < 10 {
		vncSrv.vh = 768
	}
	vncSrv.vh = vncClamp(vncSrv.vh) + vncNavH
	vncSrv.zoom = 1.0
	vncSrv.clients = make(map[*vncClient]bool)
	vncSrv.inputCh = make(chan chromedp.Action, 256)
	go func() { // ordered browser input dispatch
		for a := range vncSrv.inputCh {
			if err := chromedp.Run(ctx, a); err != nil {
				log.Printf("VNC: input dispatch: %v", err)
			}
		}
	}()
	vncPublishPlaceholder()
	ln, err := net.Listen("tcp", *vncAddr)
	if err != nil {
		log.Fatalf("VNC: listen: %v", err)
	}
	log.Printf("Started VNC server on %v, initial framebuffer %vx%v", *vncAddr, vncSrv.vw, vncSrv.vh)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				log.Printf("VNC: accept: %v", err)
				return
			}
			go vncHandle(c)
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
	fb := image.NewRGBA(image.Rect(0, 0, vncSrv.vw, vncSrv.vh-vncNavH))
	draw.Draw(fb, fb.Bounds(), image.NewUniform(vncBgColor()), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, fb, &jpeg.Options{Quality: int(*defJpgQual)}); err != nil {
		return
	}
	vncPublishJPEG(buf.Bytes())
}

// Publish a browser frame to all connected clients
func vncPublishJPEG(jpg []byte) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(jpg))
	if err != nil {
		log.Printf("VNC: bad frame: %v", err)
		return
	}
	vncSrv.Lock()
	if vncSrv.suspended {
		vncSrv.Unlock()
		return
	}
	vncSrv.frame = jpg
	vncSrv.fw = cfg.Width
	vncSrv.fh = cfg.Height
	vncSrv.seq++
	barW := vncSrv.barW
	for c := range vncSrv.clients {
		vncPoke(c)
	}
	vncSrv.Unlock()
	if barW != cfg.Width {
		vncDrawBar(cfg.Width)
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

func vncDrawBar(w int) {
	if w < vncURLX+40 {
		return
	}
	vncSrv.Lock()
	text := vncSrv.urlText
	if vncSrv.focused {
		text = vncSrv.edit + "_"
	}
	pct := fmt.Sprintf("%d%%", int(vncSrv.zoom*100+0.5))
	vncSrv.Unlock()
	img := image.NewRGBA(image.Rect(0, 0, w, vncNavH))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{0xC0, 0xC0, 0xC0, 0xFF}), image.Point{}, draw.Src)
	for x := 0; x < w; x++ {
		img.Set(x, vncNavH-1, color.Black)
	}
	dr := font.Drawer{Dst: img, Src: image.NewUniform(color.Black), Face: basicfont.Face7x13}
	for i, l := range vncBtnLabels {
		bx := 2 + i*(vncBtnW+2)
		draw.Draw(img, image.Rect(bx, 2, bx+vncBtnW, vncNavH-2), image.NewUniform(color.RGBA{0xE0, 0xE0, 0xE0, 0xFF}), image.Point{}, draw.Src)
		vncBorder(img, bx, 2, bx+vncBtnW, vncNavH-2, color.Black)
		dr.Dot = fixed.P(bx+8, 14)
		dr.DrawString(l)
	}
	dr.Dot = fixed.P(vncZoomX, 14)
	dr.DrawString(pct)
	draw.Draw(img, image.Rect(vncURLX, 2, w-3, vncNavH-2), image.NewUniform(color.White), image.Point{}, draw.Src)
	vncBorder(img, vncURLX, 2, w-3, vncNavH-2, color.Black)
	if max := (w - 3 - vncURLX - 8) / 7; len(text) > max { // show the tail
		text = text[len(text)-max:]
	}
	dr.Dot = fixed.P(vncURLX+4, 14)
	dr.DrawString(text)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		return
	}
	vncSrv.Lock()
	vncSrv.barJpg = buf.Bytes()
	vncSrv.barW = w
	vncSrv.barSeq++
	for c := range vncSrv.clients {
		vncPoke(c)
	}
	vncSrv.Unlock()
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

func vncCastListen() {
	if vncSrv.castCtx == ctx { // already listening on this browser context
		return
	}
	vncSrv.castCtx = ctx
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		switch e := ev.(type) {
		case *page.EventScreencastFrame:
			go chromedp.Run(ctx, page.ScreencastFrameAck(e.SessionID))
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
	vncSrv.Lock()
	w, h := vncSrv.vw, vncSrv.vh-vncNavH
	zoom := vncSrv.zoom
	vncSrv.Unlock()
	vncCastListen()
	var loc string
	err := chromedp.Run(ctx,
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
	vncSetURL(loc)
	if zoom != 1 {
		vncApplyZoom(zoom)
	}
	log.Printf("VNC: screencast started at %vx%v zoom %v", w, h, zoom)
}

func vncCastStop() {
	chromedp.Run(ctx, page.StopScreencast())
	log.Printf("VNC: screencast stopped")
}

// Called by http capture routines: they need the viewport for themselves
func vncSuspend() {
	if *vncAddr == "" {
		return
	}
	vncSrv.Lock()
	on := vncSrv.casting
	vncSrv.suspended = true
	vncSrv.Unlock()
	if on {
		chromedp.Run(ctx, page.StopScreencast())
	}
}

// Restore viewport and screencast after http capture, emits a fresh frame
func vncResume() {
	if *vncAddr == "" {
		return
	}
	vncSrv.Lock()
	vncSrv.suspended = false
	on := vncSrv.casting
	vncSrv.Unlock()
	if on {
		vncCastStart()
	}
}

// Called from ctxErr when a new browser context is created
func vncCtxReset() {
	if *vncAddr == "" {
		return
	}
	vncSrv.Lock()
	on := vncSrv.casting
	vncSrv.Unlock()
	if on {
		vncCastStart()
	}
}

// ---- client handling ----

func vncHandle(conn net.Conn) {
	defer conn.Close()
	log.Printf("%s VNC client connected", conn.RemoteAddr())
	if err := vncHandshake(conn); err != nil {
		log.Printf("%s VNC handshake: %v", conn.RemoteAddr(), err)
		return
	}
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
		conn.Write([]byte{1, 1}) // one security type: none
		one := make([]byte, 1)
		if _, err := io.ReadFull(conn, one); err != nil {
			return err
		}
		if one[0] != 1 {
			return fmt.Errorf("unsupported security type %d", one[0])
		}
		if min >= 8 {
			conn.Write([]byte{0, 0, 0, 0}) // security result ok
		}
	} else {
		conn.Write([]byte{0, 0, 0, 1}) // 3.3: security type none
	}
	if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil { // ClientInit / shared flag
		return fmt.Errorf("client gave up after security none - macOS Screen Sharing is not supported (no tight encoding), use TigerVNC or TightVNC viewer: %w", err)
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
			indexed := b[6] == 0 && b[3] == 8
			if indexed { // colormap client: we drive the palette adaptively per frame
				pf = vncBGR233
				log.Printf("%s VNC client wants a colormap, using adaptive quantized palette", c.conn.RemoteAddr())
			}
			vncSrv.Lock()
			c.pf = pf
			c.indexed = indexed
			vncSrv.Unlock()
			if pf.bpp < 16 {
				log.Printf("%s VNC client at %d bpp, using tight palette with %d color quantization instead of jpeg", c.conn.RemoteAddr(), pf.bpp, defGeom.c)
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
			if _, err := io.CopyN(io.Discard, c.conn, int64(binary.BigEndian.Uint32(b[3:]))); err != nil {
				return
			}
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
	var tight, ext, desk, cur bool
	for i := 0; i < len(encs); i += 4 {
		switch int32(binary.BigEndian.Uint32(encs[i:])) {
		case rfbEncodingTight:
			tight = true
		case rfbEncodingExtDesk:
			ext = true
		case rfbEncodingDesk:
			desk = true
		case rfbEncodingCursor:
			cur = true
		}
	}
	if !tight {
		log.Printf("%s VNC client does not support tight encoding, disconnecting", c.conn.RemoteAddr())
		return false
	}
	vncSrv.Lock()
	// TigerVNC only sends SetDesktopSize after it saw an ExtendedDesktopSize from us
	c.extInit = ext && !c.extDesk
	c.extDesk = ext
	c.deskSize = desk
	c.needCursor = cur
	vncSrv.Unlock()
	if cur || ext {
		vncPoke(c)
	}
	log.Printf("%s VNC client encodings: tight, resize ext=%v basic=%v, cursor=%v", c.conn.RemoteAddr(), ext, desk, cur)
	return true
}

// TigerVNC remote resize: viewport follows the client window size
func vncSetDesktopSize(c *vncClient) bool {
	b := make([]byte, 7)
	if _, err := io.ReadFull(c.conn, b); err != nil {
		return false
	}
	w := vncClamp(int(binary.BigEndian.Uint16(b[1:])))
	h := vncClamp(int(binary.BigEndian.Uint16(b[3:])))
	if h < vncMinDim+vncNavH {
		h = vncMinDim + vncNavH
	}
	if _, err := io.CopyN(io.Discard, c.conn, int64(b[5])*16); err != nil { // screen layouts
		return false
	}
	log.Printf("%s VNC client requests desktop size %vx%v", c.conn.RemoteAddr(), w, h)
	vncSrv.Lock()
	vncSrv.vw, vncSrv.vh = w, h
	c.annW, c.annH = w, h
	c.sentSeq = 0 // full frame after resize
	c.sentBar = 0
	on := vncSrv.casting
	vncSrv.Unlock()
	// ack the requester: reason=1 (this client), result=0 (success)
	c.wmu.Lock()
	err := vncSendExtDeskSize(c.conn, 1, 0, w, h)
	c.wmu.Unlock()
	if err != nil {
		return false
	}
	if on {
		vncCastStart() // apply new viewport, emits fresh frame
	}
	return true
}

// ---- input ----

func vncDispatch(a chromedp.Action) {
	select {
	case vncSrv.inputCh <- a:
	default: // browser can't keep up, drop
	}
}

var vncKeysyms = map[uint32]string{
	0xFF08: kb.Backspace, 0xFF09: kb.Tab, 0xFF0D: kb.Enter, 0xFF8D: kb.Enter,
	0xFF1B: kb.Escape, 0xFFFF: kb.Delete, 0xFF63: kb.Insert,
	0xFF51: kb.ArrowLeft, 0xFF52: kb.ArrowUp, 0xFF53: kb.ArrowRight, 0xFF54: kb.ArrowDown,
	0xFF55: kb.PageUp, 0xFF56: kb.PageDown, 0xFF50: kb.Home, 0xFF57: kb.End,
}

func vncKeyEvent(c *vncClient, down bool, ks uint32) {
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
	}
	if mod != 0 {
		if down {
			c.mods |= mod
		} else {
			c.mods &^= mod
		}
		return
	}
	vncSrv.Lock()
	focused := vncSrv.focused
	vncSrv.Unlock()
	if focused {
		if down {
			vncEditKey(ks)
		}
		return
	}
	if !down {
		return
	}
	if c.mods&int64(input.ModifierCtrl) != 0 { // browser style zoom shortcuts
		switch ks {
		case '+', '=':
			vncZoom(0.25)
			return
		case '-':
			vncZoom(-0.25)
			return
		case '0':
			vncZoom(0)
			return
		}
	}
	var keys string
	if s, ok := vncKeysyms[ks]; ok {
		keys = s
	} else if (ks >= 0x20 && ks <= 0x7E) || (ks >= 0xA0 && ks <= 0xFF) {
		keys = string(rune(ks))
	} else {
		return
	}
	if c.mods != 0 {
		vncDispatch(chromedp.KeyEvent(keys, chromedp.KeyModifiers(input.Modifier(c.mods))))
		return
	}
	vncDispatch(chromedp.KeyEvent(keys))
}

// Keystroke into the url field
func vncEditKey(ks uint32) {
	vncSrv.Lock()
	switch {
	case ks == 0xFF0D || ks == 0xFF8D: // enter - navigate
		u := vncSrv.edit
		if len(u) > 1 && !strings.HasPrefix(u, "http") {
			u = *searchEng + url.QueryEscape(u)
		}
		vncSrv.focused = false
		vncSrv.urlText = u
		vncSrv.edit = u
		w := vncSrv.barW
		vncSrv.Unlock()
		log.Printf("VNC: navigate to %s", u)
		vncDispatch(chromedp.Navigate(u))
		vncDrawBar(w)
		return
	case ks == 0xFF1B: // escape - cancel edit
		vncSrv.focused = false
		vncSrv.edit = vncSrv.urlText
		vncSrv.selAll = false
	case ks == 0xFF08 || ks == 0xFFFF: // backspace / delete
		if vncSrv.selAll {
			vncSrv.edit = ""
			vncSrv.selAll = false
			break
		}
		if r := []rune(vncSrv.edit); len(r) > 0 {
			vncSrv.edit = string(r[:len(r)-1])
		}
	case (ks >= 0x20 && ks <= 0x7E) || (ks >= 0xA0 && ks <= 0xFF):
		if vncSrv.selAll {
			vncSrv.edit = ""
			vncSrv.selAll = false
		}
		vncSrv.edit += string(rune(ks))
	}
	w := vncSrv.barW
	vncSrv.Unlock()
	vncDrawBar(w)
}

func vncPointerEvent(c *vncClient, mask byte, x, y uint16) {
	diff := mask ^ c.buttons
	c.buttons = mask
	if int(y) < vncNavH {
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
		if diff&0x01 != 0 && mask&0x01 != 0 { // left press in the bar
			vncBarClick(int(x))
		}
		return
	}
	fx, fy := float64(x), float64(int(y)-vncNavH)
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
	// wheel: buttons 4/5 are single notch press+release pairs
	if diff&0x08 != 0 && mask&0x08 != 0 {
		vncDispatch(input.DispatchMouseEvent(input.MouseWheel, fx, fy).WithDeltaY(-120))
	}
	if diff&0x10 != 0 && mask&0x10 != 0 {
		vncDispatch(input.DispatchMouseEvent(input.MouseWheel, fx, fy).WithDeltaY(120))
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
		}
		vncDispatch(input.DispatchMouseEvent(typ, fx, fy).
			WithButton(bt.b).WithClickCount(1).WithModifiers(input.Modifier(c.mods)))
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
	vncDispatch(input.DispatchMouseEvent(input.MouseMoved, fx, fy).WithButton(held))
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
			vncDispatch(chromedp.NavigateBack())
		case 1:
			vncDispatch(chromedp.NavigateForward())
		case 2:
			vncDispatch(chromedp.Reload())
		case 3:
			vncDispatch(chromedp.Stop())
		case 4: // home
			vncDispatch(chromedp.Navigate("about:blank"))
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
	vncSrv.Lock()
	vncSrv.focused = true
	vncSrv.edit = vncSrv.urlText
	vncSrv.selAll = true
	w := vncSrv.barW
	vncSrv.Unlock()
	vncDrawBar(w)
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
		if !c.pending || frame == nil || (c.sentSeq == seq && c.sentBar == barSeq && !c.extInit) {
			vncSrv.Unlock()
			continue
		}
		resize := fw != c.annW || fh != c.annH-vncNavH
		announce := resize && (c.extDesk || c.deskSize)
		if announce {
			c.annW, c.annH = fw, fh+vncNavH
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
		vncSrv.Unlock()
		if fit { // client can't resize - letterbox into its framebuffer
			if !c.warnedFit {
				c.warnedFit = true
				log.Printf("%s VNC client has no resize support, letterboxing %vx%v into %vx%v", c.conn.RemoteAddr(), fw, fh, aw, ah-vncNavH)
			}
			frame = vncLetterbox(frame, aw, ah-vncNavH)
			if frame == nil {
				continue
			}
			fw, fh = aw, ah-vncNavH
		}
		var rects [][]byte
		var cmap []byte
		if cursor {
			rects = append(rects, vncRectCursor(pf, indexed))
		}
		if pf.bpp < 16 { // tight jpeg needs 16/32 bpp, quantize and use tight palette filter
			lr, cm := c.lowBppRects(frame, bar, sendBar && barW == fw)
			if lr == nil {
				continue
			}
			rects = append(rects, lr...)
			cmap = cm
		} else {
			if sendBar && barW == fw {
				rects = append(rects, vncRectJpeg(0, 0, barW, vncNavH, bar))
			}
			rects = append(rects, vncRectJpeg(0, vncNavH, fw, fh, frame))
		}
		c.wmu.Lock()
		var err error
		if cmap != nil {
			_, err = c.conn.Write(cmap)
		}
		if err == nil && extInit && !announce {
			err = vncSendExtDeskSize(c.conn, 0, 0, aw, ah)
		}
		if err == nil && announce {
			if ext {
				err = vncSendExtDeskSize(c.conn, 0, 0, fw, fh+vncNavH)
			} else {
				err = vncSendDeskSize(c.conn, fw, fh+vncNavH)
			}
		}
		if err == nil {
			err = vncSendUpdate(c.conn, rects)
		}
		c.wmu.Unlock()
		if err != nil {
			log.Printf("%s VNC send: %v", c.conn.RemoteAddr(), err)
			c.conn.Close()
			return
		}
	}
}

func vncLetterbox(jpg []byte, w, h int) []byte {
	i, err := jpeg.Decode(bytes.NewReader(jpg))
	if err != nil {
		return nil
	}
	fb := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(fb, fb.Bounds(), image.NewUniform(vncBgColor()), image.Point{}, draw.Src)
	draw.Draw(fb, fb.Bounds(), i, image.Point{}, draw.Over)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, fb, &jpeg.Options{Quality: int(*defJpgQual)}); err != nil {
		return nil
	}
	return buf.Bytes()
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

// Low bpp clients: truecolor gets floyd steinberg dithering into its own pixel
// format, colormap clients get gif mode style median cut quantization via
// gifPalette with the palette pushed as their colormap.
func (c *vncClient) lowBppRects(frame, bar []byte, withBar bool) ([][]byte, []byte) {
	fimg, err := jpeg.Decode(bytes.NewReader(frame))
	if err != nil {
		return nil, nil
	}
	var rects [][]byte
	var cmap []byte
	if c.indexed {
		nq := defGeom.c
		if nq < 2 {
			nq = 2
		}
		if nq > 254 { // leave room for the fixed cursor colors
			nq = 254
		}
		q := vncPaletted(gifPalette(fimg, nq))
		pal := make(color.Palette, 256)
		for i := range pal {
			pal[i] = color.Black
		}
		copy(pal, q.Palette)
		pal[254], pal[255] = color.Black, color.White // cursor colors
		cm := vncColorMapMsg(pal)
		if !bytes.Equal(cm, c.lastCmap) {
			cmap = cm
			c.lastCmap = cm
		}
		if withBar {
			if bimg, err := jpeg.Decode(bytes.NewReader(bar)); err == nil {
				rects = append(rects, c.rectTightIdx(0, 0, vncIndex(bimg, q.Palette), bimg.Bounds().Dx(), bimg.Bounds().Dy()))
			}
		}
		b := q.Bounds()
		rects = append(rects, c.rectTightIdx(0, vncNavH, q.Pix, b.Dx(), b.Dy()))
		return rects, cmap
	}
	// truecolor low bpp: floyd steinberg dither straight into the client format,
	// error diffusion is what keeps greys grey in 3-3-2 color
	if withBar {
		if bimg, err := jpeg.Decode(bytes.NewReader(bar)); err == nil {
			rects = append(rects, c.rectTightIdx(0, 0, c.dither8(bimg), bimg.Bounds().Dx(), bimg.Bounds().Dy()))
		}
	}
	b := fimg.Bounds()
	rects = append(rects, c.rectTightIdx(0, vncNavH, c.dither8(fimg), b.Dx(), b.Dy()))
	return rects, nil
}

// Floyd steinberg dithering with O(1) quantization to the client's 8 bpp
// true color format, the bit packed formats need no palette search
func (c *vncClient) dither8(img image.Image) []byte {
	pf := c.pf
	rMax, gMax, bMax := int32(pf.rMax), int32(pf.gMax), int32(pf.bMax)
	if rMax < 1 || gMax < 1 || bMax < 1 {
		rMax, gMax, bMax = 7, 7, 3
	}
	bd := img.Bounds()
	w, h := bd.Dx(), bd.Dy()
	pix := make([]byte, 0, w*h)
	cur := make([][3]int32, w+2)
	next := make([][3]int32, w+2)
	clamp := func(v int32) int32 {
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return v
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, _ := img.At(bd.Min.X+x, bd.Min.Y+y).RGBA()
			cr := clamp(int32(r>>8) + cur[x+1][0])
			cg := clamp(int32(g>>8) + cur[x+1][1])
			cb := clamp(int32(bl>>8) + cur[x+1][2])
			qr := (cr*rMax + 127) / 255
			qg := (cg*gMax + 127) / 255
			qb := (cb*bMax + 127) / 255
			pix = append(pix, byte(qr<<pf.rSh|qg<<pf.gSh|qb<<pf.bSh))
			er := cr - (qr*255+rMax/2)/rMax
			eg := cg - (qg*255+gMax/2)/gMax
			eb := cb - (qb*255+bMax/2)/bMax
			for i, d := range [4]struct {
				row *[][3]int32
				x   int
				mul int32
			}{{&cur, x + 2, 7}, {&next, x, 3}, {&next, x + 1, 5}, {&next, x + 2, 1}} {
				_ = i
				(*d.row)[d.x][0] += er * d.mul / 16
				(*d.row)[d.x][1] += eg * d.mul / 16
				(*d.row)[d.x][2] += eb * d.mul / 16
			}
		}
		cur, next = next, cur
		for i := range next {
			next[i] = [3]int32{}
		}
	}
	return pix
}

// gifPalette returns a dithered grayscale image for 2 colors, normalize to paletted
func vncPaletted(i image.Image) *image.Paletted {
	if p, ok := i.(*image.Paletted); ok {
		return p
	}
	b := i.Bounds()
	p := image.NewPaletted(b, color.Palette{color.Black, color.White})
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := i.At(x, y).RGBA()
			if r+g+bl >= 3*0x8000 {
				p.SetColorIndex(x, y, 1)
			}
		}
	}
	return p
}

func vncIndex(i image.Image, pal color.Palette) []byte {
	b := i.Bounds()
	idx := make([]byte, 0, b.Dx()*b.Dy())
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			idx = append(idx, byte(pal.Index(i.At(x, y))))
		}
	}
	return idx
}

func (c *vncClient) deflate(data []byte) []byte {
	if c.zw == nil {
		c.zw = zlib.NewWriter(&c.zbuf)
	}
	c.zw.Write(data)
	c.zw.Flush()
	comp := append([]byte(nil), c.zbuf.Bytes()...)
	c.zbuf.Reset()
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
	comp := c.deflate(idx)
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
