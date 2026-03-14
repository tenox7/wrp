// WRP Simple HTML Mode Routines
package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
	"github.com/lithammer/shortuuid/v4"
	"github.com/nfnt/resize"
	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
	"github.com/tenox7/gip"
	"golang.org/x/image/webp"
	"golang.org/x/net/html"
)

var imgStor imageStore

const imgZpfx = "/imgz/"

func init() {
	imgStor.img = make(map[string]imageContainer)
}

type imageContainer struct {
	data  []byte
	url   string
	added time.Time
}

type imageStore struct {
	img map[string]imageContainer
	sync.Mutex
}

func (i *imageStore) add(id, url string, img []byte) {
	i.Lock()
	defer i.Unlock()
	i.img[id] = imageContainer{data: img, url: url, added: time.Now()}
}

func (i *imageStore) get(id string) ([]byte, error) {
	i.Lock()
	defer i.Unlock()
	img, ok := i.img[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return img.data, nil
}

func (i *imageStore) del(id string) {
	i.Lock()
	defer i.Unlock()
	delete(i.img, id)
}

func fetchImage(id, imgURL, imgType string, maxSize, imgOpt int) (int, int, int, error) {
	log.Printf("Downloading IMGZ URL=%q for ID=%q", imgURL, id)
	var in []byte
	var err error
	if len(imgURL) < 4 {
		return 0, 0, 0, fmt.Errorf("image URL too short: %q", imgURL)
	}
	switch imgURL[:4] {
	case "http":
		req, err := http.NewRequest("GET", imgURL, nil)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("Error creating request for %q: %v", imgURL, err)
		}
		if *userAgent != "" {
			req.Header.Set("User-Agent", *userAgent)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("Error downloading %q: %v", imgURL, err)
		}
		if r.StatusCode != http.StatusOK {
			return 0, 0, 0, fmt.Errorf("Error %q HTTP Status Code: %v", imgURL, r.StatusCode)
		}
		defer r.Body.Close()
		in, err = io.ReadAll(r.Body)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("Error reading %q: %v", imgURL, err)
		}
	case "data":
		idx := strings.Index(imgURL, ",")
		if idx < 1 {
			return 0, 0, 0, fmt.Errorf("image is embeded but unable to find coma: %q", imgURL)
		}
		in, err = base64.StdEncoding.DecodeString(imgURL[idx+1:])
		if err != nil {
			return 0, 0, 0, fmt.Errorf("error decoding image from url embed: %q: %v", imgURL, err)
		}
	default:
		return 0, 0, 0, fmt.Errorf("unsupported image URL scheme: %q", imgURL)
	}
	out, w, h, err := smallImg(in, imgType, maxSize, imgOpt)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("Error scaling down %q: %v", imgURL, err)
	}
	imgStor.add(id, imgURL, out)
	return len(out), w, h, nil
}

func decodeSVG(src []byte, maxSize int) (image.Image, error) {
	icon, err := oksvg.ReadIconStream(bytes.NewReader(src))
	if err != nil {
		return nil, err
	}
	w, h := int(icon.ViewBox.W), int(icon.ViewBox.H)
	if w <= 0 || h <= 0 {
		w, h = maxSize, maxSize
	}
	if w > maxSize || h > maxSize {
		ratio := float64(maxSize) / float64(max(w, h))
		w = int(float64(w) * ratio)
		h = int(float64(h) * ratio)
	}
	if w <= 0 {
		w = 1
	}
	if h <= 0 {
		h = 1
	}
	icon.SetTarget(0, 0, float64(w), float64(h))
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	icon.Draw(rasterx.NewDasher(w, h, rasterx.NewScannerGV(w, h, rgba, rgba.Bounds())), 1)
	return rgba, nil
}

func isSVG(src []byte) bool {
	s := strings.TrimSpace(string(src[:min(len(src), 256)]))
	return strings.HasPrefix(s, "<svg") || strings.HasPrefix(s, "<?xml")
}

