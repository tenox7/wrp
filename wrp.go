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
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
	"github.com/ericpauley/go-quantize/quantize"
)

var (
	version = "4.2"
	srv     http.Server
	ctx     context.Context
	cancel  context.CancelFunc
	gifmap  = make(map[string]bytes.Buffer)
	ismap   = make(map[string]wrpReq)
)

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
}

func (w *wrpReq) parseForm(req *http.Request) {
	req.ParseForm()
	w.U = req.FormValue("url")
	if len(w.U) > 1 && !strings.HasPrefix(w.U, "http") {
		w.U = fmt.Sprintf("http://www.google.com/search?q=%s", url.QueryEscape(w.U))
	}
	w.W, _ = strconv.ParseInt(req.FormValue("w"), 10, 64)
	if w.W < 10 {
		w.W = 1152
	}
	w.H, _ = strconv.ParseInt(req.FormValue("h"), 10, 64)
	if w.H < 10 {
		w.H = 600
	}
	w.S, _ = strconv.ParseFloat(req.FormValue("s"), 64)
	if w.S < 0.1 {
		w.S = 1.0
	}
	w.C, _ = strconv.ParseInt(req.FormValue("c"), 10, 64)
	if w.C < 2 || w.C > 256 {
		w.C = 256
	}
	w.K = req.FormValue("k")
	w.F = req.FormValue("Fn")
	log.Printf("%s WrpReq from Form: %+v\n", req.RemoteAddr, w)
}

func (w wrpReq) printPage(out http.ResponseWriter) {
	out.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(out, "<!-- Web Rendering Proxy Version %s -->\n", version)
	fmt.Fprintf(out, "<HTML>\n<HEAD><TITLE>WRP %s</TITLE></HEAD>\n<BODY BGCOLOR=\"#F0F0F0\">\n", w.U)
	fmt.Fprintf(out, "<FORM ACTION=\"/\" METHOD=\"POST\">\n")
	fmt.Fprintf(out, "<INPUT TYPE=\"TEXT\" NAME=\"url\" VALUE=\"%s\" SIZE=\"10\">", w.U)
	fmt.Fprintf(out, "<INPUT TYPE=\"SUBMIT\" VALUE=\"Go\">\n")
	fmt.Fprintf(out, "<INPUT TYPE=\"SUBMIT\" NAME=\"Fn\" VALUE=\"Bk\">\n")
	fmt.Fprintf(out, "W <INPUT TYPE=\"TEXT\" NAME=\"w\" VALUE=\"%d\" SIZE=\"4\"> \n", w.W)
	fmt.Fprintf(out, "H <INPUT TYPE=\"TEXT\" NAME=\"h\" VALUE=\"%d\" SIZE=\"4\"> \n", w.H)
	fmt.Fprintf(out, "S <INPUT TYPE=\"TEXT\" NAME=\"s\" VALUE=\"%1.2f\" SIZE=\"3\"> \n", w.S)
	fmt.Fprintf(out, "C <INPUT TYPE=\"TEXT\" NAME=\"c\" VALUE=\"%d\" SIZE=\"3\">\n", w.C)
	fmt.Fprintf(out, "K <INPUT TYPE=\"TEXT\" NAME=\"k\" VALUE=\"\" SIZE=\"4\"> \n")
	fmt.Fprintf(out, "<INPUT TYPE=\"SUBMIT\" NAME=\"Fn\" VALUE=\"Bs\">\n")
	fmt.Fprintf(out, "<INPUT TYPE=\"SUBMIT\" NAME=\"Fn\" VALUE=\"Rt\">\n")
	fmt.Fprintf(out, "<INPUT TYPE=\"SUBMIT\" NAME=\"Fn\" VALUE=\"&lt;\">\n")
	fmt.Fprintf(out, "<INPUT TYPE=\"SUBMIT\" NAME=\"Fn\" VALUE=\"^\">\n")
	fmt.Fprintf(out, "<INPUT TYPE=\"SUBMIT\" NAME=\"Fn\" VALUE=\"v\">\n")
	fmt.Fprintf(out, "<INPUT TYPE=\"SUBMIT\" NAME=\"Fn\" VALUE=\"&gt;\" SIZE=\"1\">\n")
	fmt.Fprintf(out, "</FORM><BR>\n")
}

