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
	_ "image"
	"image/gif"
	"image/png"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/runtime"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/chromedp"
)

var (
	ctx    context.Context
	cancel context.CancelFunc
	gifmap = make(map[string]bytes.Buffer)
)

func pageServer(out http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	u := r.FormValue("url")
	var istr string
	var i bool
	if r.FormValue("i") == "on" {
		istr = "CHECKED"
		i = true
	} else {
		istr = ""
		i = false
	}
	y, _ := strconv.ParseInt(r.FormValue("y"), 10, 64)
	w, _ := strconv.ParseInt(r.FormValue("w"), 10, 64)
	if w < 10 {
		w = 1024
	}
	h, _ := strconv.ParseInt(r.FormValue("h"), 10, 64)
	if h < 10 {
		h = 768
	}
	s, _ := strconv.ParseFloat(r.FormValue("s"), 64)
	if s < 0.1 {
		s = 1.0
	}
	log.Printf("%s Page Reqest for url=\"%s\" [%s]\n", r.RemoteAddr, u, r.URL.Path)
	out.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(out, "<HTML>\n<HEAD><TITLE>WRP %s</TITLE>\n<BODY BGCOLOR=\"#F0F0F0\">", u)
	fmt.Fprintf(out, "<FORM ACTION=\"/\">URL/Search: <INPUT TYPE=\"TEXT\" NAME=\"url\" VALUE=\"%s\" SIZE=\"40\">", u)
	fmt.Fprintf(out, "<INPUT TYPE=\"SUBMIT\" VALUE=\"Go\"><P>\n")
	fmt.Fprintf(out, "ISMAP:<INPUT TYPE=\"CHECKBOX\" NAME=\"i\" %s> [%v]\n", istr, i)
	fmt.Fprintf(out, "Width:<INPUT TYPE=\"TEXT\" NAME=\"w\" VALUE=\"%d\" SIZE=\"4\"> \n", w)
	fmt.Fprintf(out, "Height:<INPUT TYPE=\"TEXT\" NAME=\"h\" VALUE=\"%d\" SIZE=\"4\"> \n", h)
	fmt.Fprintf(out, "Scale:<INPUT TYPE=\"TEXT\" NAME=\"s\" VALUE=\"%1.2f\" SIZE=\"3\"> \n", s)
	fmt.Fprintf(out, "Scroll:<INPUT TYPE=\"TEXT\" NAME=\"y\" VALUE=\"%d\" SIZE=\"4\"> \n", y)
	fmt.Fprintf(out, "</FORM><P>")
	if len(u) > 4 {
		if strings.HasPrefix(u, "http") {
			capture(u, w, h, s, y, out)
		} else {
			capture(fmt.Sprintf("http://www.google.com/search?q=%s", url.QueryEscape(u)), w, h, s, y, out)
		}
	} else {
		fmt.Fprintf(out, "No URL or search query specified")
	}
	fmt.Fprintf(out, "</BODY>\n</HTML>\n")
}

func imgServer(out http.ResponseWriter, req *http.Request) {
	log.Printf("%s Img Request for %s\n", req.RemoteAddr, req.URL.Path)
	gifbuf := gifmap[req.URL.Path]
	defer delete(gifmap, req.URL.Path)
	out.Header().Set("Content-Type", "image/gif")
	out.Header().Set("Content-Length", strconv.Itoa(len(gifbuf.Bytes())))
	out.Write(gifbuf.Bytes())
	out.(http.Flusher).Flush()
}

func haltServer(out http.ResponseWriter, req *http.Request) {
	log.Printf("%s Shutdown request received [%s]\n", req.RemoteAddr, req.URL.Path)
	out.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(out, "WRP Shutdown")
	out.(http.Flusher).Flush()
	cancel()
	os.Exit(0)
}

func capture(gourl string, w int64, h int64, s float64, y int64, out http.ResponseWriter) {
	var nodes []*cdp.Node
	ctxx := chromedp.FromContext(ctx)
	var pngbuf []byte
	var gifbuf bytes.Buffer
	var loc string
	var res *runtime.RemoteObject

	log.Printf("Processing Caputure Request for %s\n", gourl)

	// Run ChromeDP Magic
	err := chromedp.Run(ctx,
		emulation.SetDeviceMetricsOverride(w, h, s, false),
		chromedp.Navigate(gourl),
		chromedp.Evaluate(fmt.Sprintf("window.scrollTo(0, %d);", y), &res),
		chromedp.Sleep(time.Second*1),
		chromedp.CaptureScreenshot(&pngbuf),
		chromedp.Location(&loc),
		chromedp.Nodes("a", &nodes, chromedp.ByQueryAll))

	if err != nil {
		log.Printf("%s", err)
		fmt.Fprintf(out, "<BR>%s<BR>", err)
		return
	}

	log.Printf("Landed on: %s, Nodes: %d\n", loc, len(nodes))

	// Process Screenshot Image
	bytes.NewReader(pngbuf).Seek(0, 0)
	img, err := png.Decode(bytes.NewReader(pngbuf))
	if err != nil {
		log.Printf("Failed to decode screenshot: %s\n", err)
		fmt.Fprintf(out, "<BR>Unable to decode page screenshot:<BR>%s<BR>\n", err)
		return
	}
	gifbuf.Reset()
	err = gif.Encode(&gifbuf, img, nil)
	if err != nil {
		log.Printf("Failed to encode GIF: %s\n", err)
		fmt.Fprintf(out, "<BR>Unable to encode GIF:<BR>%s<BR>\n", err)
		return
	}
	imgpath := fmt.Sprintf("/img/%04d.gif", rand.Intn(9999))
	gifmap[imgpath] = gifbuf

	// Process Nodes
	base, _ := url.Parse(loc)
	fmt.Fprintf(out, "<IMG SRC=\"%s\" ALT=\"wrp\" USEMAP=\"#map\">\n<MAP NAME=\"map\">\n", imgpath)
	log.Printf("Image path will be: %s", imgpath)

	for _, n := range nodes {
		b, err := dom.GetBoxModel().WithNodeID(n.NodeID).Do(cdp.WithExecutor(ctx, ctxx.Target))
		if err != nil {
			continue
		}
		tgt, err := base.Parse(n.AttributeValue("href"))
		if err != nil {
			continue
		}
		target := fmt.Sprintf("/?url=%s&w=%d&h=%d&s=%1.2f&y=%d", tgt, w, h, s, y)

		if len(b.Content) > 6 && len(target) > 7 {
			fmt.Fprintf(out, "<AREA SHAPE=\"RECT\" COORDS=\"%.f,%.f,%.f,%.f\" ALT=\"%s\" TITLE=\"%s\" HREF=\"%s\">\n",
				b.Content[0]*s, b.Content[1]*s, b.Content[4]*s, b.Content[5]*s, n.AttributeValue("href"), n.AttributeValue("href"), target)
		}
	}

	fmt.Fprintf(out, "</MAP>\n")
	out.(http.Flusher).Flush()
	log.Printf("Done with caputure for %s\n", gourl)
}

func main() {
	ctx, cancel = chromedp.NewContext(context.Background())
	defer cancel()
	var addr string
	flag.StringVar(&addr, "l", ":8080", "Listen address:port, default :8080")
	flag.Parse()
	rand.Seed(time.Now().UnixNano())
	http.HandleFunc("/", pageServer)
	http.HandleFunc("/img/", imgServer)
	http.HandleFunc("/favicon.ico", http.NotFound)
	http.HandleFunc("/halt", haltServer)
	log.Printf("Starting WRP http server on %s\n", addr)
	http.ListenAndServe(addr, nil)
}
