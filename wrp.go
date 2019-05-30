package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"bytes"
	_ "image"
	"image/png"
	"image/gif"

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
	var url string
	if len(furl) >= 1 && len(furl[0]) > 4 {
		url = furl[0]
	} else {
		url = "https://en.wikipedia.org/wiki/"
	}
	log.Printf("%s Page Reqest for %s URL=%s\n", req.RemoteAddr, req.URL.Path, url)
	out.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(out, "<HTML>\n<HEAD><TITLE>WRP %s</TITLE>\n<BODY BGCOLOR=\"#F0F0F0\">", url)
	fmt.Fprintf(out, "<FORM ACTION=\"/\">URL: <INPUT TYPE=\"TEXT\" NAME=\"url\" VALUE=\"%s\">", url)
	fmt.Fprintf(out, "<INPUT TYPE=\"SUBMIT\" VALUE=\"Go\"></FORM><P>\n")
	if len(url) > 4 {
		capture(url, out)
	}
	fmt.Fprintf(out, "</BODY>\n</HTML>\n")
}

func imgServer(out http.ResponseWriter, req *http.Request) {
	log.Printf("%s Img Reqest for %s\n", req.RemoteAddr, req.URL.Path)
	out.Header().Set("Content-Type", "image/gif")
	out.Header().Set("Content-Length", strconv.Itoa(len(gifbuf.Bytes())))
	out.Write(gifbuf.Bytes())
}

func capture(url string, out http.ResponseWriter) {
	var nodes []*cdp.Node
	ctxx := chromedp.FromContext(ctx)
	var target string
	var scrcap []byte

	log.Printf("Caputure Request for %s\n", url)

	chromedp.Run(ctx,
		emulation.SetDeviceMetricsOverride(1024, 768, 1.0, false),
		chromedp.Navigate(url),
		chromedp.Sleep(time.Second*2),
		chromedp.CaptureScreenshot(&scrcap),
		chromedp.Nodes("a", &nodes, chromedp.ByQueryAll))

		img, err:= png.Decode(bytes.NewReader(scrcap) )
		if err != nil {
			log.Fatal(err)
		}
		gifbuf.Reset()
		gif.Encode(&gifbuf, img, nil)

	fmt.Fprintf(out, "<IMG SRC=\"/wrp.gif\" ALT=\"wrp\" USEMAP=\"#map\">\n<MAP NAME=\"map\">\n")

	for _, n := range nodes {
		b, err := dom.GetBoxModel().WithNodeID(n.NodeID).Do(cdp.WithExecutor(ctx, ctxx.Target))
		if strings.HasPrefix(n.AttributeValue("href"), "/") {
			target = fmt.Sprintf("/?url=%s%s", url, n.AttributeValue("href"))
		} else {
			target = fmt.Sprintf("/?url=%s", n.AttributeValue("href"))
		}

		if err == nil && len(b.Content) > 6 {
			fmt.Fprintf(out, "<AREA SHAPE=\"RECT\" COORDS=\"%.f,%.f,%.f,%.f\" ALT=\"%s\" TITLE=\"%s\" HREF=\"%s\">\n",
				b.Content[0], b.Content[1], b.Content[4], b.Content[5], n.AttributeValue("href"), n.AttributeValue("href"), target)
		}
	}

	fmt.Fprintf(out, "</MAP>\n")
	log.Printf("Done with caputure for %s\n", url)
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
	log.Printf("Starting http server on %s\n", addr)
	http.ListenAndServe(addr, nil)
}
