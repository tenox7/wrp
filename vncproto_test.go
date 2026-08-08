package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"net"
	"testing"
	"time"
)

// Drives the real server over a real socket, without a browser attached: frames
// are published directly instead of arriving from the screencast.
type protoConn struct {
	t    *testing.T
	c    net.Conn
	w, h int
}

func startTestServer(t *testing.T) net.Addr {
	t.Helper()
	// the server state is a package global, so wait for the previous test's
	// connections to finish before resetting it
	for i := 0; i < 400 && vncConns.Load() > 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if n := vncConns.Load(); n > 0 {
		t.Fatalf("%d connections still open from a previous test", n)
	}
	defGeom = geom{w: 320, h: 200, c: 216}
	vncInit()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	vncServe(ln)
	return ln.Addr()
}

// Push a frame the way the screencast would, and wait for it to be picked up
func publishTestFrame(t *testing.T, img *image.RGBA) {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	before := frameSeq()
	vncPublishJPEG(buf.Bytes())
	for i := 0; i < 100 && frameSeq() == before; i++ {
		time.Sleep(5 * time.Millisecond)
	}
}

func frameSeq() uint64 {
	vncSrv.Lock()
	defer vncSrv.Unlock()
	return vncSrv.seq
}

func dialProto(t *testing.T, addr net.Addr) *protoConn {
	t.Helper()
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(20 * time.Second))
	p := &protoConn{t: t, c: c}

	ver := make([]byte, 12)
	p.read(ver)
	if string(ver) != "RFB 003.008\n" {
		t.Fatalf("server version %q", ver)
	}
	p.c.Write([]byte("RFB 003.008\n"))

	n := make([]byte, 1)
	p.read(n)
	if n[0] == 0 {
		t.Fatal("server offered no security types")
	}
	sec := make([]byte, n[0])
	p.read(sec)
	if sec[0] != 1 {
		t.Fatalf("expected security type none, got %v", sec)
	}
	p.c.Write([]byte{1})
	res := make([]byte, 4)
	p.read(res)
	if binary.BigEndian.Uint32(res) != 0 {
		t.Fatal("security handshake failed")
	}

	p.c.Write([]byte{1}) // ClientInit, shared
	si := make([]byte, 24)
	p.read(si)
	p.w = int(binary.BigEndian.Uint16(si[0:]))
	p.h = int(binary.BigEndian.Uint16(si[2:]))
	if si[4] != 32 || si[5] != 24 || si[7] != 1 {
		t.Fatalf("unexpected server pixel format bpp=%d depth=%d truecolor=%d", si[4], si[5], si[7])
	}
	p.read(make([]byte, binary.BigEndian.Uint32(si[20:])))
	return p
}

func (p *protoConn) read(b []byte) {
	p.t.Helper()
	if _, err := io.ReadFull(p.c, b); err != nil {
		p.t.Fatalf("read %d bytes: %v", len(b), err)
	}
}

func (p *protoConn) setEncodings(encs ...int32) {
	m := []byte{2, 0, 0, 0}
	binary.BigEndian.PutUint16(m[2:], uint16(len(encs)))
	for _, e := range encs {
		var t [4]byte
		binary.BigEndian.PutUint32(t[:], uint32(e))
		m = append(m, t[:]...)
	}
	p.c.Write(m)
}

func (p *protoConn) fbur(incremental bool) {
	u := make([]byte, 10)
	u[0] = 3
	if incremental {
		u[1] = 1
	}
	binary.BigEndian.PutUint16(u[6:], uint16(p.w))
	binary.BigEndian.PutUint16(u[8:], uint16(p.h))
	p.c.Write(u)
}

type protoRect struct {
	x, y, w, h int
	enc        int32
	srcX, srcY int    // copyrect only
	pix        []byte // raw rects only
}

func (p *protoConn) readUpdate() []protoRect {
	p.t.Helper()
	hdr := make([]byte, 4)
	p.read(hdr)
	if hdr[0] != 0 {
		p.t.Fatalf("expected a framebuffer update, got server message %d", hdr[0])
	}
	out := make([]protoRect, 0, binary.BigEndian.Uint16(hdr[2:]))
	for i := 0; i < cap(out); i++ {
		out = append(out, p.readRect(i))
	}
	return out
}

