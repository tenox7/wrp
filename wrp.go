//
// WRP - Web Rendering Proxy
//
// Copyright (c) 2013-2018 Antoni Sawicki
// Copyright (c) 2019-2020 Google LLC
//

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"image"
	"image/gif"
	"image/png"
	"log"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/css"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/ericpauley/go-quantize/quantize"
)

var (
	version = "4.5"
	srv     http.Server
	ctx     context.Context
	cancel  context.CancelFunc
	img     = make(map[string]bytes.Buffer)
	ismap   = make(map[string]wrpReq)
	nodel   bool
	deftype string
	defgeom geom
)

type geom struct {
	w int64
	h int64
	c int64
}

// WRP Request
type wrpReq struct {
	U string  // url
	W int64   // width
	H int64   // height
	S float64 // scale
	C int64   // #colors
	X int64   // mouseX
	Y int64   // mouseY
	K string  // keys to send
	F string  // Fn buttons
	T string  // imgtype
	o http.ResponseWriter
	r *http.Request
}

// Parse HTML Form, Process Input Boxes, Etc.
func (w *wrpReq) parseForm() {
	w.r.ParseForm()
	w.U = w.r.FormValue("url")
	if len(w.U) > 1 && !strings.HasPrefix(w.U, "http") {
		w.U = fmt.Sprintf("http://www.google.com/search?q=%s", url.QueryEscape(w.U))
	}
	w.W, _ = strconv.ParseInt(w.r.FormValue("w"), 10, 64)
	w.H, _ = strconv.ParseInt(w.r.FormValue("h"), 10, 64)
	if w.W < 10 && w.H < 10 {
		w.W = defgeom.w
		w.H = defgeom.h
	}
	w.S, _ = strconv.ParseFloat(w.r.FormValue("s"), 64)
	if w.S < 0.1 {
		w.S = 1.0
	}
	w.C, _ = strconv.ParseInt(w.r.FormValue("c"), 10, 64)
	if w.C < 2 || w.C > 256 {
		w.C = defgeom.c
	}
	w.K = w.r.FormValue("k")
	w.F = w.r.FormValue("Fn")
	w.T = w.r.FormValue("t")
	if w.T != "gif" && w.T != "png" {
		w.T = deftype
	}
	log.Printf("%s WrpReq from Form: %+v\n", w.r.RemoteAddr, w)
}

// Display WP UI
// TODO: make this in to an external template
func (w wrpReq) printPage(bgcolor string) {
	var s string
	w.o.Header().Set("Cache-Control", "max-age=0")
	w.o.Header().Set("Expires", "-1")
	w.o.Header().Set("Pragma", "no-cache")
	w.o.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w.o, "<!-- Web Rendering Proxy Version %s -->\n", version)
	fmt.Fprintf(w.o, "<HTML>\n<HEAD><TITLE>WRP %s</TITLE></HEAD>\n<BODY BGCOLOR=\"%s\">\n", w.U, bgcolor)
	fmt.Fprintf(w.o, "<FORM ACTION=\"/\" METHOD=\"POST\">\n")
	fmt.Fprintf(w.o, "<INPUT TYPE=\"TEXT\" NAME=\"url\" VALUE=\"%s\" SIZE=\"20\">", w.U)
	fmt.Fprintf(w.o, "<INPUT TYPE=\"SUBMIT\" VALUE=\"Go\">\n")
	fmt.Fprintf(w.o, "<INPUT TYPE=\"SUBMIT\" NAME=\"Fn\" VALUE=\"Bk\">\n")
	fmt.Fprintf(w.o, "W <INPUT TYPE=\"TEXT\" NAME=\"w\" VALUE=\"%d\" SIZE=\"4\"> \n", w.W)
	fmt.Fprintf(w.o, "H <INPUT TYPE=\"TEXT\" NAME=\"h\" VALUE=\"%d\" SIZE=\"4\"> \n", w.H)
	fmt.Fprintf(w.o, "S <SELECT NAME=\"s\">\n")
	for _, v := range []float64{0.65, 0.75, 0.85, 0.95, 1.0, 1.05, 1.15, 1.25} {
		if v == w.S {
			s = "SELECTED"
		} else {
			s = ""
		}
		fmt.Fprintf(w.o, "<OPTION VALUE=\"%1.2f\" %s>%1.2f</OPTION>\n", v, s, v)
	}
	fmt.Fprintf(w.o, "</SELECT>\n")
	fmt.Fprintf(w.o, "T <SELECT NAME=\"t\">\n")
	for _, v := range []string{"gif", "png"} {
		if v == w.T {
			s = "SELECTED"
		} else {
			s = ""
		}
		fmt.Fprintf(w.o, "<OPTION VALUE=\"%s\" %s>%s</OPTION>\n", v, s, strings.ToUpper(v))
	}
	fmt.Fprintf(w.o, "</SELECT>\n")
	fmt.Fprintf(w.o, "C <INPUT TYPE=\"TEXT\" NAME=\"c\" VALUE=\"%d\" SIZE=\"3\">\n", w.C)
	fmt.Fprintf(w.o, "K <INPUT TYPE=\"TEXT\" NAME=\"k\" VALUE=\"\" SIZE=\"4\"> \n")
	fmt.Fprintf(w.o, "<INPUT TYPE=\"SUBMIT\" NAME=\"Fn\" VALUE=\"Bs\">\n")
	fmt.Fprintf(w.o, "<INPUT TYPE=\"SUBMIT\" NAME=\"Fn\" VALUE=\"Rt\">\n")
	fmt.Fprintf(w.o, "<INPUT TYPE=\"SUBMIT\" NAME=\"Fn\" VALUE=\"&lt;\">\n")
	fmt.Fprintf(w.o, "<INPUT TYPE=\"SUBMIT\" NAME=\"Fn\" VALUE=\"^\">\n")
	fmt.Fprintf(w.o, "<INPUT TYPE=\"SUBMIT\" NAME=\"Fn\" VALUE=\"v\">\n")
	fmt.Fprintf(w.o, "<INPUT TYPE=\"SUBMIT\" NAME=\"Fn\" VALUE=\"&gt;\" SIZE=\"1\">\n")
	fmt.Fprintf(w.o, "</FORM><BR>\n")
}

