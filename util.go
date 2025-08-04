// wrp utility functions
package main

import (
	"encoding/json"
	"image"
	"image/color/palette"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/MaxHalford/halfgone"
	"github.com/soniakeys/quant/median"
)

func printMyIPs(b string) {
	ap := strings.Split(b, ":")
	if len(ap) < 1 {
		log.Fatal("Wrong format of ipaddress:port")
	}
	log.Printf("Listen address: %v", b)
	if ap[0] != "" && ap[0] != "0.0.0.0" {
		return
	}
	a, err := net.InterfaceAddrs()
	if err != nil {
		log.Print("Unable to get interfaces: ", err)
		return
	}
	var m string
	for _, i := range a {
		n, ok := i.(*net.IPNet)
		if !ok || n.IP.IsLoopback() || strings.Contains(n.IP.String(), ":") {
			continue
		}
		m = m + n.IP.String() + " "
	}
	log.Print("My IP addresses: ", m)
}

func gifPalette(i image.Image, n int64) image.Image {
	switch n {
	case 2:
		i = halfgone.FloydSteinbergDitherer{}.Apply(halfgone.ImageToGray(i))
	case 216:
		var FastGifLut = [256]int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 5}
		r := i.Bounds()
		// NOTE: the color index computation below works only for palette.WebSafe!
		p := image.NewPaletted(r, palette.WebSafe)
		if i64, ok := i.(image.RGBA64Image); ok {
			for y := r.Min.Y; y < r.Max.Y; y++ {
				for x := r.Min.X; x < r.Max.X; x++ {
					c := i64.RGBA64At(x, y)
					r6 := FastGifLut[c.R>>8]
					g6 := FastGifLut[c.G>>8]
					b6 := FastGifLut[c.B>>8]
					p.SetColorIndex(x, y, uint8(36*r6+6*g6+b6))
				}
			}
		} else {
			for y := r.Min.Y; y < r.Max.Y; y++ {
				for x := r.Min.X; x < r.Max.X; x++ {
					c := i.At(x, y)
					r, g, b, _ := c.RGBA()
					r6 := FastGifLut[r&0xff]
					g6 := FastGifLut[g&0xff]
					b6 := FastGifLut[b&0xff]
					p.SetColorIndex(x, y, uint8(36*r6+6*g6+b6))
				}
			}
		}
		i = p
	default:
		q := median.Quantizer(n)
		i = q.Paletted(i)
	}
	return i
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

func fetchJnrbsnUserAgent() string {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("https://jnrbsn.github.io/user-agents/user-agents.json")
	if err != nil {
		log.Printf("Failed to fetch user agents from jnrbsn: %v", err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("jnrbsn API returned status: %d", resp.StatusCode)
		return ""
	}

	var userAgents []string
	if err := json.NewDecoder(resp.Body).Decode(&userAgents); err != nil {
		log.Printf("Failed to decode jnrbsn user agents JSON: %v", err)
		return ""
	}

	if len(userAgents) == 0 {
		log.Printf("jnrbsn API returned no user agents")
		return ""
	}

	log.Printf("Fetched user agent from jnrbsn: %s", userAgents[0])
	return userAgents[0]
}
