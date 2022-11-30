//
// WRP - Web Rendering Proxy
//
// Copyright (c) 2013-2018 Antoni Sawicki
// Copyright (c) 2019-2022 Google LLC
//

package main

import (
	"bytes"
	"context"
	"embed"
	"flag"
	"fmt"
	"html/template"
	"image"
	"image/color/palette"
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
	"github.com/soniakeys/quant/median"
)

var (
	version  = "4.5.3"
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

// go:embed *.html
var fs embed.FS

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
	w       http.ResponseWriter
	r       *http.Request
}

// Parse HTML Form, Process Input Boxes, Etc.
func (rq *wrpReq) parseForm() {
	rq.r.ParseForm()
	rq.url = rq.r.FormValue("url")
	if len(rq.url) > 1 && !strings.HasPrefix(rq.url, "http") {
		rq.url = fmt.Sprintf("http://www.google.com/search?q=%s", url.QueryEscape(rq.url))
	}
	rq.width, _ = strconv.ParseInt(rq.r.FormValue("w"), 10, 64)
	rq.height, _ = strconv.ParseInt(rq.r.FormValue("h"), 10, 64)
	if rq.width < 10 && rq.height < 10 {
		rq.width = defGeom.w
		rq.height = defGeom.h
	}
	rq.zoom, _ = strconv.ParseFloat(rq.r.FormValue("z"), 64)
	if rq.zoom < 0.1 {
		rq.zoom = 1.0
	}
	rq.colors, _ = strconv.ParseInt(rq.r.FormValue("c"), 10, 64)
	if rq.colors < 2 || rq.colors > 256 {
		rq.colors = defGeom.c
	}
	rq.keys = rq.r.FormValue("k")
	rq.buttons = rq.r.FormValue("Fn")
	rq.imgType = rq.r.FormValue("t")
	if rq.imgType != "gif" && rq.imgType != "png" {
		rq.imgType = defType
	}
	log.Printf("%s WrpReq from UI Form: %+v\n", rq.r.RemoteAddr, rq)
}

// Display WP UI
func (rq *wrpReq) printHTML(p printParams) {
	rq.w.Header().Set("Cache-Control", "max-age=0")
	rq.w.Header().Set("Expires", "-1")
	rq.w.Header().Set("Pragma", "no-cache")
	rq.w.Header().Set("Content-Type", "text/html")
	data := uiData{
		Version:    version,
		URL:        rq.url,
		BgColor:    p.bgColor,
		Width:      rq.width,
		Height:     rq.height,
		NColors:    rq.colors,
		Zoom:       rq.zoom,
		ImgType:    rq.imgType,
		ImgSize:    p.imgSize,
		ImgWidth:   p.imgWidth,
		ImgHeight:  p.imgHeight,
		ImgURL:     p.imgURL,
		MapURL:     p.mapURL,
		PageHeight: p.pageHeight,
	}
	err := htmlTmpl.Execute(rq.w, data)
	if err != nil {
		log.Fatal(err)
	}
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
	if len(rq.keys) > 0 {
		log.Printf("%s Sending Keys: %#v\n", rq.r.RemoteAddr, rq.keys)
		return chromedp.KeyEvent(rq.keys)
	}
	// Navigate to URL
	log.Printf("%s Processing Capture Request for %s\n", rq.r.RemoteAddr, rq.url)
	return chromedp.Navigate(rq.url)
}

// Process Keyboard and Mouse events or Navigate to the desired URL.
func (rq *wrpReq) navigate() {
	err := chromedp.Run(ctx, rq.action())
	if err != nil {
		if err.Error() == "context canceled" {
			log.Printf("%s Contex cancelled, try again", rq.r.RemoteAddr)
			fmt.Fprintf(rq.w, "<BR>%s<BR> -- restarting, try again", err)
			ctx, cancel = chromedp.NewContext(context.Background())
			return
		}
		log.Printf("%s %s", rq.r.RemoteAddr, err)
		fmt.Fprintf(rq.w, "<BR>%s<BR>", err)
	}
}

