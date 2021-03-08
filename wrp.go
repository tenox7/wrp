//
// WRP - Web Rendering Proxy
//
// Copyright (c) 2013-2018 Antoni Sawicki
// Copyright (c) 2019-2021 Google LLC
//

//go:generate statik -f -src=. -include=wrp.html

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"html/template"
	"image"
	"image/gif"
	"image/png"
	"io/ioutil"
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

	"github.com/MaxHalford/halfgone"
	"github.com/chromedp/cdproto/css"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/ericpauley/go-quantize/quantize"
	"github.com/rakyll/statik/fs"
	_ "github.com/tenox7/wrp/statik"
)

var (
	version  = "4.5.2"
	srv      http.Server
	ctx      context.Context
	cancel   context.CancelFunc
	img      = make(map[string]bytes.Buffer)
	ismap    = make(map[string]wrpReq)
	noDel    bool
	defType  string
	defGeom  geom
	htmlTmpl *template.Template
)

type geom struct {
	w int64
	h int64
	c int64
}

// Data for html template
type uiData struct {
	Version    string
	URL        string
	BgColor    string
	NColors    int64
	Width      int64
	Height     int64
	Zoom       float64
	ImgType    string
	ImgURL     string
	ImgSize    string
	ImgWidth   int
	ImgHeight  int
	MapURL     string
	PageHeight string
}

// Parameters for HTML print function
type printParams struct {
	bgColor    string
	pageHeight string
	imgSize    string
	imgURL     string
	mapURL     string
	imgWidth   int
	imgHeight  int
}

// WRP Request
type wrpReq struct {
	url     string  // url
	width   int64   // width
	height  int64   // height
	zoom    float64 // zoom/scale
	colors  int64   // #colors
	mouseX  int64   // mouseX
	mouseY  int64   // mouseY
	keys    string  // keys to send
	buttons string  // Fn buttons
	imgType string  // imgtype
	out     http.ResponseWriter
	req     *http.Request
}

// Parse HTML Form, Process Input Boxes, Etc.
func parseForm(w *wrpReq) {
	w.req.ParseForm()
	w.url = w.req.FormValue("url")
	if len(w.url) > 1 && !strings.HasPrefix(w.url, "http") {
		w.url = fmt.Sprintf("http://www.google.com/search?q=%s", url.QueryEscape(w.url))
	}
	w.width, _ = strconv.ParseInt(w.req.FormValue("w"), 10, 64)
	w.height, _ = strconv.ParseInt(w.req.FormValue("h"), 10, 64)
	if w.width < 10 && w.height < 10 {
		w.width = defGeom.w
		w.height = defGeom.h
	}
	w.zoom, _ = strconv.ParseFloat(w.req.FormValue("z"), 64)
	if w.zoom < 0.1 {
		w.zoom = 1.0
	}
	w.colors, _ = strconv.ParseInt(w.req.FormValue("c"), 10, 64)
	if w.colors < 2 || w.colors > 256 {
		w.colors = defGeom.c
	}
	w.keys = w.req.FormValue("k")
	w.buttons = w.req.FormValue("Fn")
	w.imgType = w.req.FormValue("t")
	if w.imgType != "gif" && w.imgType != "png" {
		w.imgType = defType
	}
	log.Printf("%s WrpReq from UI Form: %+v\n", w.req.RemoteAddr, w)
}

// Display WP UI
func printHTML(w wrpReq, p printParams) {
	w.out.Header().Set("Cache-Control", "max-age=0")
	w.out.Header().Set("Expires", "-1")
	w.out.Header().Set("Pragma", "no-cache")
	w.out.Header().Set("Content-Type", "text/html")
	data := uiData{
		Version:    version,
		URL:        w.url,
		BgColor:    p.bgColor,
		Width:      w.width,
		Height:     w.height,
		NColors:    w.colors,
		Zoom:       w.zoom,
		ImgType:    w.imgType,
		ImgSize:    p.imgSize,
		ImgWidth:   p.imgWidth,
		ImgHeight:  p.imgHeight,
		ImgURL:     p.imgURL,
		MapURL:     p.mapURL,
		PageHeight: p.pageHeight,
	}
	err := htmlTmpl.Execute(w.out, data)
	if err != nil {
		log.Fatal(err)
	}
}

