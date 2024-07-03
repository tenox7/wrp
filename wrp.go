//
// WRP - Web Rendering Proxy
//
// Copyright (c) 2013-2024 Antoni Sawicki
// Copyright (c) 2019-2024 Google LLC
//

package main

import (
	"bytes"
	"context"
	"embed"
	"flag"
	"fmt"
	"image"
	"image/color/palette"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/MaxHalford/halfgone"
	"github.com/soniakeys/quant/median"
)

const version = "4.8.0"

var (
	addr       = flag.String("l", ":8080", "Listen address:port, default :8080")
	headless   = flag.Bool("h", true, "Headless mode / hide browser window (default true)")
	noDel      = flag.Bool("n", false, "Do not free maps and images after use")
	defType    = flag.String("t", "gif", "Image type: png|gif|jpg")
	wrpMode    = flag.String("m", "ismap", "WRP Mode: ismap|html")
	defImgSize = flag.Int64("is", 200, "html mode default image size")
	jpgQual    = flag.Int("q", 75, "Jpeg image quality, default 75%") // TODO: this should be form dropdown when jpeg is selected as image type
	fgeom      = flag.String("g", "1152x600x216", "Geometry: width x height x colors, height can be 0 for unlimited")
	htmFnam    = flag.String("ui", "wrp.html", "HTML template file for the UI")
	delay      = flag.Duration("s", 2*time.Second, "Delay/sleep after page is rendered and before screenshot is taken")
	userAgent  = flag.String("ua", "", "override chrome user agent")
)

var (
	srv         http.Server
	actx, ctx   context.Context
	acncl, cncl context.CancelFunc
	img         = make(map[string]bytes.Buffer)
	ismap       = make(map[string]wrpReq)
	defGeom     geom
	htmlTmpl    *template.Template
)

//go:embed *.html
var fs embed.FS

type geom struct {
	w int64
	h int64
	c int64
}

// Data for html template
type uiData struct {
	Version    string
	WrpMode    string
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
	MaxSize    int64
	MapURL     string
	PageHeight string
	TeXT       string
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
	text       string
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
	wrpMode string  // mode ismap/html
	maxSize int64   // image max size for html mode
	imgOpt  int64
	w       http.ResponseWriter
	r       *http.Request
}

// Parse HTML Form, Process Input Boxes, Etc.
func (rq *wrpReq) parseForm() {
	rq.r.ParseForm()
	rq.wrpMode = rq.r.FormValue("m")
	if rq.wrpMode == "" {
		rq.wrpMode = *wrpMode
	}
	rq.url = rq.r.FormValue("url")
	if len(rq.url) > 1 && !strings.HasPrefix(rq.url, "http") {
		rq.url = fmt.Sprintf("http://www.google.com/search?q=%s", url.QueryEscape(rq.url))
	}
	// TODO: implement atoiOrZero
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
	rq.colors, _ = strconv.ParseInt(rq.r.FormValue("c"), 10, 64) // TODO: this needs to be jpeg quality as well
	if rq.colors < 2 || rq.colors > 256 {                        // ... but maybe not because of this?
		rq.colors = defGeom.c
	}
	rq.keys = rq.r.FormValue("k")
	rq.buttons = rq.r.FormValue("Fn")
	rq.maxSize, _ = strconv.ParseInt(rq.r.FormValue("s"), 10, 64)
	if rq.maxSize == 0 {
		rq.maxSize = *defImgSize
	}
	rq.imgType = rq.r.FormValue("t")
	switch rq.imgType {
	case "png":
	case "gif":
		rq.imgOpt = defGeom.c
	case "jpg":
		rq.imgOpt = int64(*jpgQual)
	default:
		rq.imgType = *defType
		rq.imgOpt = 80 // TODO: fixme, this needs to be different based on image type
	}
	log.Printf("%s WrpReq from UI Form: %+v\n", rq.r.RemoteAddr, rq)
}

