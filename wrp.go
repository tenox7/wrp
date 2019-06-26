//
// WRP - Web Rendering Proxy
//
// Copyright (c) 2013-2018 Antoni Sawicki
// Copyright (c) 2019 Google LLC
//

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"image/gif"
	"image/png"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/runtime"

	"github.com/chromedp/chromedp"

	"github.com/ericpauley/go-quantize/quantize"
)

var (
	version = "3.0"
	srv     http.Server
	ctx     context.Context
	cancel  context.CancelFunc
	gifmap  = make(map[string]bytes.Buffer)
	ismap   = make(map[string]Params)
)

// Params - Page Configuration Parameters
type Params struct {
	U string  // url
	P int64   // page
	W int64   // width
	H int64   // height
	S float64 // scale
	C int64   // #colors
}

func (p *Params) parseForm(req *http.Request) {
	req.ParseForm()
	p.U = req.FormValue("url")
	if len(p.U) > 1 && !strings.HasPrefix(p.U, "http") {
		p.U = fmt.Sprintf("http://www.google.com/search?q=%s", url.QueryEscape(p.U))
	}
	p.P, _ = strconv.ParseInt(req.FormValue("p"), 10, 64)
	if req.FormValue("pg") == "Dn" {
		p.P++
	} else if req.FormValue("pg") == "Up" {
		p.P--
	} else {
		p.P = 0
	}
	p.W, _ = strconv.ParseInt(req.FormValue("w"), 10, 64)
	if p.W < 10 {
		p.W = 1024
	}
	p.H, _ = strconv.ParseInt(req.FormValue("h"), 10, 64)
	if p.H < 10 {
		p.H = 768
	}
	p.S, _ = strconv.ParseFloat(req.FormValue("s"), 64)
	if p.S < 0.1 {
		p.S = 1.0
	}
	p.C, _ = strconv.ParseInt(req.FormValue("c"), 10, 64)
	if p.C < 2 || p.C > 256 {
		p.C = 256
	}
	log.Printf("Params from Form: %+v\n", p)
}

func (p Params) printPage(out http.ResponseWriter) {
	out.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(out, "<!-- Web Rendering Proxy Version %s -->\n", version)
	fmt.Fprintf(out, "<HTML>\n<HEAD><TITLE>WRP %s</TITLE></HEAD>\n<BODY BGCOLOR=\"#F0F0F0\">\n", p.U)
	fmt.Fprintf(out, "<FORM ACTION=\"/\"><INPUT TYPE=\"TEXT\" NAME=\"url\" VALUE=\"%s\" SIZE=\"20\">", p.U)
	fmt.Fprintf(out, "<INPUT TYPE=\"SUBMIT\" VALUE=\"Go\"> \n")
	fmt.Fprintf(out, "<INPUT TYPE=\"SUBMIT\" NAME=\"pg\" VALUE=\"Up\"> \n")
	fmt.Fprintf(out, "<INPUT TYPE=\"TEXT\" NAME=\"p\" VALUE=\"%d\" SIZE=\"2\"> \n", p.P)
	fmt.Fprintf(out, "<INPUT TYPE=\"SUBMIT\" NAME=\"pg\" VALUE=\"Dn\"> \n")
	fmt.Fprintf(out, "W <INPUT TYPE=\"TEXT\" NAME=\"w\" VALUE=\"%d\" SIZE=\"4\"> \n", p.W)
	fmt.Fprintf(out, "H <INPUT TYPE=\"TEXT\" NAME=\"h\" VALUE=\"%d\" SIZE=\"4\"> \n", p.H)
	fmt.Fprintf(out, "S <INPUT TYPE=\"TEXT\" NAME=\"s\" VALUE=\"%1.2f\" SIZE=\"3\"> \n", p.S)
	fmt.Fprintf(out, "C <INPUT TYPE=\"TEXT\" NAME=\"c\" VALUE=\"%d\" SIZE=\"3\"> \n", p.C)
	fmt.Fprintf(out, "</FORM><BR>\n")
}

func (p Params) printFooter(out http.ResponseWriter) {
	fmt.Fprintf(out, "\n<P><A HREF=\"/?url=https://github.com/tenox7/wrp/&w=%d&h=%d&s=%1.2f&c=%d\">"+
		"Web Rendering Proxy Version %s</A> | <A HREF=\"/shutdown/\">Shutdown WRP</A></BODY>\n</HTML>\n", p.W, p.H, p.S, p.C, version)
}

func pageServer(out http.ResponseWriter, req *http.Request) {
	log.Printf("%s Page Request for %s [%+v]\n", req.RemoteAddr, req.URL.Path, req.URL.RawQuery)
	var p Params
	p.parseForm(req)
	p.printPage(out)
	if len(p.U) > 4 {
		p.capture(req.RemoteAddr, out)
	}
	p.printFooter(out)
}

func mapServer(out http.ResponseWriter, req *http.Request) {
	log.Printf("%s ISMAP Request for %s [%+v]\n", req.RemoteAddr, req.URL.Path, req.URL.RawQuery)
	var x, y int64
	n, err := fmt.Sscanf(req.URL.RawQuery, "%d,%d", &x, &y)
	if err != nil || n != 2 {
		fmt.Fprintf(out, "n=%d, err=%s\n", n, err)
		log.Printf("%s ISMAP n=%d, err=%s\n", req.RemoteAddr, n, err)
		return
	}
	p, ok := ismap[req.URL.Path]
	if !ok {
		fmt.Fprintf(out, "Unable to find map %s\n", req.URL.Path)
		log.Printf("Unable to find map %s\n", req.URL.Path)
		return
	}
	defer delete(ismap, req.URL.Path)
	log.Printf("%s Params from ISMAP: %+v\n", req.RemoteAddr, p)
	p.printPage(out)
	if len(p.U) > 4 {
		p.capture(req.RemoteAddr, out)
	}
	p.printFooter(out)
}