func smallImg(src []byte, imgType string, maxSize, imgOpt int) ([]byte, int, int, error) {
	t := http.DetectContentType(src)
	var err error
	var img image.Image
	switch {
	case t == "image/png":
		img, err = png.Decode(bytes.NewReader(src))
	case t == "image/gif":
		img, err = gif.Decode(bytes.NewReader(src))
	case t == "image/jpeg":
		img, err = jpeg.Decode(bytes.NewReader(src))
	case t == "image/webp":
		img, err = webp.Decode(bytes.NewReader(src))
	case t == "image/svg+xml", isSVG(src):
		img, err = decodeSVG(src, maxSize)
	default:
		err = errors.New("unknown content type: " + t)
	}
	if err != nil {
		return nil, 0, 0, fmt.Errorf("image decode problem: %v", err)
	}
	img = resize.Thumbnail(uint(maxSize), uint(maxSize), img, resize.NearestNeighbor)
	b := img.Bounds()
	var outBuf bytes.Buffer
	switch imgType {
	case "gip":
		err = gip.Encode(&outBuf, img, nil)
	case "png":
		err = png.Encode(&outBuf, img)
	case "gif":
		err = gif.Encode(&outBuf, gifPalette(img, int64(imgOpt)), &gif.Options{})
	case "jpg":
		err = jpeg.Encode(&outBuf, img, &jpeg.Options{Quality: imgOpt})
	}
	if err != nil {
		return nil, 0, 0, fmt.Errorf("image encode problem: %v", err)
	}
	return outBuf.Bytes(), b.Dx(), b.Dy(), nil
}

var removeElements = []string{
	"script", "style", "link", "meta", "noscript", "iframe",
	"svg", "canvas", "video", "audio", "source", "picture",
	"template", "slot", "dialog", "portal",
}

var renameToDiv = map[string]bool{
	"section": true, "article": true, "nav": true,
	"header": true, "footer": true, "aside": true,
	"main": true, "figure": true, "figcaption": true,
	"details": true, "summary": true, "hgroup": true,
	"mark": true, "time": true, "search": true,
}

var keepAttrs = map[string]bool{
	"href": true, "src": true, "alt": true, "title": true,
	"width": true, "height": true, "border": true,
	"cellpadding": true, "cellspacing": true,
	"bgcolor": true, "background": true,
	"align": true, "valign": true,
	"colspan": true, "rowspan": true, "nowrap": true,
	"name": true, "value": true, "type": true,
	"action": true, "method": true, "enctype": true,
	"size": true, "maxlength": true,
	"checked": true, "selected": true, "multiple": true,
	"disabled": true, "readonly": true,
	"placeholder": true, "for": true,
	"rows": true, "cols": true,
	"color": true, "face": true,
}