// Determine what action to take
func action(w wrpReq) chromedp.Action {
	// Mouse Click
	if w.mouseX > 0 && w.mouseY > 0 {
		log.Printf("%s Mouse Click %d,%d\n", w.req.RemoteAddr, w.mouseX, w.mouseY)
		return chromedp.MouseClickXY(float64(w.mouseX)/float64(w.zoom), float64(w.mouseY)/float64(w.zoom))
	}
	// Buttons
	if len(w.buttons) > 0 {
		log.Printf("%s Button %v\n", w.req.RemoteAddr, w.buttons)
		switch w.buttons {
		case "Bk":
			return chromedp.NavigateBack()
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
		}
	}
	// Keys
	if len(w.keys) > 0 {
		log.Printf("%s Sending Keys: %#v\n", w.req.RemoteAddr, w.keys)
		return chromedp.KeyEvent(w.keys)
	}
	// Navigate to URL
	log.Printf("%s Processing Capture Request for %s\n", w.req.RemoteAddr, w.url)
	return chromedp.Navigate(w.url)
}

// Process Keyboard and Mouse events or Navigate to the desired URL.
func navigate(w wrpReq) {
	err := chromedp.Run(ctx, action(w))
	if err != nil {
		if err.Error() == "context canceled" {
			log.Printf("%s Contex cancelled, try again", w.req.RemoteAddr)
			fmt.Fprintf(w.out, "<BR>%s<BR> -- restarting, try again", err)
			ctx, cancel = chromedp.NewContext(context.Background())
			return
		}
		log.Printf("%s %s", w.req.RemoteAddr, err)
		fmt.Fprintf(w.out, "<BR>%s<BR>", err)
	}
}

