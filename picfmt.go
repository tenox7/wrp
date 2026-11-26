// Which picture a client gets for a slice of a GIF page.
//
// WRP has more than one way to send such a slice. Two of them are for a
// client with a slow CPU: its decoder does no work for each pixel.
//
//	black and white   ABM (abm.go), fast to decode, or ABM3 (abm3.go),
//	                  about 39% smaller and slow to decode; else the GIF
//	more colors       AB8 (ab8.go), fast to decode, or the GIF
//
// The client names the types it takes in its Accept header, each with a q
// if it likes it less (image/x-abm3;q=0.5). WRP makes the slice in each
// of these types that the client likes best, and sends the smallest one
// (of two that are as small, the one that is faster to decode). So a
// client whose CPU is the slow part gives the dense types a lower q, and
// a client whose line is the slow part gives all the same q.
//
// When WRP made more than one, X-Fast-Length is the slice's size in the
// type that decodes fastest. A client that times its pictures to choose
// can so compare pictures that came in different types.
//
// A client that names none of ABM, ABM3 and AB8 gets the GIF, as before.
package main

import (
	"bytes"
	"fmt"
	"image"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const (
	gifType       = "image/gif"
	fastLenHeader = "X-Fast-Length"
)

// acceptQ: the q of each type in the request's Accept header, as the
// client names it (a pattern like image/* is not a type)
func acceptQ(r *http.Request) map[string]float64 {
	m := map[string]float64{}
	for _, t := range strings.Split(r.Header.Get("Accept"), ",") {
		name, par, _ := strings.Cut(t, ";")
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		q := 1.0
		for _, p := range strings.Split(par, ";") {
			k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
			if strings.TrimSpace(k) != "q" {
				continue
			}
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f >= 0 && f <= 1 {
				q = f
			}
		}
		m[name] = q
	}
	return m
}

// a way to send a slice
type picFmt struct {
	name string // for the log
	mime string
	enc  func(io.Writer) error
}

// sliceFmts: the types the client takes for the slice i of a page of type
// typ, the fastest to decode first, and only those it likes best
func sliceFmts(r *http.Request, i image.Image, typ string, jq int) []picFmt {
	def := picFmt{typ, gifType, func(w io.Writer) error { return encodeImg(w, i, typ, jq) }}
	switch typ {
	case "png":
		def.mime = "image/png"
	case "jpg":
		def.mime = "image/jpeg"
	}
	p, ok := i.(*image.Paletted)
	if !ok || typ != "gif" {
		return []picFmt{def}
	}
	acc := acceptQ(r)
	var all []picFmt
	if bits, bpl := abmBits(p); bits != nil {
		wd, ht := p.Rect.Dx(), p.Rect.Dy()
		all = []picFmt{
			{"abm", abmType, func(w io.Writer) error { return writeABM(w, '1', wd, ht, abmPack(bits, bpl)) }},
			{"abm3", abm3Type, func(w io.Writer) error { return writeABM(w, '3', wd, ht, abm3Pack(bits, bpl, wd)) }},
		}
	} else if len(p.Palette) >= 1 && len(p.Palette) <= 256 {
		all = []picFmt{{"ab8", ab8Type, func(w io.Writer) error { return encodeAB8(w, p) }}}
		if _, named := acc[gifType]; named {
			all = append(all, def)
		}
	}
	best := 0.0
	for _, f := range all {
		best = max(best, acc[f.mime])
	}
	var fs []picFmt
	for _, f := range all {
		if q := acc[f.mime]; q > 0 && q == best {
			fs = append(fs, f)
		}
	}
	if len(fs) == 0 {
		return []picFmt{def}
	}
	return fs
}

// encodeSlice makes the slice i for the client: its bytes, how it was
// made, and, if WRP chose among more than one type, its size in the one
// that decodes fastest (else 0)
func encodeSlice(r *http.Request, i image.Image, typ string, jq int) (data []byte, f picFmt, fastLen int, err error) {
	fs := sliceFmts(r, i, typ, jq)
	for k, c := range fs {
		var b bytes.Buffer
		if err = c.enc(&b); err != nil {
			return nil, c, 0, err
		}
		if k == 0 {
			fastLen = b.Len()
		}
		if k == 0 || b.Len() < len(data) {
			data, f = b.Bytes(), c
		}
	}
	if len(fs) < 2 {
		fastLen = 0
	}
	return data, f, fastLen, nil
}

// sendSlice answers with the slice i as the client takes it. It returns
// what it sent, for the log: the type, the size, and what the client's
// Accept names of WRP's own types.
func sendSlice(w http.ResponseWriter, r *http.Request, i image.Image, typ string, jq int) (string, error) {
	data, pf, fast, err := encodeSlice(r, i, typ, jq)
	if err != nil {
		return "", err
	}
	w.Header().Set("Content-Type", pf.mime)
	if typ == "gif" {
		w.Header().Set("Vary", "Accept")
	}
	what := fmt.Sprintf("%s, %d bytes", pf.name, len(data))
	if fast > 0 {
		w.Header().Set(fastLenHeader, strconv.Itoa(fast))
		what += fmt.Sprintf(" (fastest to decode: %d)", fast)
	}
	if a := acceptOwn(r); a != "" {
		what += ", client takes " + a
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "max-age=0")
	w.Header().Set("Expires", "-1")
	w.Header().Set("Pragma", "no-cache")
	w.Write(data)
	return what, nil
}

// acceptOwn: WRP's own picture types that the client's Accept names, with
// their q if it is not 1, and the GIF's if it has one: "abm abm3;q=0.5".
// It shows in the log which client asks: an older Archaic names only abm.
func acceptOwn(r *http.Request) string {
	acc := acceptQ(r)
	var o []string
	for _, t := range []struct{ name, mime string }{{"abm", abmType}, {"abm3", abm3Type}, {"ab8", ab8Type}, {"gif", gifType}} {
		q, ok := acc[t.mime]
		if !ok || (t.name == "gif" && len(o) == 0) {
			continue
		}
		if q != 1 {
			o = append(o, t.name+";q="+strconv.FormatFloat(q, 'g', 3, 64))
		} else {
			o = append(o, t.name)
		}
	}
	return strings.Join(o, " ")
}