// https://github.com/chromedp/chromedp/issues/979
func chromedpCaptureScreenshot(res *[]byte, h int64) chromedp.Action {
	if res == nil {
		panic("res cannot be nil")
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

func gifPalette(i image.Image, n int64) image.Image {
	switch n {
	case 2:
		i = halfgone.FloydSteinbergDitherer{}.Apply(halfgone.ImageToGray(i))
	case 216:
		var FastGifLut = [256]int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5}
		r := i.Bounds()
		// NOTE: the color index computation below works only for palette.WebSafe!
		p := image.NewPaletted(r, palette.WebSafe)
		if i64, ok := i.(image.RGBA64Image); ok {
			for y := r.Min.Y; y < r.Max.Y; y++ {
				for x := r.Min.X; x < r.Max.X; x++ {
					c := i64.RGBA64At(x, y)
					r6 := FastGifLut[c.R>>8]
					g6 := FastGifLut[c.G>>8]
					b6 := FastGifLut[c.B>>8]
					p.SetColorIndex(x, y, uint8(36*r6+6*g6+b6))
				}
			}
		} else {
			for y := r.Min.Y; y < r.Max.Y; y++ {
				for x := r.Min.X; x < r.Max.X; x++ {
					c := i.At(x, y)
					r, g, b, _ := c.RGBA()
					r6 := FastGifLut[r&0xff]
					g6 := FastGifLut[g&0xff]
					b6 := FastGifLut[b&0xff]
					p.SetColorIndex(x, y, uint8(36*r6+6*g6+b6))
				}
			}
		}
		i = p
	default:
		q := median.Quantizer(n)
		i = q.Paletted(i)
	}
	return i
}

// Capture currently rendered web page to an image and fake ISMAP
func (rq *wrpReq) capture() {
	var err error
	var styles []*css.ComputedStyleProperty
	var r, g, b int
	var h int64
	var pngcap []byte
	chromedp.Run(ctx,
		emulation.SetDeviceMetricsOverride(int64(float64(rq.width)/rq.zoom), 10, rq.zoom, false),
		chromedp.Location(&rq.url),
		chromedp.ComputedStyle("body", &styles, chromedp.ByQuery),
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, _, _, _, _, s, err := page.GetLayoutMetrics().Do(ctx)
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
	log.Printf("%s Landed on: %s, Height: %v\n", rq.r.RemoteAddr, rq.url, h)
	height := int64(float64(rq.height) / rq.zoom)
	if rq.height == 0 && h > 0 {
		height = h + 30
	}
	chromedp.Run(
		ctx, emulation.SetDeviceMetricsOverride(int64(float64(rq.width)/rq.zoom), height, rq.zoom, false),
		chromedp.Sleep(time.Second*2), // TODO(tenox): totally lame, find a better way to determine if page is rendered
	)
	// Capture screenshot...
	err = chromedp.Run(ctx, chromedpCaptureScreenshot(&pngcap, rq.height))
	if err != nil {
		if err.Error() == "context canceled" {
			log.Printf("%s Contex cancelled, try again", rq.r.RemoteAddr)
			fmt.Fprintf(rq.w, "<BR>%s<BR> -- restarting, try again", err)
			ctx, cancel = chromedp.NewContext(context.Background())
			return
		}
		log.Printf("%s Failed to capture screenshot: %s\n", rq.r.RemoteAddr, err)
		fmt.Fprintf(rq.w, "<BR>Unable to capture screenshot:<BR>%s<BR>\n", err)
		return
	}
	seq := rand.Intn(9999)
	imgpath := fmt.Sprintf("/img/%04d.%s", seq, rq.imgType)
	mappath := fmt.Sprintf("/map/%04d.map", seq)
	ismap[mappath] = *rq
	var ssize string
	var iw, ih int
	switch rq.imgType {
	case "gif":
		i, err := png.Decode(bytes.NewReader(pngcap))
		if err != nil {
			log.Printf("%s Failed to decode PNG screenshot: %s\n", rq.r.RemoteAddr, err)
			fmt.Fprintf(rq.w, "<BR>Unable to decode page PNG screenshot:<BR>%s<BR>\n", err)
			return
		}
		st := time.Now()
		var gifbuf bytes.Buffer
		err = gif.Encode(&gifbuf, gifPalette(i, rq.colors), &gif.Options{})
		if err != nil {
			log.Printf("%s Failed to encode GIF: %s\n", rq.r.RemoteAddr, err)
			fmt.Fprintf(rq.w, "<BR>Unable to encode GIF:<BR>%s<BR>\n", err)
			return
		}
		img[imgpath] = gifbuf
		ssize = fmt.Sprintf("%.0f KB", float32(len(gifbuf.Bytes()))/1024.0)
		iw = i.Bounds().Max.X
		ih = i.Bounds().Max.Y
		log.Printf("%s Encoded GIF image: %s, Size: %s, Colors: %d, Res: %dx%d, Time: %vms\n", rq.r.RemoteAddr, imgpath, ssize, rq.colors, iw, ih, time.Since(st).Milliseconds())
	case "png":
		pngbuf := bytes.NewBuffer(pngcap)
		img[imgpath] = *pngbuf
		cfg, _, _ := image.DecodeConfig(pngbuf)
		ssize = fmt.Sprintf("%.0f KB", float32(len(pngbuf.Bytes()))/1024.0)
		iw = cfg.Width
		ih = cfg.Height
		log.Printf("%s Got PNG image: %s, Size: %s, Res: %dx%d\n", rq.r.RemoteAddr, imgpath, ssize, iw, ih)
	}
	rq.printHTML(printParams{
		bgColor:    fmt.Sprintf("#%02X%02X%02X", r, g, b),
		pageHeight: fmt.Sprintf("%d PX", h),
		imgSize:    ssize,
		imgURL:     imgpath,
		mapURL:     mappath,
		imgWidth:   iw,
		imgHeight:  ih,
	})
	log.Printf("%s Done with capture for %s\n", rq.r.RemoteAddr, rq.url)
}

// Process HTTP requests to WRP '/' url
func pageServer(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s Page Request for %s [%+v]\n", r.RemoteAddr, r.URL.Path, r.URL.RawQuery)
	rq := wrpReq{
		r: r,
		w: w,
	}
	rq.parseForm()
	if len(rq.url) < 4 {
		rq.printHTML(printParams{bgColor: "#FFFFFF"})
		return
	}
	rq.navigate()
	rq.capture()
}

// Process HTTP requests to ISMAP '/map/' url
func mapServer(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s ISMAP Request for %s [%+v]\n", r.RemoteAddr, r.URL.Path, r.URL.RawQuery)
	rq, ok := ismap[r.URL.Path]
	rq.r = r
	rq.w = w
	if !ok {
		fmt.Fprintf(w, "Unable to find map %s\n", r.URL.Path)
		log.Printf("Unable to find map %s\n", r.URL.Path)
		return
	}
	if !noDel {
		defer delete(ismap, r.URL.Path)
	}
	n, err := fmt.Sscanf(r.URL.RawQuery, "%d,%d", &rq.mouseX, &rq.mouseY)
	if err != nil || n != 2 {
		fmt.Fprintf(w, "n=%d, err=%s\n", n, err)
		log.Printf("%s ISMAP n=%d, err=%s\n", r.RemoteAddr, n, err)
		return
	}
	log.Printf("%s WrpReq from ISMAP: %+v\n", r.RemoteAddr, rq)
	if len(rq.url) < 4 {
		rq.printHTML(printParams{bgColor: "#FFFFFF"})
		return
	}
	rq.navigate()
	rq.capture()
}

// Process HTTP requests for images '/img/' url
func imgServer(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s IMG Request for %s\n", r.RemoteAddr, r.URL.Path)
	imgbuf, ok := img[r.URL.Path]
	if !ok || imgbuf.Bytes() == nil {
		fmt.Fprintf(w, "Unable to find image %s\n", r.URL.Path)
		log.Printf("%s Unable to find image %s\n", r.RemoteAddr, r.URL.Path)
		return
	}
	if !noDel {
		defer delete(img, r.URL.Path)
	}
	switch {
	case strings.HasPrefix(r.URL.Path, ".gif"):
		w.Header().Set("Content-Type", "image/gif")
	case strings.HasPrefix(r.URL.Path, ".png"):
		w.Header().Set("Content-Type", "image/png")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(imgbuf.Bytes())))
	w.Header().Set("Cache-Control", "max-age=0")
	w.Header().Set("Expires", "-1")
	w.Header().Set("Pragma", "no-cache")
	w.Write(imgbuf.Bytes())
	w.(http.Flusher).Flush()
}

// Process HTTP requests for Shutdown via '/shutdown/' url
func haltServer(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s Shutdown Request for %s\n", r.RemoteAddr, r.URL.Path)
	w.Header().Set("Cache-Control", "max-age=0")
	w.Header().Set("Expires", "-1")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "Shutting down WRP...\n")
	w.(http.Flusher).Flush()
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
		goto builtin
	}
	tmpl, err = ioutil.ReadAll(fh)
	if err != nil {
		goto builtin
	}
	log.Printf("Got UI template from %v file\n", t)
	return string(tmpl)

builtin:
	fhs, err := fs.Open("/wrp.html")
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
	flag.StringVar(&fgeom, "g", "1152x600x216", "Geometry: width x height x colors, height can be 0 for unlimited")
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
	switch debug {
	case true:
		ctx, cancel = chromedp.NewContext(actx, chromedp.WithDebugf(log.Printf))
	default:
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
