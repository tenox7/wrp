package main

// TODO:
// - image type based on form value
// - also size and quality
// - non overlaping image names atomic.int etc
// - garbage collector / delete old images from map
// - add referer header
// - svg support
// - BOG: DomainFromURL always prefixes with http instead of https
//   reproduces on vsi vms docs
// - BUG: markdown table errors
//   reproduces on hacker news

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
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	h2m "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/JohannesKaufmann/html-to-markdown/plugin"
	"github.com/nfnt/resize"
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
	// TODO: add garbage collector
	// think about how to remove old images
	// if removed from cache how to download them later if a browser goes back?
	// browser should cache on it's own... but it may request it, what then?
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

func fetchImage(id, url, imgType string, maxSize, imgOpt int) error {
	log.Printf("Downloading IMGZ URL=%q for ID=%q", url, id)
	var img []byte
	var err error
	switch url[:4] {
	case "http":
		r, err := http.Get(url) // TODO: possibly set a header "referer" here
		if err != nil {
			return fmt.Errorf("Error downloading %q: %v", url, err)
		}
		if r.StatusCode != http.StatusOK {
			return fmt.Errorf("Error %q HTTP Status Code: %v", url, r.StatusCode)
		}
		defer r.Body.Close()
		img, err = io.ReadAll(r.Body)
		if err != nil {
			return fmt.Errorf("Error reading %q: %v", url, err)
		}
	case "data":
		idx := strings.Index(url, ",")
		if idx < 1 {
			return fmt.Errorf("image is embeded but unable to find coma: %q", url)
		}
		img, err = base64.StdEncoding.DecodeString(url[idx+1:])
		if err != nil {
			return fmt.Errorf("error decoding image from url embed: %q: %v", url, err)
		}
	}
	gif, err := smallImg(img, imgType, maxSize, imgOpt)
	if err != nil {
		return fmt.Errorf("Error scaling down image: %v", err)
	}
	imgStor.add(id, url, gif)
	return nil
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
	img = resize.Thumbnail(uint(*defImgSize), uint(*defImgSize), img, resize.NearestNeighbor)
	var outBuf bytes.Buffer
	switch imgType {
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
}

func (t *astTransformer) Transform(node *ast.Document, reader text.Reader, pc parser.Context) {
	ast.Walk(node, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if link, ok := n.(*ast.Link); ok && entering {
			link.Destination = append([]byte("/?t=txt&url="), link.Destination...)
		}
		if img, ok := n.(*ast.Image); ok && entering {
			// TODO: dynamic extension based on form value
			id := fmt.Sprintf("txt%05d.gif", rand.Intn(99999))                             // BUG: atomic.AddInt64 or something that ever increases - time based?
			err := fetchImage(id, string(img.Destination), t.imgType, t.maxSize, t.imgOpt) // TODO: use goroutines with waitgroup
			if err != nil {
				log.Print(err)
				n.Parent().RemoveChildren(n)
				return ast.WalkContinue, nil
			}
			img.Destination = []byte(imgZpfx + id)
		}
		return ast.WalkContinue, nil
	})
}

func asciify(s []byte) []byte {
	a := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			a[i] = '.'
			continue
		}
		a[i] = s[i]
	}
	return a
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
	t := &astTransformer{imgType: rq.imgType, maxSize: int(rq.maxSize), imgOpt: int(rq.imgOpt)} // TODO: maxSize still doesn't work
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
	rq.printHTML(printParams{
		text:    string(asciify([]byte(ht.String()))),
		bgColor: "#FFFFFF",
	})
}

func imgServerZ(w http.ResponseWriter, r *http.Request) {
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
