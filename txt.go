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

func grabImage(id, url string) {
	log.Printf("Downloading IMGZ URL=%q for ID=%q", url, id)
	var img []byte
	var err error
	switch url[:4] {
	case "http":
		r, err := http.Get(url) // TODO: possibly set a header "referer" here
		if err != nil {
			log.Printf("Error downloading %q: %v", url, err)
			return
		}
		defer r.Body.Close()
		img, err = io.ReadAll(r.Body)
		if err != nil {
			log.Printf("Error reading %q: %v", url, err)
			return
		}
	case "data":
		idx := strings.Index(url, ",")
		if idx < 1 {
			log.Printf("image is embeded but unable to find coma: %q", url)
			return
		}
		img, err = base64.StdEncoding.DecodeString(url[idx+1:])
		if err != nil {
			log.Printf("error decoding image from url embed: %q: %v", err)
			return
		}
	}
	gif, err := smallGif(img)
	if err != nil {
		log.Printf("Error scaling down image: %v", err)
		return
	}
	imgStor.add(id, url, gif)
}

type astTransformer struct{}

func (t *astTransformer) Transform(node *ast.Document, reader text.Reader, pc parser.Context) {
	ast.Walk(node, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if link, ok := n.(*ast.Link); ok && entering {
			link.Destination = append([]byte("?t=txt&url="), link.Destination...)
		}
		if img, ok := n.(*ast.Image); ok && entering {
			id := fmt.Sprintf("txt%05d.gif", rand.Intn(99999)) // atomic.AddInt64 could be better here
			grabImage(id, string(img.Destination))             // TODO: use goroutines with waitgroup
			img.Destination = []byte(imgZpfx + id)             // get error from grab image and blank out destination on error
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
	t := &astTransformer{}
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
		text: string(asciify([]byte(ht.String()))),
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
	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Content-Length", strconv.Itoa(len(img)))
	// TODO: we may want to let the client browser cache images
	w.Header().Set("Cache-Control", "max-age=0")
	w.Header().Set("Expires", "-1")
	w.Header().Set("Pragma", "no-cache")
	w.Write(img)
	w.(http.Flusher).Flush()
}

func smallGif(src []byte) ([]byte, error) {
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
	default:
		err = errors.New("unknown content type: " + t)
	}
	if err != nil {
		return nil, fmt.Errorf("image decode problem: %v", err)
	}
	if img.Bounds().Max.X-img.Bounds().Min.X > 200 {
		img = resize.Resize(200, 0, img, resize.NearestNeighbor)
	}
	var gifBuf bytes.Buffer
	err = gif.Encode(&gifBuf, gifPalette(img, 216), &gif.Options{})
	if err != nil {
		return nil, fmt.Errorf("gif encode problem: %v", err)
	}
	return gifBuf.Bytes(), nil
}