func resolveURL(raw string, base *url.URL) string {
	if raw == "" {
		return ""
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return base.ResolveReference(ref).String()
}

func simplifyDOM(doc *goquery.Document, rq *wrpReq) int {
	doc.Find(strings.Join(removeElements, ", ")).Remove()

	doc.Find("*").Each(func(i int, s *goquery.Selection) {
		for _, n := range s.Nodes {
			if n.Type != html.ElementNode {
				return
			}
			if renameToDiv[n.Data] {
				n.Data = "div"
			}
			var keep []html.Attribute
			for _, a := range n.Attr {
				if keepAttrs[a.Key] {
					keep = append(keep, a)
				}
			}
			n.Attr = keep
		}
	})

	imgExt := rq.imgType
	if imgExt == "gip" {
		imgExt = "gif"
	}
	var imgOpt int
	switch rq.imgType {
	case "jpg":
		imgOpt = int(rq.jQual)
	case "gif":
		imgOpt = int(rq.nColors)
	}
	baseURL, _ := url.Parse(rq.url)
	wrpParams := fmt.Sprintf("m=html&t=%s&s=%d", rq.imgType, rq.maxSize)

	doc.Find("a[href]").Each(func(i int, s *goquery.Selection) {
		href, exists := s.Attr("href")
		if !exists || href == "" {
			return
		}
		if strings.HasPrefix(href, "#") || strings.HasPrefix(href, "javascript:") {
			return
		}
		abs := resolveURL(href, baseURL)
		if rq.proxy {
			abs = strings.Replace(abs, "https://", "http://", 1)
			s.SetAttr("href", abs)
		} else {
			s.SetAttr("href", "/?"+wrpParams+"&url="+url.QueryEscape(abs))
		}
	})

	type imgJob struct {
		sel *goquery.Selection
		seq string
		abs string
	}
	var jobs []imgJob
	doc.Find("img[src]").Each(func(i int, s *goquery.Selection) {
		src, exists := s.Attr("src")
		if !exists || src == "" {
			s.Remove()
			return
		}
		abs := resolveURL(src, baseURL)
		seq := shortuuid.New() + "." + imgExt
		jobs = append(jobs, imgJob{sel: s, seq: seq, abs: abs})
	})

	var wg sync.WaitGroup
	var mu sync.Mutex
	var totSize int
	for i := range jobs {
		wg.Add(1)
		go func(j imgJob) {
			defer wg.Done()
			size, w, h, err := fetchImage(j.seq, j.abs, rq.imgType, int(rq.maxSize), imgOpt)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				log.Print(err)
				j.sel.Remove()
				return
			}
			j.sel.SetAttr("src", imgZpfx+j.seq)
			j.sel.SetAttr("width", strconv.Itoa(w))
			j.sel.SetAttr("height", strconv.Itoa(h))
			totSize += size
		}(jobs[i])
	}
	wg.Wait()

	return totSize
}

func (rq *wrpReq) captureMarkdown() {
	log.Printf("Processing simple HTML conversion for %v", rq.url)
	var outerHTML string
	err := chromedp.Run(ctx,
		waitForRender(),
		emulation.SetEmulatedMedia().WithMedia("print"),
		chromedp.Evaluate(`(function(){document.querySelectorAll('*').forEach(function(e){if(getComputedStyle(e).display==='none')e.remove()})})()`, nil),
		chromedp.OuterHTML("html", &outerHTML, chromedp.ByQuery),
		emulation.SetEmulatedMedia().WithMedia(""),
	)
	if err != nil {
		log.Printf("Failed to get OuterHTML via CDP: %v", err)
		http.Error(rq.w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("Got %v bytes HTML from CDP for %v", len(outerHTML), rq.url)

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(outerHTML))
	if err != nil {
		log.Printf("Failed to parse HTML: %v", err)
		http.Error(rq.w, err.Error(), http.StatusInternalServerError)
		return
	}

	totSize := simplifyDOM(doc, rq)

	body := doc.Find("body")
	simplified, err := body.Html()
	if err != nil {
		http.Error(rq.w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("Simplified to %v bytes html for %v", len(simplified), rq.url)

	if rq.proxy {
		rq.w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(rq.w, "<HTML><HEAD><TITLE>%s</TITLE></HEAD><BODY BGCOLOR=\"%s\">%s</BODY></HTML>",
			rq.url, *bgColor, string(asciify([]byte(simplified))))
		return
	}
	rq.printUI(uiParams{
		text:    string(asciify([]byte(simplified))),
		bgColor: *bgColor,
		imgSize: fmt.Sprintf("%.0f KB", float32(totSize)/1024.0),
	})
}

func imgServerTxt(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s IMGZ Request for %s", r.RemoteAddr, r.URL.Path)
	id := strings.Replace(r.URL.Path, imgZpfx, "", 1)
	img, err := imgStor.get(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Printf("%s IMGZ error for %s: %v", r.RemoteAddr, r.URL.Path, err)
		return
	}
	imgStor.del(id)
	w.Header().Set("Content-Type", http.DetectContentType(img))
	w.Header().Set("Content-Length", strconv.Itoa(len(img)))
	w.Write(img)
	w.(http.Flusher).Flush()
}