func (p *protoConn) readRect(i int) protoRect {
	p.t.Helper()
	rh := make([]byte, 12)
	p.read(rh)
	r := protoRect{
		x:   int(binary.BigEndian.Uint16(rh[0:])),
		y:   int(binary.BigEndian.Uint16(rh[2:])),
		w:   int(binary.BigEndian.Uint16(rh[4:])),
		h:   int(binary.BigEndian.Uint16(rh[6:])),
		enc: int32(binary.BigEndian.Uint32(rh[8:])),
	}
	switch r.enc {
	case rfbEncodingRaw:
		r.pix = make([]byte, r.w*r.h*4)
		p.read(r.pix)
	case rfbEncodingCopyRect:
		src := make([]byte, 4)
		p.read(src)
		r.srcX = int(binary.BigEndian.Uint16(src[0:]))
		r.srcY = int(binary.BigEndian.Uint16(src[2:]))
	default:
		p.t.Fatalf("unexpected encoding %d in rect %d", r.enc, i)
	}
	return r
}

// Draw an update into a framebuffer the way a viewer would, in order: a
// CopyRect moves what is already there, the rects after it paint over the
// result. Getting this wrong here would hide exactly the bug it is looking for.
func (p *protoConn) apply(fb *image.RGBA, rects []protoRect) {
	p.t.Helper()
	for _, r := range rects {
		if r.enc == rfbEncodingCopyRect {
			src := image.NewRGBA(image.Rect(0, 0, r.w, r.h))
			draw.Draw(src, src.Bounds(), fb, image.Pt(r.srcX, r.srcY), draw.Src)
			draw.Draw(fb, image.Rect(r.x, r.y, r.x+r.w, r.y+r.h), src, image.Point{}, draw.Src)
			continue
		}
		if r.enc != rfbEncodingRaw {
			continue
		}
		for y := 0; y < r.h; y++ {
			for x := 0; x < r.w; x++ {
				i := (y*r.w + x) * 4 // 32 bpp little endian, r<<16 g<<8 b
				o := fb.PixOffset(r.x+x, r.y+y)
				fb.Pix[o], fb.Pix[o+1] = r.pix[i+2], r.pix[i+1]
				fb.Pix[o+2], fb.Pix[o+3] = r.pix[i], 0xff
			}
		}
	}
}

// The viewport at this scroll offset, as a frame the screencast would deliver
func testCrop(page *image.RGBA, top, w, h int) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(out, out.Bounds(), page, image.Pt(0, top), draw.Src)
	return out
}

func solidFrame(w, h int, r, g, b byte) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = r, g, b, 0xff
	}
	return img
}

// Pick out the rect covering the browser viewport, ie below the nav bar
func frameRect(t *testing.T, rects []protoRect) protoRect {
	t.Helper()
	for _, r := range rects {
		if r.y >= vncChromeH {
			return r
		}
	}
	t.Fatalf("no rect below the nav bar in %d rects", len(rects))
	return protoRect{}
}

// Every pixel of a raw rect must be the colour we published. Frames reach the
// server as jpeg, so a channel may land a step or two off.
func assertSolid(t *testing.T, r protoRect, want [3]byte) {
	t.Helper()
	for i := 0; i+3 < len(r.pix); i += 4 {
		// server pixel format is 32 bpp little endian, r<<16 g<<8 b
		got := [3]byte{r.pix[i+2], r.pix[i+1], r.pix[i]}
		for k := 0; k < 3; k++ {
			if abs(int(got[k])-int(want[k])) > 3 {
				t.Fatalf("pixel %d is %v, want about %v", i/4, got, want)
			}
		}
	}
}