// Status bar below captured image
func (w wrpReq) printFooter(h string, s string) {
	fmt.Fprintf(w.o, "\n<P><FONT SIZE=\"-2\"><A HREF=\"/?url=https://github.com/tenox7/wrp/&w=%d&h=%d&s=%1.2f&c=%d&t=%s\">"+
		"Web Rendering Proxy Version %s</A> | <A HREF=\"/shutdown/\">Shutdown WRP</A> | "+
		"<A HREF=\"/\">Page Height: %s</A> | <A HREF=\"/\">Img Size: %s</A></FONT></BODY>\n</HTML>\n", w.W, w.H, w.S, w.C, w.T, version, h, s)
}

// Process HTTP requests to WRP '/' url
func pageServer(out http.ResponseWriter, req *http.Request) {
	log.Printf("%s Page Request for %s [%+v]\n", req.RemoteAddr, req.URL.Path, req.URL.RawQuery)
	var w wrpReq
	w.r = req
	w.o = out
	w.parseForm()
	if len(w.U) > 4 {
		w.navigate()
		w.capture()
	} else {
		w.printPage("#FFFFFF")
		w.printFooter("", "")
	}
}

// Process HTTP requests to ISMAP '/map/' url
func mapServer(out http.ResponseWriter, req *http.Request) {
	log.Printf("%s ISMAP Request for %s [%+v]\n", req.RemoteAddr, req.URL.Path, req.URL.RawQuery)
	w, ok := ismap[req.URL.Path]
	w.r = req
	w.o = out
	if !ok {
		fmt.Fprintf(out, "Unable to find map %s\n", req.URL.Path)
		log.Printf("Unable to find map %s\n", req.URL.Path)
		return
	}
	if !nodel {
		defer delete(ismap, req.URL.Path)
	}
	n, err := fmt.Sscanf(req.URL.RawQuery, "%d,%d", &w.X, &w.Y)
	if err != nil || n != 2 {
		fmt.Fprintf(out, "n=%d, err=%s\n", n, err)
		log.Printf("%s ISMAP n=%d, err=%s\n", req.RemoteAddr, n, err)
		return
	}
	log.Printf("%s WrpReq from ISMAP: %+v\n", req.RemoteAddr, w)
	if len(w.U) > 4 {
		w.navigate()
		w.capture()
	} else {
		w.printPage("#FFFFFF")
		w.printFooter("", "")
	}
}

// Process HTTP requests for images '/img/' url
func imgServer(out http.ResponseWriter, req *http.Request) {
	log.Printf("%s IMG Request for %s\n", req.RemoteAddr, req.URL.Path)
	imgbuf, ok := img[req.URL.Path]
	if !ok || imgbuf.Bytes() == nil {
		fmt.Fprintf(out, "Unable to find image %s\n", req.URL.Path)
		log.Printf("%s Unable to find image %s\n", req.RemoteAddr, req.URL.Path)
		return
	}
	if !nodel {
		defer delete(img, req.URL.Path)
	}
	if strings.HasPrefix(req.URL.Path, ".gif") {
		out.Header().Set("Content-Type", "image/gif")
	} else if strings.HasPrefix(req.URL.Path, ".png") {
		out.Header().Set("Content-Type", "image/png")
	}
	out.Header().Set("Content-Length", strconv.Itoa(len(imgbuf.Bytes())))
	out.Header().Set("Cache-Control", "max-age=0")
	out.Header().Set("Expires", "-1")
	out.Header().Set("Pragma", "no-cache")
	out.Write(imgbuf.Bytes())
	out.(http.Flusher).Flush()
}

