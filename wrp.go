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

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/chromedp"

	"github.com/ericpauley/go-quantize/quantize"
)

// Ismap for server side processing
type Ismap struct {
	xmin int64
	ymin int64
	xmax int64
	ymax int64
	url  string
}

var (
	version = "3.0"
	srv     http.Server
	ctx     context.Context
	cancel  context.CancelFunc
	gifmap  = make(map[string]bytes.Buffer)
	ismap   = make(map[string][]Ismap)
)

func pageServer(out http.ResponseWriter, req *http.Request) {
	req.ParseForm()
	u := req.FormValue("url")
	var istr string
	var ion string
	var i bool
	if req.FormValue("i") == "on" {
		istr = "CHECKED"
		i = true
		ion = "&i=on"
	} else {
		istr = ""
		i = false
	}
	p, _ := strconv.ParseInt(req.FormValue("p"), 10, 64)
	if req.FormValue("pg") == "Dn" {
		p++
	} else if req.FormValue("pg") == "Up" {
		p--
	} else {
		p = 0
	}
	w, _ := strconv.ParseInt(req.FormValue("w"), 10, 64)
	if w < 10 {
		w = 1024
	}
	h, _ := strconv.ParseInt(req.FormValue("h"), 10, 64)
	if h < 10 {
		h = 768
	}
	s, _ := strconv.ParseFloat(req.FormValue("s"), 64)
	if s < 0.1 {
		s = 1.0
	}
	c, _ := strconv.ParseInt(req.FormValue("c"), 10, 64)
	if c < 2 || c > 256 {
		c = 256
	}
	log.Printf("%s Page Request for url=\"%s\" [%s]\n", req.RemoteAddr, u, req.URL.Path)
	out.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(out, "<!-- Web Rendering Proxy Version %s -->\n", version)
	fmt.Fprintf(out, "<HTML>\n<HEAD><TITLE>WRP %s</TITLE></HEAD>\n<BODY BGCOLOR=\"#F0F0F0\">\n", u)
	fmt.Fprintf(out, "<FORM ACTION=\"/\"><INPUT TYPE=\"TEXT\" NAME=\"url\" VALUE=\"%s\" SIZE=\"20\">", u)
	fmt.Fprintf(out, "<INPUT TYPE=\"SUBMIT\" VALUE=\"Go\"> \n")
	fmt.Fprintf(out, "<INPUT TYPE=\"SUBMIT\" NAME=\"pg\" VALUE=\"Up\"> \n")
	fmt.Fprintf(out, "<INPUT TYPE=\"TEXT\" NAME=\"p\" VALUE=\"%d\" SIZE=\"2\"> \n", p)
	fmt.Fprintf(out, "<INPUT TYPE=\"SUBMIT\" NAME=\"pg\" VALUE=\"Dn\"> \n")
	fmt.Fprintf(out, "I <INPUT TYPE=\"CHECKBOX\" NAME=\"i\" %s> \n", istr)
	fmt.Fprintf(out, "W <INPUT TYPE=\"TEXT\" NAME=\"w\" VALUE=\"%d\" SIZE=\"4\"> \n", w)
	fmt.Fprintf(out, "H <INPUT TYPE=\"TEXT\" NAME=\"h\" VALUE=\"%d\" SIZE=\"4\"> \n", h)
	fmt.Fprintf(out, "S <INPUT TYPE=\"TEXT\" NAME=\"s\" VALUE=\"%1.2f\" SIZE=\"3\"> \n", s)
	fmt.Fprintf(out, "C <INPUT TYPE=\"TEXT\" NAME=\"c\" VALUE=\"%d\" SIZE=\"3\"> \n", c)
	fmt.Fprintf(out, "</FORM><BR>\n")
	if len(u) > 1 {
		if strings.HasPrefix(u, "http") {
			capture(u, w, h, s, int(c), p, i, req.RemoteAddr, out)
		} else {
			capture(fmt.Sprintf("http://www.google.com/search?q=%s", url.QueryEscape(u)), w, h, s, int(c), p, i, req.RemoteAddr, out)
		}
	} else {
		fmt.Fprintf(out, "No URL or search query specified")
	}
	fmt.Fprintf(out, "\n<P><A HREF=\"/?url=https://github.com/tenox7/wrp/&w=%d&h=%d&s=%1.2f&c=%d%s\">Web Rendering Proxy Version %s</A> | <A HREF=\"/shutdown/\">Shutdown WRP</A></BODY>\n</HTML>\n", w, h, s, c, ion, version)
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

func mapServer(out http.ResponseWriter, req *http.Request) {
	log.Printf("%s ISMAP Request for %s [%+v]\n", req.RemoteAddr, req.URL.Path, req.URL.RawQuery)
	var loc string
	var x, y int64
	n, err := fmt.Sscanf(req.URL.RawQuery, "%d,%d", &x, &y)
	if err != nil || n != 2 {
		fmt.Fprintf(out, "n=%d, err=%s\n", n, err)
		log.Printf("%s ISMAP n=%d, err=%s\n", req.RemoteAddr, n, err)
		return
	}
	is, ok := ismap[req.URL.Path]
	if !ok || is == nil {
		fmt.Fprintf(out, "Unable to find map %s\n", req.URL.Path)
		log.Printf("Unable to find map %s\n", req.URL.Path)
		return
	}
	defer delete(ismap, req.URL.Path)
	for _, i := range is {
		if x >= i.xmin && x <= i.xmax && y >= i.ymin && y <= i.ymax {
			loc = i.url
		}
	}
	if len(loc) < 1 {
		loc = is[0].url
	}
	log.Printf("%s ISMAP Redirect to: http://%s%s\n", req.RemoteAddr, req.Context().Value(http.LocalAddrContextKey), loc)
	http.Redirect(out, req, fmt.Sprintf("http://%s%s", req.Context().Value(http.LocalAddrContextKey), loc), 301)
}

func capture(gourl string, w int64, h int64, s float64, co int, p int64, i bool, c string, out http.ResponseWriter) {
	var nodes []*cdp.Node
	ctxx := chromedp.FromContext(ctx)
	var pngbuf []byte
	var gifbuf bytes.Buffer
	var loc string
	var res *runtime.RemoteObject
	is := make([]Ismap, 0)
	var ion string

	log.Printf("%s Processing Capture Request for %s\n", c, gourl)

	// Run ChromeDP Magic
	err := chromedp.Run(ctx,
		emulation.SetDeviceMetricsOverride(int64(float64(w)/s), int64(float64(h)/s), s, false),
		chromedp.Navigate(gourl),
		chromedp.Evaluate(fmt.Sprintf("window.scrollTo(0, %d);", p*int64(float64(h)*float64(0.9))), &res),
		chromedp.Sleep(time.Second*1),
		chromedp.CaptureScreenshot(&pngbuf),
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

	log.Printf("%s Landed on: %s, Nodes: %d\n", c, loc, len(nodes))

	// Process Screenshot Image
	bytes.NewReader(pngbuf).Seek(0, 0)
	img, err := png.Decode(bytes.NewReader(pngbuf))
	if err != nil {
		log.Printf("%s Failed to decode screenshot: %s\n", c, err)
		fmt.Fprintf(out, "<BR>Unable to decode page screenshot:<BR>%s<BR>\n", err)
		return
	}
	gifbuf.Reset()
	err = gif.Encode(&gifbuf, img, &gif.Options{NumColors: co, Quantizer: quantize.MedianCutQuantizer{}})
	if err != nil {
		log.Printf("%s Failed to encode GIF: %s\n", c, err)
		fmt.Fprintf(out, "<BR>Unable to encode GIF:<BR>%s<BR>\n", err)
		return
	}
	seq := rand.Intn(9999)
	imgpath := fmt.Sprintf("/img/%04d.gif", seq)
	mappath := fmt.Sprintf("/map/%04d.map", seq)
	log.Printf("%s Encoded GIF image: %s, Size: %dKB, Colors: %d\n", c, imgpath, len(gifbuf.Bytes())/1024, co)
	gifmap[imgpath] = gifbuf

	// Process Nodes
	tctx, cancel := context.WithTimeout(ctx, time.Second*2) // a context with timeout is needed for pages without nodes
	defer cancel()
	chromedp.Run(tctx, chromedp.Nodes("a", &nodes, chromedp.ByQueryAll))
	base, _ := url.Parse(loc)
	if i {
		fmt.Fprintf(out, "<A HREF=\"%s\"><IMG SRC=\"%s\" ALT=\"wrp\" BORDER=\"0\" ISMAP></A>", mappath, imgpath)
		is = append(is, Ismap{xmin: -1, xmax: -1, ymin: -1, ymax: -1, url: fmt.Sprintf("/?url=%s&w=%d&h=%d&s=%1.2f&c=%d&i=on", loc, w, h, s, co)})
		ion = "&i=on"
	} else {
		fmt.Fprintf(out, "<IMG SRC=\"%s\" ALT=\"wrp\" BORDER=\"0\" USEMAP=\"#map\">\n<MAP NAME=\"map\">\n", imgpath)
	}

	for _, n := range nodes {
		b, err := dom.GetBoxModel().WithNodeID(n.NodeID).Do(cdp.WithExecutor(ctx, ctxx.Target))
		if err != nil {
			continue
		}
		tgt, err := base.Parse(n.AttributeValue("href"))
		if err != nil {
			continue
		}
		target := fmt.Sprintf("/?url=%s&w=%d&h=%d&s=%1.2f&c=%d%s", tgt, w, h, s, co, ion) // no page# here

		if len(b.Padding) > 6 && len(target) > 7 {
			if i {
				is = append(is, Ismap{
					xmin: int64(b.Padding[0] * s), ymin: int64(b.Padding[1] * s),
					xmax: int64(b.Padding[4] * s), ymax: int64(b.Padding[5] * s),
					url: target})
			} else {
				fmt.Fprintf(out, "<AREA SHAPE=\"RECT\" COORDS=\"%.f,%.f,%.f,%.f\" ALT=\"%s\" TITLE=\"%s\" HREF=\"%s\">\n",
					b.Padding[0]*s, b.Padding[1]*s, b.Padding[4]*s, b.Padding[5]*s, n.AttributeValue("href"), n.AttributeValue("href"), target)
			}
		}
	}

	if i {
		log.Printf("%s Encoded ISMAP %s\n", c, mappath)
	} else {
		fmt.Fprintf(out, "</MAP>\n")
	}
	ismap[mappath] = is
	out.(http.Flusher).Flush()
	log.Printf("%s Done with caputure for %s\n", c, gourl)
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
	http.HandleFunc("/img/", imgServer)
	http.HandleFunc("/map/", mapServer)
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