// Handshake through to a correct first frame, over a real socket
func TestProtoFullUpdate(t *testing.T) {
	addr := startTestServer(t)
	publishTestFrame(t, solidFrame(320, 200, 0x20, 0x90, 0x40))
	p := dialProto(t, addr)
	if p.w != 320 || p.h != 200+vncChromeH {
		t.Fatalf("desktop is %dx%d, want 320x%d", p.w, p.h, 200+vncChromeH)
	}
	p.setEncodings(rfbEncodingRaw)
	p.fbur(false)

	rects := p.readUpdate()
	fr := frameRect(t, rects)
	if fr.x != 0 || fr.y != vncChromeH || fr.w != 320 || fr.h != 200 {
		t.Fatalf("frame rect is %d,%d %dx%d, want 0,%d 320x200", fr.x, fr.y, fr.w, fr.h, vncChromeH)
	}
	assertSolid(t, fr, [3]byte{0x20, 0x90, 0x40})
}

// An incremental request after a small change must send only that region, and a
// non incremental one must repaint everything even though nothing changed.
func TestProtoIncrementalUpdate(t *testing.T) {
	addr := startTestServer(t)
	publishTestFrame(t, solidFrame(320, 200, 0x10, 0x10, 0x10))
	p := dialProto(t, addr)
	p.setEncodings(rfbEncodingRaw)
	p.fbur(false)
	if fr := frameRect(t, p.readUpdate()); fr.h != 200 {
		t.Fatalf("first update covered %d rows, want the whole frame", fr.h)
	}

	// repaint one band near the top
	img := solidFrame(320, 200, 0x10, 0x10, 0x10)
	for y := 70; y < 90; y++ {
		for x := 0; x < 320; x++ {
			o := img.PixOffset(x, y)
			img.Pix[o], img.Pix[o+1], img.Pix[o+2] = 0xff, 0x00, 0x00
		}
	}
	publishTestFrame(t, img)
	p.fbur(true)
	fr := frameRect(t, p.readUpdate())
	if fr.h >= 200 {
		t.Fatalf("incremental update sent %d rows, expected just the changed band", fr.h)
	}
	if fr.y > vncChromeH+70 || fr.y+fr.h < vncChromeH+90 {
		t.Fatalf("changed band at rows 70-90 not covered by rect y=%d h=%d", fr.y-vncChromeH, fr.h)
	}

	// nothing changed, but a non incremental request must still repaint
	p.fbur(false)
	if fr := frameRect(t, p.readUpdate()); fr.h != 200 {
		t.Fatalf("non incremental request sent %d rows, want a full repaint", fr.h)
	}
}

// A scrolled frame should arrive as a CopyRect plus roughly the exposed band
func TestProtoCopyRectOnScroll(t *testing.T) {
	addr := startTestServer(t)
	const w, h, dy = 320, 200, 64
	page := testPage(w, 600, 5)

	publishTestFrame(t, testCrop(page, 0, w, h))
	p := dialProto(t, addr)
	p.setEncodings(rfbEncodingRaw, rfbEncodingCopyRect)
	p.fbur(false)
	p.readUpdate()

	publishTestFrame(t, testCrop(page, dy, w, h)) // same content, scrolled up by dy
	p.fbur(true)
	rects := p.readUpdate()

	var copies, rawRows int
	for _, r := range rects {
		switch r.enc {
		case rfbEncodingCopyRect:
			copies++
			if r.h != h-dy {
				t.Fatalf("CopyRect moved %d rows, expected %d", r.h, h-dy)
			}
			if r.y != vncChromeH || r.srcY != dy+vncChromeH {
				t.Fatalf("CopyRect moves %d to %d, expected %d to %d", r.srcY, r.y, dy+vncChromeH, vncChromeH)
			}
		case rfbEncodingRaw:
			if r.y >= vncChromeH {
				rawRows += r.h
			}
		}
	}
	if copies != 1 {
		t.Fatalf("expected exactly one CopyRect for a scroll, got %d", copies)
	}
	// the exposed band, plus the rows around it that jpeg left too different to
	// pass for the same content
	if rawRows > dy+48 {
		t.Fatalf("sent %d raw rows after the copy, the exposed band is only %d", rawRows, dy)
	}
	if rawRows >= h {
		t.Fatal("CopyRect saved nothing")
	}
}