// Process HTTP requests for Shutdown via '/shutdown/' url
func haltServer(out http.ResponseWriter, req *http.Request) {
	log.Printf("%s Shutdown Request for %s\n", req.RemoteAddr, req.URL.Path)
	out.Header().Set("Cache-Control", "max-age=0")
	out.Header().Set("Expires", "-1")
	out.Header().Set("Pragma", "no-cache")
	out.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(out, "Shutting down WRP...\n")
	out.(http.Flusher).Flush()
	time.Sleep(time.Second * 2)
	cancel()
	srv.Shutdown(context.Background())
	os.Exit(1)
}

// Process Keyboard and Mouse events or Navigate to the desired URL.
func (w wrpReq) navigate() {
	var err error
	// Mouse Click
	if w.X > 0 && w.Y > 0 {
		log.Printf("%s Mouse Click %d,%d\n", w.r.RemoteAddr, w.X, w.Y)
		err = chromedp.Run(ctx, chromedp.MouseClickXY(float64(w.X)/float64(w.S), float64(w.Y)/float64(w.S)))
		// Buttons
	} else if len(w.F) > 0 {
		log.Printf("%s Button %v\n", w.r.RemoteAddr, w.F)
		switch w.F {
		case "Bk":
			err = chromedp.Run(ctx, chromedp.NavigateBack())
		case "Bs":
			err = chromedp.Run(ctx, chromedp.KeyEvent("\b"))
		case "Rt":
			err = chromedp.Run(ctx, chromedp.KeyEvent("\r"))
		case "<":
			err = chromedp.Run(ctx, chromedp.KeyEvent("\u0302"))
		case "^":
			err = chromedp.Run(ctx, chromedp.KeyEvent("\u0304"))
		case "v":
			err = chromedp.Run(ctx, chromedp.KeyEvent("\u0301"))
		case ">":
			err = chromedp.Run(ctx, chromedp.KeyEvent("\u0303"))
		}
		// Keys
	} else if len(w.K) > 0 {
		log.Printf("%s Sending Keys: %#v\n", w.r.RemoteAddr, w.K)
		err = chromedp.Run(ctx, chromedp.KeyEvent(w.K))
		// Navigate to URL
	} else {
		log.Printf("%s Processing Capture Request for %s\n", w.r.RemoteAddr, w.U)
		err = chromedp.Run(ctx, chromedp.Navigate(w.U))
	}
	if err != nil {
		if err.Error() == "context canceled" {
			log.Printf("%s Contex cancelled, try again", w.r.RemoteAddr)
			fmt.Fprintf(w.o, "<BR>%s<BR> -- restarting, try again", err)
			ctx, cancel = chromedp.NewContext(context.Background())
		} else {
			log.Printf("%s %s", w.r.RemoteAddr, err)
			fmt.Fprintf(w.o, "<BR>%s<BR>", err)
		}
		return
	}
}

