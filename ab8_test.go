package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"math/rand"
	"testing"
)

// ab8Unpack undoes ab8Pack, as a client does: n bytes, in rows of w
func ab8Unpack(d []byte, w, n int) ([]byte, error) {
	o := make([]byte, 0, n)
	back := func(k, dist int) error {
		if dist > len(o) {
			return fmt.Errorf("%d bytes back, only %d there", dist, len(o))
		}
		for ; k > 0; k-- {
			o = append(o, o[len(o)-dist])
		}
		return nil
	}
	for i := 0; i < len(d); {
		c := int(d[i])
		i++
		k := c&63 + 1
		var err error
		switch {
		case c < 0x40:
			o = append(o, d[i:i+k]...)
			i += k
		case c < 0xc0:
			if k == 64 {
				k = 64 + int(d[i])<<8 + int(d[i+1])
				i += 2
			}
			if c < 0x80 {
				err = back(k, 1)
			} else {
				err = back(k, w)
			}
		case c < 0xe0:
			err = back(c&31+2, int(d[i])+1)
			i++
		default:
			k = c&31 + 3
			dist := int(d[i])<<8 + int(d[i+1]) + 1
			i += 2
			if k == 34 {
				k = 34 + int(d[i])
				i++
			}
			err = back(k, dist)
		}
		if err != nil {
			return nil, fmt.Errorf("token before %d: %v", i, err)
		}
	}
	if len(o) != n {
		return nil, fmt.Errorf("%d bytes, want %d", len(o), n)
	}
	return o, nil
}

// pictures for the pack tests: rows of w bytes
func ab8TestPix(w int) map[string][]byte {
	rnd := rand.New(rand.NewSource(1))
	pages := map[string][]byte{
		"empty": {},
		"one":   {7},
		"flat":  bytes.Repeat([]byte{3}, 300*w), // a run longer than a 2-byte count's 64
	}
	noise := make([]byte, 40*w)
	rnd.Read(noise)
	pages["noise"] = noise
	// a page: white, with words (short patterns that come again), a
	// photo (noise), and a block of rows that comes twice
	page := bytes.Repeat([]byte{0}, 200*w)
	words := make([][]byte, 20)
	for k := range words {
		words[k] = make([]byte, 3+rnd.Intn(40))
		for j := range words[k] {
			words[k][j] = byte(rnd.Intn(4))
		}
	}
	for k := 0; k < 400; k++ {
		wd := words[rnd.Intn(len(words))]
		copy(page[rnd.Intn(len(page)-len(wd)):], wd)
	}
	rnd.Read(page[150*w : 160*w])
	copy(page[100*w:], page[20*w:60*w])
	pages["page"] = page
	// a long match: 300 bytes that come again far away, in noise
	far := make([]byte, 2000)
	rnd.Read(far)
	copy(far[1500:], far[100:400])
	pages["far"] = far
	return pages
}

func TestAB8Pack(t *testing.T) {
	for _, w := range []int{1, 7, 64, 632} {
		for name, d := range ab8TestPix(w) {
			p := ab8Pack(d, w)
			got, err := ab8Unpack(p, w, len(d))
			if err != nil {
				t.Errorf("%s, rows of %d: %v", name, w, err)
			} else if !bytes.Equal(got, d) {
				t.Errorf("%s, rows of %d: %d bytes packed to %d do not come back the same", name, w, len(d), len(p))
			}
			if len(p) > len(d)+len(d)/64+1 {
				t.Errorf("%s, rows of %d: %d bytes packed to %d: more than as they are", name, w, len(d), len(p))
			}
		}
	}
	if n := len(ab8Pack(bytes.Repeat([]byte{3}, 351*632), 632)); n > 16 {
		t.Errorf("a flat slice: %d bytes, want 16 or fewer", n)
	}
	if d := ab8TestPix(64)["page"]; len(ab8Pack(d, 64)) > len(d)/3 {
		t.Errorf("a page of %d bytes packed to %d: want a third or less", len(d), len(ab8Pack(d, 64)))
	}
}

// ab8Pack is used by many requests at once
func TestAB8PackTogether(t *testing.T) {
	d := ab8TestPix(64)["page"]
	want := ab8Pack(d, 64)
	done := make(chan bool)
	for k := 0; k < 8; k++ {
		go func(k int) {
			ok := true
			for j := 0; j < 5; j++ {
				if k&1 == 0 {
					ok = ok && bytes.Equal(ab8Pack(d, 64), want)
				} else {
					ab8Pack(d[:len(d)/2+k], 7)
				}
			}
			done <- ok
		}(k)
	}
	for k := 0; k < 8; k++ {
		if !<-done {
			t.Error("a pack that ran with others is not the same")
		}
	}
}

func TestEncodeAB8(t *testing.T) {
	pal := color.Palette{color.White, color.Black, color.RGBA{255, 0, 0, 255}, color.RGBA{0x12, 0x34, 0x56, 255}}
	big := image.NewPaletted(image.Rect(0, 0, 5, 20), pal)
	for i := range big.Pix {
		big.Pix[i] = byte(i * 7 % 4)
	}
	// rows 6 to 9, as a slice has them: a part of a larger picture
	p := cut(big, 6, 4).(*image.Paletted)
	var b bytes.Buffer
	if err := encodeAB8(&b, p); err != nil {
		t.Fatal(err)
	}
	f := b.Bytes()
	want := []byte{'A', 'B', '8', '1', 0, 5, 0, 4, 3, 255, 255, 255, 0, 0, 0, 255, 0, 0, 0x12, 0x34, 0x56}
	if len(f) < len(want) || !bytes.Equal(f[:len(want)], want) {
		t.Fatalf("header % x, want % x", f, want)
	}
	got, err := ab8Unpack(f[len(want):], 5, 20)
	if err != nil || !bytes.Equal(got, big.Pix[30:50]) {
		t.Errorf("the rows % x (%v), want % x", got, err, big.Pix[30:50])
	}
	if encodeAB8(&b, image.NewPaletted(image.Rect(0, 0, 0, 3), pal)) == nil {
		t.Error("an empty picture: no error")
	}
}