// The end to end check: a long scroll over a real socket, with the framebuffer
// built purely from incremental updates, then compared against a clean one
// filled by a single non incremental refresh. Copies are matched with a
// tolerance, so this is where drift accumulating down the page would show.
func TestProtoScrollReconstruct(t *testing.T) {
	addr := startTestServer(t)
	const w, h, steps = 320, 200, 24
	page := testPage(w, 2400, 11)

	publishTestFrame(t, testCrop(page, 0, w, h))
	p := dialProto(t, addr)
	p.setEncodings(rfbEncodingRaw, rfbEncodingCopyRect)
	p.fbur(false)
	fb := image.NewRGBA(image.Rect(0, 0, p.w, p.h))
	p.apply(fb, p.readUpdate())

	copies, rawRows, top := 0, 0, 0
	for i := 0; i < steps; i++ {
		top += 11 + i*13%37
		publishTestFrame(t, testCrop(page, top, w, h))
		p.fbur(true)
		rects := p.readUpdate()
		for _, r := range rects {
			switch {
			case r.enc == rfbEncodingCopyRect:
				copies++
			case r.y >= vncChromeH:
				rawRows += r.h
			}
		}
		p.apply(fb, rects)
	}
	if copies < steps*3/4 {
		t.Fatalf("only %d of %d scroll steps produced a CopyRect", copies, steps)
	}
	if rawRows > steps*h/2 {
		t.Fatalf("repainted %d rows over %d steps, CopyRect saved little", rawRows, steps)
	}

	// one clean refresh into a fresh buffer is the ground truth
	p.fbur(false)
	ref := image.NewRGBA(image.Rect(0, 0, p.w, p.h))
	p.apply(ref, p.readUpdate())
	mean, worst, over := testDiff(fb, ref, 64)
	t.Logf("%d copies, %d rows repainted of %d, incremental vs refresh: mean %.2f worst %d over64 %d",
		copies, rawRows, steps*h, mean, worst, over)
	if mean > 8 || over*10000 > w*h*3 {
		t.Fatalf("incremental framebuffer drifted from a full refresh: mean %.2f worst %d, %d samples over 64",
			mean, worst, over)
	}
}

// A client that never asked for CopyRect must never be sent one
func TestProtoNoCopyRectUnlessOffered(t *testing.T) {
	addr := startTestServer(t)
	publishTestFrame(t, solidFrame(320, 200, 0x40, 0x40, 0x40))
	p := dialProto(t, addr)
	p.setEncodings(rfbEncodingRaw) // deliberately no CopyRect
	p.fbur(false)
	p.readUpdate()

	img := image.NewRGBA(image.Rect(0, 0, 320, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 320; x++ {
			o := img.PixOffset(x, y)
			img.Pix[o], img.Pix[o+1], img.Pix[o+2], img.Pix[o+3] = byte(y*3), byte(x), 0x80, 0xff
		}
	}
	publishTestFrame(t, img)
	p.fbur(true)
	for _, r := range p.readUpdate() { // readUpdate fails on anything unexpected
		if r.enc == rfbEncodingCopyRect {
			t.Fatal("server sent CopyRect to a client that did not offer it")
		}
	}
}

// TigerVNC style remote resize: the ack comes back inline and the framebuffer
// follows on the next update
func TestProtoDesktopResize(t *testing.T) {
	addr := startTestServer(t)
	publishTestFrame(t, solidFrame(320, 200, 0x55, 0x55, 0x55))
	p := dialProto(t, addr)
	p.setEncodings(rfbEncodingRaw, rfbEncodingExtDesk)

	// SetDesktopSize: type, pad, w, h, screens, pad, then one screen
	const nw, nh = 400, 260
	m := make([]byte, 8+16)
	m[0], m[5] = 251, 1
	binary.BigEndian.PutUint16(m[2:], nw)
	binary.BigEndian.PutUint16(m[4:], nh)
	p.c.Write(m)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		hdr := make([]byte, 4)
		p.read(hdr)
		if hdr[0] != 0 {
			t.Fatalf("unexpected server message %d", hdr[0])
		}
		n := int(binary.BigEndian.Uint16(hdr[2:]))
		for i := 0; i < n; i++ {
			rh := make([]byte, 12)
			p.read(rh)
			w := int(binary.BigEndian.Uint16(rh[4:]))
			h := int(binary.BigEndian.Uint16(rh[6:]))
			switch enc := int32(binary.BigEndian.Uint32(rh[8:])); enc {
			case rfbEncodingExtDesk:
				p.read(make([]byte, 4))
				p.read(make([]byte, 16))
				if w != nw || h != nh {
					t.Fatalf("resize acked as %dx%d, asked for %dx%d", w, h, nw, nh)
				}
				vncSrv.Lock()
				gotW, gotH := vncSrv.vw, vncSrv.vh
				vncSrv.Unlock()
				if gotW != nw || gotH != nh {
					t.Fatalf("server kept %dx%d after the resize request", gotW, gotH)
				}
				return
			case rfbEncodingRaw:
				p.read(make([]byte, w*h*4))
			default:
				t.Fatalf("unexpected encoding %d", enc)
			}
		}
	}
	t.Fatal("no ExtendedDesktopSize ack within the deadline")
}

