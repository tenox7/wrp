// wrp utility functions
package main

import (
	"encoding/json"
	"image"
	"image/color"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/MaxHalford/halfgone"
	"github.com/ericpauley/go-quantize/quantize"
)

func printMyIPs(b string) {
	ap := strings.Split(b, ":")
	if len(ap) < 2 {
		log.Fatal("Wrong format of ipaddress:port")
	}
	port := ap[len(ap)-1]
	if ap[0] != "" && ap[0] != "0.0.0.0" {
		log.Printf("Listen address: %v", b)
		return
	}
	a, err := net.InterfaceAddrs()
	if err != nil {
		log.Printf("Listen address: %v", b)
		return
	}
	var m string
	for _, i := range a {
		n, ok := i.(*net.IPNet)
		if !ok || n.IP.IsLoopback() || strings.Contains(n.IP.String(), ":") {
			continue
		}
		m += n.IP.String() + ":" + port + " "
	}
	log.Printf("Listen address: %v", m)
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

func findBrowser() string {
	var paths []string
	switch runtime.GOOS {
	case "darwin":
		paths = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
			"/Applications/Brave Browser Beta.app/Contents/MacOS/Brave Browser Beta",
			"/Applications/Brave Browser Nightly.app/Contents/MacOS/Brave Browser Nightly",
			"/Applications/Vivaldi.app/Contents/MacOS/Vivaldi",
			"/Applications/Arc.app/Contents/MacOS/Arc",
		}
	case "windows":
		pf := os.Getenv("ProgramFiles")
		pfx86 := os.Getenv("ProgramFiles(x86)")
		lad := os.Getenv("LOCALAPPDATA")
		paths = []string{
			filepath.Join(pf, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(pfx86, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(lad, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(pf, `BraveSoftware\Brave-Browser\Application\brave.exe`),
			filepath.Join(pfx86, `BraveSoftware\Brave-Browser\Application\brave.exe`),
			filepath.Join(lad, `BraveSoftware\Brave-Browser\Application\brave.exe`),
			filepath.Join(pf, `Chromium\Application\chrome.exe`),
			filepath.Join(lad, `Chromium\Application\chrome.exe`),
			filepath.Join(pf, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(pfx86, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(pf, `Vivaldi\Application\vivaldi.exe`),
		}
	default:
		paths = []string{
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
			"/usr/bin/brave-browser",
			"/usr/bin/brave-browser-stable",
			"/usr/bin/brave",
			"/usr/bin/microsoft-edge",
			"/usr/bin/vivaldi",
			"/snap/bin/chromium",
			"/snap/bin/brave",
			"/opt/brave.com/brave/brave",
			"/opt/google/chrome/chrome",
			"/opt/vivaldi/vivaldi",
			"/usr/local/bin/chrome",
			"/usr/local/bin/chromium",
			"/usr/local/bin/brave",
		}
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	for _, n := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "brave-browser", "brave", "microsoft-edge", "vivaldi", "chrome"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
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
