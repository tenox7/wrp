// WRP's version check: WRP asks GitHub for its newest release (-vercheck),
// at the start and then once a day. When that release is newer than this
// WRP, WRP says so in its log and on its page, next to its version.
// It does not update itself.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	verCheckEvery = 24 * time.Hour // between two checks
	verCheckRetry = time.Hour      // after an error
)

var verNew struct {
	sync.Mutex
	ver, page string // the newer WRP: its version and release page, or ""
}

var verClient = &http.Client{Timeout: 30 * time.Second}

// the newer WRP: its version and release page, or ""
func verNewer() (string, string) {
	verNew.Lock()
	defer verNew.Unlock()
	return verNew.ver, verNew.page
}

// versionNewer says if version a is newer than version b. Their numbers
// compare part by part, as numbers: 4.10.0 is newer than 4.9.2.
func versionNewer(a, b string) bool {
	x, y := versionParts(a), versionParts(b)
	for i := 0; i < len(x) || i < len(y); i++ {
		var p, q int
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		if p != q {
			return p > q
		}
	}
	return false
}

func versionParts(s string) []int {
	var n []int
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r < '0' || r > '9' }) {
		v, _ := strconv.Atoi(f)
		n = append(n, v)
	}
	return n
}

// the newest release of WRP: its version and its page
func verLatest() (string, string, error) {
	req, err := http.NewRequest("GET", *verCheck, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "WRP/"+version)
	resp, err := verClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("HTTP %d from %s", resp.StatusCode, req.URL.Host)
	}
	var rel struct {
		Tag  string `json:"tag_name"`
		Page string `json:"html_url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return "", "", err
	}
	// the version goes on WRP's page: only letters, digits, dots and dashes
	ver := strings.TrimPrefix(rel.Tag, "v")
	if ver == "" || len(ver) > 32 || strings.IndexFunc(ver, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '.' || r == '-')
	}) >= 0 {
		return "", "", fmt.Errorf("the newest release has an odd tag: %q", rel.Tag)
	}
	if !strings.HasPrefix(rel.Page, "https://") {
		rel.Page = "https://github.com/tenox7/wrp/releases/latest"
	}
	return ver, rel.Page, nil
}

// one check: it sets what WRP's page shows, and gives the log line and
// the time until the next check
func verCheckOnce() (string, time.Duration) {
	ver, page, err := verLatest()
	if err != nil {
		return fmt.Sprintf("Version check: %v", err), verCheckRetry
	}
	if !versionNewer(ver, version) {
		ver, page = "", ""
	}
	verNew.Lock()
	verNew.ver, verNew.page = ver, page
	verNew.Unlock()
	if ver == "" {
		return fmt.Sprintf("Version check: WRP %s is the newest", version), verCheckEvery
	}
	return fmt.Sprintf("Update available: WRP %s (this is %s): %s", ver, version, page), verCheckEvery
}

// the checks: the log gets the first answer, then only a new one
func verChecker() {
	last := ""
	for {
		msg, wait := verCheckOnce()
		if msg != last {
			log.Print(msg)
			last = msg
		}
		time.Sleep(wait)
	}
}