func (w wrpReq) printFooter(out http.ResponseWriter) {
	fmt.Fprintf(out, "\n<P><A HREF=\"/?url=https://github.com/tenox7/wrp/&w=%d&h=%d&s=%1.2f&c=%d\">"+
		"Web Rendering Proxy Version %s</A> | <A HREF=\"/shutdown/\">Shutdown WRP</A></BODY>\n</HTML>\n", w.W, w.H, w.S, w.C, version)
}

func pageServer(out http.ResponseWriter, req *http.Request) {
	log.Printf("%s Page Request for %s [%+v]\n", req.RemoteAddr, req.URL.Path, req.URL.RawQuery)
	var w wrpReq
	w.parseForm(req)

	if len(w.U) > 4 {
		w.capture(req.RemoteAddr, out)
	} else {
		w.printPage(out)
		w.printFooter(out)
	}
}

func mapServer(out http.ResponseWriter, req *http.Request) {
	log.Printf("%s ISMAP Request for %s [%+v]\n", req.RemoteAddr, req.URL.Path, req.URL.RawQuery)
	w, ok := ismap[req.URL.Path]
	if !ok {
		fmt.Fprintf(out, "Unable to find map %s\n", req.URL.Path)
		log.Printf("Unable to find map %s\n", req.URL.Path)
		return
	}
	defer delete(ismap, req.URL.Path)
	n, err := fmt.Sscanf(req.URL.RawQuery, "%d,%d", &w.X, &w.Y)
	if err != nil || n != 2 {
		fmt.Fprintf(out, "n=%d, err=%s\n", n, err)
		log.Printf("%s ISMAP n=%d, err=%s\n", req.RemoteAddr, n, err)
		return
	}
	log.Printf("%s WrpReq from ISMAP: %+v\n", req.RemoteAddr, w)
	if len(w.U) > 4 {
		w.capture(req.RemoteAddr, out)
	} else {
		w.printPage(out)
		w.printFooter(out)
	}
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

func (w wrpReq) capture(c string, out http.ResponseWriter) {
	var pngbuf []byte
	var gifbuf bytes.Buffer
	var err error

	if w.X > 0 && w.Y > 0 {
		log.Printf("%s Mouse Click %d,%d\n", c, w.X, w.Y)
		err = chromedp.Run(ctx, chromedp.MouseClickXY(int64(float64(w.X)/w.S), int64(float64(w.Y)/w.S)))
	} else if len(w.F) > 0 {
		log.Printf("%s Button %v\n", c, w.F)
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
	} else if len(w.K) > 0 {
		log.Printf("%s Sending Keys: %#v\n", c, w.K)
		err = chromedp.Run(ctx, chromedp.KeyEvent(w.K))
	} else {
		log.Printf("%s Processing Capture Request for %s\n", c, w.U)
		err = chromedp.Run(ctx,
			emulation.SetDeviceMetricsOverride(int64(float64(w.W)/w.S), int64(float64(w.H)/w.S), w.S, false),
			chromedp.Navigate(w.U),
		)
	}

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

	chromedp.Run(
		ctx, chromedp.Sleep(time.Second*2),
		chromedp.Location(&w.U),
	)
	log.Printf("%s Landed on: %s\n", c, w.U)
	w.printPage(out)

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
	err = gif.Encode(&gifbuf, img, &gif.Options{NumColors: int(w.C), Quantizer: quantize.MedianCutQuantizer{}})
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
	ismap[mappath] = w
	log.Printf("%s Encoded GIF image: %s, Size: %dKB, Colors: %d\n", c, imgpath, len(gifbuf.Bytes())/1024, w.C)
	fmt.Fprintf(out, "<A HREF=\"%s\"><IMG SRC=\"%s\" BORDER=\"0\" ISMAP></A>", mappath, imgpath)
	w.printFooter(out)
	log.Printf("%s Done with caputure for %s\n", c, w.U)
}

func haltServer(out http.ResponseWriter, req *http.Request) {
	log.Printf("%s Shutdown Request for %s\n", req.RemoteAddr, req.URL.Path)
	out.Header().Set("Content-Type", "text/text")
	fmt.Fprintf(out, "Shutting down WRP...\n")
	out.(http.Flusher).Flush()
	time.Sleep(time.Second * 2)
	cancel()
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
	err := srv.ListenAndServe()
	if err != nil {
		log.Fatal(err)
	}
}