func imgServer(out http.ResponseWriter, req *http.Request) {
	log.Printf("%s IMG Request for %s\n", req.RemoteAddr, req.URL.Path)
	gifbuf, ok := gifmap[req.URL.Path]
	if !ok || gifbuf.Bytes() == nil {
		fmt.Fprintf(out, "Unable to find image %s\n", req.URL.Path)
		log.Printf("Unable to find image %s\n", req.URL.Path)
		return
	}
	defer delete(gifmap, req.URL.Path)
	out.Header().Set("Content-Type", "image/gif")
	out.Header().Set("Content-Length", strconv.Itoa(len(gifbuf.Bytes())))
	out.Write(gifbuf.Bytes())
	out.(http.Flusher).Flush()
}

func (p Params) capture(c string, out http.ResponseWriter) {
	var pngbuf []byte
	var gifbuf bytes.Buffer
	var loc string
	var res *runtime.RemoteObject

	log.Printf("%s Processing Capture Request for %s\n", c, p.U)

	// Run ChromeDP Magic
	err := chromedp.Run(ctx,
		emulation.SetDeviceMetricsOverride(int64(float64(p.W)/p.S), int64(float64(p.H)/p.S), p.S, false),
		chromedp.Navigate(p.U),
		chromedp.Evaluate(fmt.Sprintf("window.scrollTo(0, %d);", p.P*int64(float64(p.H)*float64(0.9))), &res),
		chromedp.Sleep(time.Second*1),
		chromedp.Location(&loc))

	if err != nil {
		if err.Error() == "context canceled" {
			log.Printf("%s Contex cancelled, try again", c)
			fmt.Fprintf(out, "<BR>%s<BR> -- restarting, try again", err)
			ctx, cancel = chromedp.NewContext(context.Background())
		} else {
			log.Printf("%s %s", c, err)
			fmt.Fprintf(out, "<BR>%s<BR>", err)
		}
		return
	}
	log.Printf("%s Landed on: %s\n", c, loc)
	p.U = loc

	// Process Screenshot Image
	err = chromedp.Run(ctx, chromedp.CaptureScreenshot(&pngbuf))
	if err != nil {
		log.Printf("%s Failed to capture screenshot: %s\n", c, err)
		fmt.Fprintf(out, "<BR>Unable to capture screenshot:<BR>%s<BR>\n", err)
		return
	}
	bytes.NewReader(pngbuf).Seek(0, 0)
	img, err := png.Decode(bytes.NewReader(pngbuf))
	if err != nil {
		log.Printf("%s Failed to decode screenshot: %s\n", c, err)
		fmt.Fprintf(out, "<BR>Unable to decode page screenshot:<BR>%s<BR>\n", err)
		return
	}
	gifbuf.Reset()
	err = gif.Encode(&gifbuf, img, &gif.Options{NumColors: int(p.C), Quantizer: quantize.MedianCutQuantizer{}})
	if err != nil {
		log.Printf("%s Failed to encode GIF: %s\n", c, err)
		fmt.Fprintf(out, "<BR>Unable to encode GIF:<BR>%s<BR>\n", err)
		return
	}

	// Compose map and gif
	seq := rand.Intn(9999)
	imgpath := fmt.Sprintf("/img/%04d.gif", seq)
	mappath := fmt.Sprintf("/map/%04d.map", seq)
	gifmap[imgpath] = gifbuf
	ismap[mappath] = p
	log.Printf("%s Encoded GIF image: %s, Size: %dKB, Colors: %d\n", c, imgpath, len(gifbuf.Bytes())/1024, p.C)
	fmt.Fprintf(out, "<A HREF=\"%s\"><IMG SRC=\"%s\" ALT=\"wrp\" BORDER=\"0\" ISMAP></A>", mappath, imgpath)
	log.Printf("%s Done with caputure for %s\n", c, p.U)
}

func haltServer(out http.ResponseWriter, req *http.Request) {
	log.Printf("%s Shutdown Request for %s\n", req.RemoteAddr, req.URL.Path)
	defer cancel()
	srv.Shutdown(context.Background())
}

func main() {
	var addr string
	var head, headless bool
	var debug bool
	flag.StringVar(&addr, "l", ":8080", "Listen address:port, default :8080")
	flag.BoolVar(&head, "h", false, "Headed mode - display browser window")
	flag.BoolVar(&debug, "d", false, "Debug ChromeDP")
	flag.Parse()
	if head {
		headless = false
	} else {
		headless = true
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", headless),
	)
	actx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	if debug {
		ctx, cancel = chromedp.NewContext(actx, chromedp.WithDebugf(log.Printf))
	} else {
		ctx, cancel = chromedp.NewContext(actx)
	}
	defer cancel()
	rand.Seed(time.Now().UnixNano())
	http.HandleFunc("/", pageServer)
	http.HandleFunc("/map/", mapServer)
	http.HandleFunc("/img/", imgServer)
	http.HandleFunc("/shutdown/", haltServer)
	http.HandleFunc("/favicon.ico", http.NotFound)
	log.Printf("Web Rendering Proxy Version %s\n", version)
	log.Printf("Starting WRP http server on %s\n", addr)
	srv.Addr = addr
	err := srv.ListenAndServe()
	if err != nil {
		log.Fatal(err)
	}
}
