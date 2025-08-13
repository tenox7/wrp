// wrp utility functions
package main

import (
	"encoding/json"
	"image"
	"image/color"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/MaxHalford/halfgone"
	"github.com/ericpauley/go-quantize/quantize"
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
	default:
		q := quantize.MedianCutQuantizer{}
		p := q.Quantize(make([]color.Color, 0, int(n)), i)
		bounds := i.Bounds()
		quantized := image.NewPaletted(bounds, p)
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				quantized.Set(x, y, i.At(x, y))
			}
		}
		i = quantized
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
