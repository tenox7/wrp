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
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/chromedp/cdproto/emulation"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/chromedp"
)

var (
	ctx    context.Context
	cancel context.CancelFunc
	gifbuf bytes.Buffer
)

func pageServer(out http.ResponseWriter, req *http.Request) {
	req.ParseForm()
	furl := req.Form["url"]
	var gourl string
	if len(furl) >= 1 && len(furl[0]) > 4 {
		gourl = furl[0]
	} else {
		gourl = "https://en.wikipedia.org/wiki/"
	}
	log.Printf("%s Page Reqest for %s URL=%s\n", req.RemoteAddr, req.URL.Path, gourl)
	out.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(out, "<HTML>\n<HEAD><TITLE>WRP %s</TITLE>\n<BODY BGCOLOR=\"#F0F0F0\">", gourl)
	fmt.Fprintf(out, "<FORM ACTION=\"/\">URL: <INPUT TYPE=\"TEXT\" NAME=\"url\" VALUE=\"%s\">", gourl)
	fmt.Fprintf(out, "<INPUT TYPE=\"SUBMIT\" VALUE=\"Go\"></FORM><P>\n")
	if len(gourl) > 4 {
		capture(gourl, out)
	}
	fmt.Fprintf(out, "</BODY>\n</HTML>\n")
}

func imgServer(out http.ResponseWriter, req *http.Request) {
	log.Printf("%s Img Reqest for %s\n", req.RemoteAddr, req.URL.Path)
	out.Header().Set("Content-Type", "image/gif")
	out.Header().Set("Content-Length", strconv.Itoa(len(gifbuf.Bytes())))
	out.Write(gifbuf.Bytes())
}

func haltServer(out http.ResponseWriter, req *http.Request) {
	log.Printf("%s Shutdown request received [%s]\n", req.RemoteAddr, req.URL.Path)
	out.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(out, "WRP Shutdown")
	out.(http.Flusher).Flush()
	cancel()
	os.Exit(0)
}

func capture(gourl string, out http.ResponseWriter) {
	var nodes []*cdp.Node
	ctxx := chromedp.FromContext(ctx)
	var scrcap []byte
	var loc string

	log.Printf("Caputure Request for %s\n", gourl)

	chromedp.Run(ctx,
		emulation.SetDeviceMetricsOverride(1024, 768, 1.0, false),
		chromedp.Navigate(gourl),
		chromedp.Sleep(time.Second*2),
		chromedp.CaptureScreenshot(&scrcap),
		chromedp.Location(&loc),
		chromedp.Nodes("a", &nodes, chromedp.ByQueryAll))

	log.Printf("Landed on: %s, Got %d nodes\n", loc, len(nodes))

	img, err := png.Decode(bytes.NewReader(scrcap))
	if err != nil {
		log.Printf("Failed to decode screenshot: %s\n", err)
		fmt.Fprintf(out, "<BR>Unable to decode page screenshot:<BR>%s<BR>\n", err)
		return
	}
	gifbuf.Reset()
	gif.Encode(&gifbuf, img, nil)

	base, _ := url.Parse(loc)
	fmt.Fprintf(out, "<IMG SRC=\"/wrp.gif\" ALT=\"wrp\" USEMAP=\"#map\">\n<MAP NAME=\"map\">\n")

	for _, n := range nodes {
		b, err := dom.GetBoxModel().WithNodeID(n.NodeID).Do(cdp.WithExecutor(ctx, ctxx.Target))
		if err != nil {
			continue
		}
		tgt, err := base.Parse(n.AttributeValue("href"))
		if err != nil {
			continue
		}
		target := fmt.Sprintf("/?url=%s", tgt)

		if len(b.Content) > 6 && len(target) > 7 {
			fmt.Fprintf(out, "<AREA SHAPE=\"RECT\" COORDS=\"%.f,%.f,%.f,%.f\" ALT=\"%s\" TITLE=\"%s\" HREF=\"%s\">\n",
				b.Content[0], b.Content[1], b.Content[4], b.Content[5], n.AttributeValue("href"), n.AttributeValue("href"), target)
		}
	}

	fmt.Fprintf(out, "</MAP>\n")
	log.Printf("Done with caputure for %s\n", gourl)
}

func main() {
	ctx, cancel = chromedp.NewContext(context.Background())
	defer cancel()
	var addr string
	flag.StringVar(&addr, "l", ":8080", "Listen address:port, default :8080")
	flag.Parse()

	http.HandleFunc("/", pageServer)
	http.HandleFunc("/wrp.gif", imgServer)
	http.HandleFunc("/favicon.ico", http.NotFound)
	http.HandleFunc("/halt", haltServer)
	log.Printf("Starting http server on %s\n", addr)
	http.ListenAndServe(addr, nil)
}