// Display WP UI
func (rq *wrpReq) printHTML(p printParams) {
	rq.w.Header().Set("Cache-Control", "max-age=0")
	rq.w.Header().Set("Expires", "-1")
	rq.w.Header().Set("Pragma", "no-cache")
	rq.w.Header().Set("Content-Type", "text/html")
	if p.bgColor == "" {
		p.bgColor = "#FFFFFF"
	}
	data := uiData{
		Version:    version,
		WrpMode:    rq.wrpMode,
		URL:        rq.url,
		BgColor:    p.bgColor,
		Width:      rq.width,
		Height:     rq.height,
		NColors:    rq.colors, // TODO: this needs to be also jpeg quality
		Zoom:       rq.zoom,
		MaxSize:    rq.maxSize,
		ImgType:    rq.imgType,
		ImgSize:    p.imgSize,
		ImgWidth:   p.imgWidth,
		ImgHeight:  p.imgHeight,
		ImgURL:     p.imgURL,
		MapURL:     p.mapURL,
		PageHeight: p.pageHeight,
		TeXT:       p.text,
	}
	err := htmlTmpl.Execute(rq.w, data)
	if err != nil {
		fmt.Fprintf(rq.w, "Error: %v", err)
	}
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
	rq.navigate() // TODO: if error from navigate do not capture
	if rq.wrpMode == "html" {
		rq.captureMarkdown()
		return
	}
	rq.captureScreenshot()
}

// Process HTTP requests for images '/img/' url
// TODO: merge this with html mode IMGZ
func imgServer(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s IMG Request for %s\n", r.RemoteAddr, r.URL.Path)
	imgBuf, ok := img[r.URL.Path]
	if !ok || imgBuf.Bytes() == nil {
		fmt.Fprintf(w, "Unable to find image %s\n", r.URL.Path)
		log.Printf("%s Unable to find image %s\n", r.RemoteAddr, r.URL.Path)
		return
	}
	if !*noDel {
		defer delete(img, r.URL.Path)
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

// Process HTTP requests for Shutdown via '/shutdown/' url
func haltServer(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s Shutdown Request for %s\n", r.RemoteAddr, r.URL.Path)
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "Shutting down WRP...\n")
	w.(http.Flusher).Flush()
	time.Sleep(time.Second * 2)
	cncl()
	acncl()
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
	defer fh.Close()

	tmpl, err = io.ReadAll(fh)
	if err != nil {
		goto builtin
	}
	log.Printf("Got HTML UI template from %v file, size %v \n", t, len(tmpl))
	return string(tmpl)

builtin:
	fhs, err := fs.Open("wrp.html")
	if err != nil {
		log.Fatal(err)
	}
	defer fhs.Close()

	tmpl, err = io.ReadAll(fhs)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Got HTML UI template from embed\n")
	return string(tmpl)
}

// Print my own IP addresses
func printIPs(b string) {
	ap := strings.Split(b, ":")
	if len(ap) < 1 {
		log.Fatal("Wrong format of ipaddress:port")
	}
	log.Printf("Listen address: %v", b)
	if ap[0] != "" && ap[0] != "0.0.0.0" {
		return
	}
	a, err := net.InterfaceAddrs()
	if err != nil {
		log.Print("Unable to get interfaces: ", err)
		return
	}
	var m string
	for _, i := range a {
		n, ok := i.(*net.IPNet)
		if !ok || n.IP.IsLoopback() || strings.Contains(n.IP.String(), ":") {
			continue
		}
		m = m + n.IP.String() + " "
	}
	log.Print("My IP addresses: ", m)
}

// Main
func main() {
	var err error
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	flag.Parse()
	log.Printf("Web Rendering Proxy Version %s (%v)\n", version, runtime.GOARCH)
	log.Printf("Args: %q", os.Args)
	if len(os.Getenv("PORT")) > 0 {
		*addr = ":" + os.Getenv(("PORT"))
	}
	printIPs(*addr)
	n, err := fmt.Sscanf(*fgeom, "%dx%dx%d", &defGeom.w, &defGeom.h, &defGeom.c)
	if err != nil || n != 3 {
		log.Fatalf("Unable to parse -g geometry flag / %s", err)
	}

	cncl, acncl = chromedpStart()
	defer cncl()
	defer acncl()

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		log.Printf("Interrupt - shutting down.")
		cncl()
		acncl()
		srv.Shutdown(context.Background())
		os.Exit(1)
	}()

	http.HandleFunc("/", pageServer)
	http.HandleFunc("/map/", mapServer)
	http.HandleFunc("/img/", imgServer)
	http.HandleFunc(imgZpfx, imgServerZ)
	http.HandleFunc("/shutdown/", haltServer)
	http.HandleFunc("/favicon.ico", http.NotFound)

	log.Printf("Default Img Type: %v, Geometry: %+v", *defType, defGeom)

	htmlTmpl, err = template.New("wrp.html").Parse(tmpl(*htmFnam))
	if err != nil {
		log.Fatal(err)
	}

	log.Print("Starting WRP http server")
	srv.Addr = *addr
	err = srv.ListenAndServe()
	if err != nil {
		log.Fatal(err)
	}
}
