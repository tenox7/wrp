// WRP TXT / Simple HTML Mode Routines
package main

// TODO:
// - add image processing times counter to the footer
// - img cache w/garbage collector / test back/button behavior in old browsers
// - add referer header
// - svg support
// - incorrect cert support in both markdown and image download
// - unify cdp and txt image handlers
// - use goroutiness to process images
// - get inner html from chromedp instead of html2markdown
//
// - BUG: DomainFromURL always prefixes with http instead of https
//   reproduces on vsi vms docs
// - BUG: markdown table errors
//   reproduces on hacker news
// - BUG: captcha errors using html to markdown, perhaps use cdp inner html + downloaded images
//   reproduces on https://www.cnn.com/cnn-underscored/electronics

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
	"strconv"
	"strings"
	"sync"
	"time"

	h2m "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/JohannesKaufmann/html-to-markdown/plugin"
	"github.com/lithammer/shortuuid/v4"
	"github.com/nfnt/resize"
	"github.com/tenox7/gip"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"golang.org/x/image/webp"
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

func fetchImage(id, url, imgType string, maxSize, imgOpt int) (int, error) {
	log.Printf("Downloading IMGZ URL=%q for ID=%q", url, id)
	var in []byte
	var err error
	switch url[:4] {
	case "http":
		r, err := http.Get(url) // TODO: possibly set a header "referer" here
		if err != nil {
			return 0, fmt.Errorf("Error downloading %q: %v", url, err)
		}
		if r.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("Error %q HTTP Status Code: %v", url, r.StatusCode)
		}
		defer r.Body.Close()
		in, err = io.ReadAll(r.Body)
		if err != nil {
			return 0, fmt.Errorf("Error reading %q: %v", url, err)
		}
	case "data":
		idx := strings.Index(url, ",")
		if idx < 1 {
			return 0, fmt.Errorf("image is embeded but unable to find coma: %q", url)
		}
		in, err = base64.StdEncoding.DecodeString(url[idx+1:])
		if err != nil {
			return 0, fmt.Errorf("error decoding image from url embed: %q: %v", url, err)
		}
	}
	out, err := smallImg(in, imgType, maxSize, imgOpt)
	if err != nil {
		return 0, fmt.Errorf("Error scaling down image: %v", err)
	}
	imgStor.add(id, url, out)
	return len(out), nil
}

func smallImg(src []byte, imgType string, maxSize, imgOpt int) ([]byte, error) {
	t := http.DetectContentType(src)
	var err error
	var img image.Image
	switch t {
	case "image/png":
		img, err = png.Decode(bytes.NewReader(src))
	case "image/gif":
		img, err = gif.Decode(bytes.NewReader(src))
	case "image/jpeg":
		img, err = jpeg.Decode(bytes.NewReader(src))
	case "image/webp":
		img, err = webp.Decode(bytes.NewReader(src))
	default: // TODO: also add svg
		err = errors.New("unknown content type: " + t)
	}
	if err != nil {
		return nil, fmt.Errorf("image decode problem: %v", err)
	}
	img = resize.Thumbnail(uint(maxSize), uint(maxSize), img, resize.NearestNeighbor)
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
		return nil, fmt.Errorf("gif encode problem: %v", err)
	}
	return outBuf.Bytes(), nil
}

type astTransformer struct {
	imgType string
	maxSize int
	imgOpt  int
	totSize int
}

func (t *astTransformer) Transform(node *ast.Document, reader text.Reader, pc parser.Context) {
	ast.Walk(node, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if link, ok := n.(*ast.Link); ok && entering {
			link.Destination = append([]byte("/?m=html&t="+t.imgType+"&s="+strconv.Itoa(t.maxSize)+"&url="), link.Destination...)
		}
		if img, ok := n.(*ast.Image); ok && entering {
			var imgExt string
			if t.imgType == "gip" {
				imgExt = "gif"
			} else {
				imgExt = t.imgType
			}
			seq := shortuuid.New() + "." + imgExt
			size, err := fetchImage(seq, string(img.Destination), t.imgType, t.maxSize, t.imgOpt) // TODO: use goroutines with waitgroup
			if err != nil {
				log.Print(err)
				n.Parent().RemoveChildren(n)
				return ast.WalkContinue, nil
			}
			img.Destination = []byte(imgZpfx + seq)
			t.totSize += size
		}
		return ast.WalkContinue, nil
	})
}

func (rq *wrpReq) captureMarkdown() {
	log.Printf("Processing Markdown conversion request for %v", rq.url)
	// TODO: bug - DomainFromURL always prefixes with http:// instead of https
	// this causes issues on some websites, fix or write a smarter DomainFromURL
	c := h2m.NewConverter(h2m.DomainFromURL(rq.url), true, nil)
	c.Use(plugin.GitHubFlavored())
	md, err := c.ConvertURL(rq.url) // We could also get inner html from chromedp
	if err != nil {
		http.Error(rq.w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("Got %v bytes md from %v", len(md), rq.url)
	var imgOpt int
	switch rq.imgType {
	case "jpg":
		imgOpt = int(rq.jQual)
	case "gif":
		imgOpt = int(rq.nColors)
	case "gip":
		imgOpt = 0
	}
	t := &astTransformer{imgType: rq.imgType, maxSize: int(rq.maxSize), imgOpt: imgOpt}
	gm := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithASTTransformers(util.Prioritized(t, 100))),
	)
	var ht bytes.Buffer
	err = gm.Convert([]byte(md), &ht)
	if err != nil {
		http.Error(rq.w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("Rendered %v bytes html for %v", len(ht.String()), rq.url)
	rq.printUI(uiParams{
		text:    string(asciify([]byte(ht.String()))),
		bgColor: "#FFFFFF",
		imgSize: fmt.Sprintf("%.0f KB", float32(t.totSize)/1024.0),
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