// Every client message has to be consumed to exactly the right length. Getting
// one wrong desyncs the stream and the connection wedges rather than failing
// loudly, so each is sent followed by a request that must still be answered.
func TestProtoMessageFraming(t *testing.T) {
	addr := startTestServer(t)
	publishTestFrame(t, solidFrame(320, 200, 0x30, 0x30, 0x30))
	p := dialProto(t, addr)
	p.setEncodings(rfbEncodingRaw, rfbEncodingQemuKey)
	p.fbur(false)
	p.readUpdate()

	// SetPixelFormat: type, 3 pad, 16 byte format
	spf := make([]byte, 20)
	spf[4], spf[5], spf[7] = 32, 24, 1
	binary.BigEndian.PutUint16(spf[8:], 255)
	binary.BigEndian.PutUint16(spf[10:], 255)
	binary.BigEndian.PutUint16(spf[12:], 255)
	spf[14], spf[15], spf[16] = 16, 8, 0

	key := make([]byte, 8) // KeyEvent: type, down, 2 pad, u32 keysym
	key[0], key[1] = 4, 1
	binary.BigEndian.PutUint32(key[4:], 'a')

	ptr := make([]byte, 6) // PointerEvent: type, mask, u16 x, u16 y
	ptr[0] = 5
	binary.BigEndian.PutUint16(ptr[2:], 10)
	binary.BigEndian.PutUint16(ptr[4:], uint16(vncChromeH+10))

	cut := append(make([]byte, 8), []byte("hello wrp")...) // ClientCutText
	cut[0] = 6
	binary.BigEndian.PutUint32(cut[4:], 9)

	qemu := make([]byte, 12) // QEMU extended key: type, sub, u16 down, keysym, keycode
	qemu[0], qemu[1] = 255, 0
	binary.BigEndian.PutUint16(qemu[2:], 1)
	binary.BigEndian.PutUint32(qemu[4:], 'b')
	binary.BigEndian.PutUint32(qemu[8:], 0x30) // KeyB

	fence := []byte{248, 0, 0, 0, 0, 0, 0, 0, 3, 'a', 'b', 'c'}
	binary.BigEndian.PutUint32(fence[4:], 1<<31)

	for _, tc := range []struct {
		name string
		msg  []byte
	}{
		{"SetPixelFormat", spf},
		{"KeyEvent", key},
		{"PointerEvent", ptr},
		{"ClientCutText", cut},
		{"QemuExtendedKey", qemu},
		{"ClientFence", fence},
	} {
		p.c.Write(tc.msg)
		// a fence is echoed back before anything else, so drain that first
		if tc.name == "ClientFence" {
			hdr := make([]byte, 9)
			p.read(hdr)
			if hdr[0] != 248 {
				t.Fatalf("%s: expected a fence echo, got message %d", tc.name, hdr[0])
			}
			p.read(make([]byte, hdr[8]))
			continue
		}
		// SetPixelFormat forces a resend, so ask non incrementally either way
		p.fbur(false)
		if fr := frameRect(t, p.readUpdate()); fr.w != 320 {
			t.Fatalf("%s: stream desynced, frame rect came back %dx%d", tc.name, fr.w, fr.h)
		}
	}
}