// Capture currently rendered web page to an image and fake ISMAP
func capture(w wrpReq) {
	var err error
	var styles []*css.ComputedStyleProperty
	var r, g, b int
	var h int64
	var pngcap []byte
	chromedp.Run(ctx,
		emulation.SetDeviceMetricsOverride(int64(float64(w.width)/w.zoom), 10, w.zoom, false),
		chromedp.Location(&w.url),
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
	log.Printf("%s Landed on: %s, Height: %v\n", w.req.RemoteAddr, w.url, h)
	height := int64(float64(w.height) / w.zoom)
	if w.height == 0 && h > 0 {
		height = h + 30
	}
	chromedp.Run(ctx, emulation.SetDeviceMetricsOverride(int64(float64(w.width)/w.zoom), height, w.zoom, false))
	// Capture screenshot...
	err = chromedp.Run(ctx,
		chromedp.Sleep(time.Second*2),
		chromedp.CaptureScreenshot(&pngcap),
	)
	if err != nil {
		if err.Error() == "context canceled" {
			log.Printf("%s Contex cancelled, try again", w.req.RemoteAddr)
			fmt.Fprintf(w.out, "<BR>%s<BR> -- restarting, try again", err)
			ctx, cancel = chromedp.NewContext(context.Background())
			return
		}
		log.Printf("%s Failed to capture screenshot: %s\n", w.req.RemoteAddr, err)
		fmt.Fprintf(w.out, "<BR>Unable to capture screenshot:<BR>%s<BR>\n", err)
		return
	}
	seq := rand.Intn(9999)
	imgpath := fmt.Sprintf("/img/%04d.%s", seq, w.imgType)
	mappath := fmt.Sprintf("/map/%04d.map", seq)
	ismap[mappath] = w
	var ssize string
	var iw, ih int
	switch w.imgType {
	case "gif":
		i, err := png.Decode(bytes.NewReader(pngcap))
		if err != nil {
			log.Printf("%s Failed to decode screenshot: %s\n", w.req.RemoteAddr, err)
			fmt.Fprintf(w.out, "<BR>Unable to decode page screenshot:<BR>%s<BR>\n", err)
			return
		}
		if w.colors == 2 {
			gray := halfgone.ImageToGray(i)
			i = halfgone.FloydSteinbergDitherer{}.Apply(gray)
		}
		var gifbuf bytes.Buffer
		err = gif.Encode(&gifbuf, i, &gif.Options{NumColors: int(w.colors), Quantizer: quantize.MedianCutQuantizer{}})
		if err != nil {
			log.Printf("%s Failed to encode GIF: %s\n", w.req.RemoteAddr, err)
			fmt.Fprintf(w.out, "<BR>Unable to encode GIF:<BR>%s<BR>\n", err)
			return
		}
		img[imgpath] = gifbuf
		ssize = fmt.Sprintf("%.0f KB", float32(len(gifbuf.Bytes()))/1024.0)
		iw = i.Bounds().Max.X
		ih = i.Bounds().Max.Y
		log.Printf("%s Encoded GIF image: %s, Size: %s, Colors: %d, %dx%d\n", w.req.RemoteAddr, imgpath, ssize, w.colors, iw, ih)
	case "png":
		pngbuf := bytes.NewBuffer(pngcap)
		img[imgpath] = *pngbuf
		cfg, _, _ := image.DecodeConfig(pngbuf)
		ssize = fmt.Sprintf("%.0f KB", float32(len(pngbuf.Bytes()))/1024.0)
		iw = cfg.Width
		ih = cfg.Height
		log.Printf("%s Got PNG image: %s, Size: %s, %dx%d\n", w.req.RemoteAddr, imgpath, ssize, iw, ih)
	}
	printHTML(w, printParams{
		bgColor:    fmt.Sprintf("#%02X%02X%02X", r, g, b),
		pageHeight: fmt.Sprintf("%d PX", h),
		imgSize:    ssize,
		imgURL:     imgpath,
		mapURL:     mappath,
		imgWidth:   iw,
		imgHeight:  ih,
	})
	log.Printf("%s Done with capture for %s\n", w.req.RemoteAddr, w.url)
}

// Process HTTP requests to WRP '/' url
func pageServer(out http.ResponseWriter, req *http.Request) {
	log.Printf("%s Page Request for %s [%+v]\n", req.RemoteAddr, req.URL.Path, req.URL.RawQuery)
	var w wrpReq
	w.req = req
	w.out = out
	parseForm(&w)
	if len(w.url) < 4 {
		printHTML(w, printParams{bgColor: "#FFFFFF"})
		return
	}
	navigate(w)
	capture(w)
}

// Process HTTP requests to ISMAP '/map/' url
func mapServer(out http.ResponseWriter, req *http.Request) {
	log.Printf("%s ISMAP Request for %s [%+v]\n", req.RemoteAddr, req.URL.Path, req.URL.RawQuery)
	w, ok := ismap[req.URL.Path]
	w.req = req
	w.out = out
	if !ok {
		fmt.Fprintf(out, "Unable to find map %s\n", req.URL.Path)
		log.Printf("Unable to find map %s\n", req.URL.Path)
		return
	}
	if !noDel {
		defer delete(ismap, req.URL.Path)
	}
	n, err := fmt.Sscanf(req.URL.RawQuery, "%d,%d", &w.mouseX, &w.mouseY)
	if err != nil || n != 2 {
		fmt.Fprintf(out, "n=%d, err=%s\n", n, err)
		log.Printf("%s ISMAP n=%d, err=%s\n", req.RemoteAddr, n, err)
		return
	}
	log.Printf("%s WrpReq from ISMAP: %+v\n", req.RemoteAddr, w)
	if len(w.url) < 4 {
		printHTML(w, printParams{bgColor: "#FFFFFF"})
		return
	}
	navigate(w)
	capture(w)
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
	if !noDel {
		defer delete(img, req.URL.Path)
	}
	switch {
	case strings.HasPrefix(req.URL.Path, ".gif"):
		out.Header().Set("Content-Type", "image/gif")
	case strings.HasPrefix(req.URL.Path, ".png"):
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

// returns html template, either from html file or built-in
func tmpl(t string) string {
	var tmpl []byte
	fh, err := os.Open(t)
	if err != nil {
		goto statik
	}
	tmpl, err = ioutil.ReadAll(fh)
	if err != nil {
		goto statik
	}
	log.Printf("Got UI template from %v file\n", t)
	return string(tmpl)

statik:
	sfs, err := fs.New()
	if err != nil {
		log.Fatal(err)
	}
	fhs, err := sfs.Open("/wrp.html")
	if err != nil {
		log.Fatal(err)
	}

	tmpl, err = ioutil.ReadAll(fhs)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Got UI template from built-in\n")
	return string(tmpl)
}

// Main...
func main() {
	var addr, fgeom, tHTML string
	var headless bool
	var debug bool
	var err error
	flag.StringVar(&addr, "l", ":8080", "Listen address:port, default :8080")
	flag.BoolVar(&headless, "h", true, "Headless mode - hide browser window")
	flag.BoolVar(&debug, "d", false, "Debug ChromeDP")
	flag.BoolVar(&noDel, "n", false, "Do not free maps and images after use")
	flag.StringVar(&defType, "t", "gif", "Image type: gif|png")
	flag.StringVar(&fgeom, "g", "1152x600x256", "Geometry: width x height x colors, height can be 0 for unlimited")
	flag.StringVar(&tHTML, "ui", "wrp.html", "HTML template file for the UI")
	flag.Parse()
	if len(os.Getenv("PORT")) > 0 {
		addr = ":" + os.Getenv(("PORT"))
	}
	n, err := fmt.Sscanf(fgeom, "%dx%dx%d", &defGeom.w, &defGeom.h, &defGeom.c)
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
	log.Printf("Args: %q", os.Args)
	log.Printf("Default Img Type: %v, Geometry: %+v", defType, defGeom)

	htmlTmpl, err = template.New("wrp.html").Parse(tmpl(tHTML))
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Starting WRP http server on %s\n", addr)
	srv.Addr = addr
	err = srv.ListenAndServe()
	if err != nil {
		log.Fatal(err)
	}
}