// Capture currently rendered web page to an image and fake ISMAP
func (w wrpReq) capture() {
	var err error
	var styles []*css.ComputedStyleProperty
	var r, g, b int
	var h int64
	var pngcap []byte
	chromedp.Run(ctx,
		emulation.SetDeviceMetricsOverride(int64(float64(w.W)/w.S), 10, w.S, false),
		chromedp.Sleep(time.Second*2),
		chromedp.Location(&w.U),
		chromedp.ComputedStyle("body", &styles, chromedp.ByQuery),
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, _, s, err := page.GetLayoutMetrics().Do(ctx)
			if err == nil {
				h = int64(math.Ceil(s.Height))
			}
			return nil
		}),
	)
	for _, style := range styles {
		if style.Name == "background-color" {
			fmt.Sscanf(style.Value, "rgb(%d,%d,%d)", &r, &g, &b)
		}
	}
	log.Printf("%s Landed on: %s, Height: %v\n", w.r.RemoteAddr, w.U, h)
	w.printPage(fmt.Sprintf("#%02X%02X%02X", r, g, b))
	if w.H == 0 && h > 0 {
		chromedp.Run(ctx, emulation.SetDeviceMetricsOverride(int64(float64(w.W)/w.S), h+30, w.S, false))
	} else {
		chromedp.Run(ctx, emulation.SetDeviceMetricsOverride(int64(float64(w.W)/w.S), int64(float64(w.H)/w.S), w.S, false))
	}
	err = chromedp.Run(ctx, chromedp.CaptureScreenshot(&pngcap))
	if err != nil {
		log.Printf("%s Failed to capture screenshot: %s\n", w.r.RemoteAddr, err)
		fmt.Fprintf(w.o, "<BR>Unable to capture screenshot:<BR>%s<BR>\n", err)
		return
	}
	seq := rand.Intn(9999)
	imgpath := fmt.Sprintf("/img/%04d.%s", seq, w.T)
	mappath := fmt.Sprintf("/map/%04d.map", seq)
	ismap[mappath] = w
	var ssize string
	var sw, sh int
	if w.T == "gif" {
		i, err := png.Decode(bytes.NewReader(pngcap))
		if err != nil {
			log.Printf("%s Failed to decode screenshot: %s\n", w.r.RemoteAddr, err)
			fmt.Fprintf(w.o, "<BR>Unable to decode page screenshot:<BR>%s<BR>\n", err)
			return
		}
		var gifbuf bytes.Buffer
		err = gif.Encode(&gifbuf, i, &gif.Options{NumColors: int(w.C), Quantizer: quantize.MedianCutQuantizer{}})
		if err != nil {
			log.Printf("%s Failed to encode GIF: %s\n", w.r.RemoteAddr, err)
			fmt.Fprintf(w.o, "<BR>Unable to encode GIF:<BR>%s<BR>\n", err)
			return
		}
		img[imgpath] = gifbuf
		ssize = fmt.Sprintf("%.1f MB", float32(len(gifbuf.Bytes()))/1024.0/1024.0)
		sw = i.Bounds().Max.X
		sh = i.Bounds().Max.Y
		log.Printf("%s Encoded GIF image: %s, Size: %s, Colors: %d, %dx%d\n", w.r.RemoteAddr, imgpath, ssize, w.C, sw, sh)
	} else if w.T == "png" {
		pngbuf := bytes.NewBuffer(pngcap)
		img[imgpath] = *pngbuf
		cfg, _, _ := image.DecodeConfig(pngbuf)
		ssize = fmt.Sprintf("%.1f MB", float32(len(pngbuf.Bytes()))/1024.0/1024.0)
		sw = cfg.Width
		sh = cfg.Height
		log.Printf("%s Got PNG image: %s, Size: %s, %dx%d\n", w.r.RemoteAddr, imgpath, ssize, sw, sh)
	}
	fmt.Fprintf(w.o, "<A HREF=\"%s\"><IMG SRC=\"%s\" BORDER=\"0\" ALT=\"Url: %s, Size: %s\" WIDTH=\"%d\" HEIGHT=\"%d\" ISMAP></A>", mappath, imgpath, w.U, ssize, sw, sh)
	w.printFooter(fmt.Sprintf("%d PX", h), ssize)
	log.Printf("%s Done with caputure for %s\n", w.r.RemoteAddr, w.U)
}

// Main...
func main() {
	var addr, fgeom string
	var head, headless bool
	var debug bool
	var err error
	flag.StringVar(&addr, "l", ":8080", "Listen address:port, default :8080")
	flag.BoolVar(&head, "h", false, "Headed mode - display browser window")
	flag.BoolVar(&debug, "d", false, "Debug ChromeDP")
	flag.BoolVar(&nodel, "n", false, "Do not free maps and images after use")
	flag.StringVar(&deftype, "t", "gif", "Image type: gif|png")
	flag.StringVar(&fgeom, "g", "1152x600x256", "Geometry: width x height x colors, height can be 0 for unlimited")
	flag.Parse()
	if head {
		headless = false
	} else {
		headless = true
	}
	n, err := fmt.Sscanf(fgeom, "%dx%dx%d", &defgeom.w, &defgeom.h, &defgeom.c)
	if err != nil || n != 3 {
		log.Fatalf("Unable to parse -g geometry flag / %s", err)
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", headless),
		chromedp.Flag("hide-scrollbars", false),
	)
	actx, acancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer acancel()
	if debug {
		ctx, cancel = chromedp.NewContext(actx, chromedp.WithDebugf(log.Printf))
	} else {
		ctx, cancel = chromedp.NewContext(actx)
	}
	defer cancel()
	rand.Seed(time.Now().UnixNano())
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		log.Printf("Interrupt - shutting down.")
		cancel()
		srv.Shutdown(context.Background())
		os.Exit(1)
	}()
	http.HandleFunc("/", pageServer)
	http.HandleFunc("/map/", mapServer)
	http.HandleFunc("/img/", imgServer)
	http.HandleFunc("/shutdown/", haltServer)
	http.HandleFunc("/favicon.ico", http.NotFound)
	log.Printf("Web Rendering Proxy Version %s\n", version)
	log.Printf("Starting WRP http server on %s\n", addr)
	srv.Addr = addr
	err = srv.ListenAndServe()
	if err != nil {
		log.Fatal(err)
	}
}
