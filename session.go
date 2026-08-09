// WRP session persistence: window geometry, rendering parameters and open tabs
// are kept in the -profile directory and restored on the next run
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	sessVer     = 1
	sessFile    = "wrp-session.json"
	sessIntv    = 5 * time.Second  // between periodic saves, when anything changed
	sessNavTO   = 30 * time.Second // per restored page, so a stuck load cannot hang startup
	sessMaxTabs = 32
)

// Http ui parameters, restored as the defaults a fresh client gets
type sessUI struct {
	Width   int64   `json:"width"`
	Height  int64   `json:"height"`
	Colors  int64   `json:"colors"`
	JpgQual int64   `json:"jpgqual"`
	MaxSize int64   `json:"maxsize"`
	Zoom    float64 `json:"zoom"`
	ImgType string  `json:"imgtype"`
	Mode    string  `json:"mode"`
}

// Vnc framebuffer, height includes the tab and nav bars
type sessFB struct {
	Width  int     `json:"width"`
	Height int     `json:"height"`
	Zoom   float64 `json:"zoom"`
}

type sessData struct {
	Ver    int      `json:"ver"`
	UI     sessUI   `json:"ui"`
	FB     sessFB   `json:"framebuffer"`
	Tabs   []string `json:"tabs"` // in tab bar order, blanks kept so Active lines up
	Active int      `json:"active"`
}

var (
	sessMu    sync.Mutex
	sess      sessData
	sessDirty atomic.Bool
	sessSet   map[string]bool // flags given on the command line, they win over the file
	defZoom   = 1.0           // http ui zoom default, has no flag, comes from the session
)

func sessPath() string {
	if *userDataDir == "" {
		return ""
	}
	return filepath.Join(*userDataDir, sessFile)
}

// Only real pages are worth reopening
func sessURL(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// Note which flags were given explicitly, they override the saved session
func sessCmdLine() {
	sessSet = make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { sessSet[f.Name] = true })
}

// Restore the saved defaults, called after the -g flag is parsed
func sessLoad() {
	p := sessPath()
	if p == "" {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Session: %v", err)
		}
		return
	}
	var d sessData
	if err := json.Unmarshal(b, &d); err != nil {
		log.Printf("Session: %v is unreadable: %v", p, err)
		return
	}
	if d.Ver != sessVer {
		log.Printf("Session: %v is version %v, want %v, ignoring", p, d.Ver, sessVer)
		return
	}
	sessMu.Lock()
	sess = d
	sessMu.Unlock()
	u := d.UI
	if !sessSet["g"] {
		if u.Width >= vncMinDim && u.Width <= vncMaxDim {
			defGeom.w = u.Width
		}
		if u.Height >= 0 && u.Height <= vncMaxDim { // 0 is a full page capture
			defGeom.h = u.Height
		}
		if u.Colors >= 2 && u.Colors <= 256 {
			defGeom.c = u.Colors
		}
	}
	if !sessSet["t"] && slices.Contains([]string{"gip", "png", "gif", "jpg"}, u.ImgType) {
		*defType = u.ImgType
	}
	if !sessSet["m"] && (u.Mode == "ismap" || u.Mode == "html") {
		*wrpMode = u.Mode
	}
	if !sessSet["q"] && u.JpgQual >= 1 && u.JpgQual <= 100 {
		*defJpgQual = u.JpgQual
	}
	if !sessSet["is"] && u.MaxSize > 0 {
		*defImgSize = u.MaxSize
	}
	if u.Zoom >= 0.1 && u.Zoom <= 10 {
		defZoom = u.Zoom
	}
	log.Printf("Session: restored from %v: %+v", p, d)
}

// Saved framebuffer size and zoom for vncInit, zero when there is nothing to
// restore. Explicit -g still sets the initial size.
func sessVncGeom() (w, h int, z float64) {
	if sessPath() == "" {
		return 0, 0, 0
	}
	sessMu.Lock()
	defer sessMu.Unlock()
	if sess.FB.Zoom >= 0.25 && sess.FB.Zoom <= 4 {
		z = sess.FB.Zoom
	}
	if sessSet["g"] || sess.FB.Width < vncMinDim || sess.FB.Height < vncMinDim+vncChromeH {
		return 0, 0, z
	}
	return vncClamp(sess.FB.Width), vncClamp(sess.FB.Height), z
}

// Remember the parameters of the last http ui request
func sessSaveUI(rq *wrpReq) {
	if sessPath() == "" {
		return
	}
	sessMu.Lock()
	sess.UI = sessUI{rq.width, rq.height, rq.nColors, rq.jQual, rq.maxSize, rq.zoom, rq.imgType, rq.wrpMode}
	sessMu.Unlock()
	sessDirty.Store(true)
}

// Remember the page the http ui landed on
func sessSaveURL(u string) {
	if sessPath() == "" || !sessURL(u) {
		return
	}
	// in vnc mode the ui drives the active tab, keep that tab current instead:
	// the tab list is only reconciled with the browser while a client is casting
	if *vncAddr != "" {
		vncSrv.Lock()
		if vncSrv.active < len(vncSrv.tabs) {
			vncSrv.tabs[vncSrv.active].url = u
		}
		vncSrv.Unlock()
		sessDirty.Store(true)
		return
	}
	sessMu.Lock()
	sess.Tabs = []string{u}
	sess.Active = 0
	sessMu.Unlock()
	sessDirty.Store(true)
}

// Something the snapshot picks up from the vnc state has changed
func sessMark() {
	if sessPath() == "" {
		return
	}
	sessDirty.Store(true)
}

// Current state to write out. Framebuffer and tabs are read live from the vnc
// server, the rest is whatever the http ui last used.
func sessSnap() sessData {
	sessMu.Lock()
	d := sess
	sessMu.Unlock()
	d.Ver = sessVer
	if *vncAddr == "" {
		return d
	}
	vncSrv.Lock()
	d.FB = sessFB{vncSrv.vw, vncSrv.vh, vncSrv.zoom}
	d.Tabs = make([]string, 0, len(vncSrv.tabs))
	for _, t := range vncSrv.tabs {
		d.Tabs = append(d.Tabs, t.url)
	}
	d.Active = vncSrv.active
	vncSrv.Unlock()
	return d
}

func sessWrite() {
	p := sessPath()
	if p == "" {
		return
	}
	b, err := json.MarshalIndent(sessSnap(), "", "  ")
	if err != nil {
		log.Printf("Session: %v", err)
		return
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("Session: %v", err)
		return
	}
	f, err := os.CreateTemp(dir, sessFile+".*")
	if err != nil {
		log.Printf("Session: %v", err)
		return
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), p)
	}
	if err != nil {
		os.Remove(f.Name())
		log.Printf("Session: %v", err)
	}
}

// Periodic writer, only runs when something actually changed
func sessSaver() {
	if sessPath() == "" {
		return
	}
	for range time.Tick(sessIntv) {
		if sessDirty.Swap(false) {
			sessWrite()
		}
	}
}

// Final write on the way out, before the browser goes away
func sessFlush() {
	if sessPath() == "" {
		return
	}
	if *vncAddr != "" && brCtx() != nil {
		vncTabSync() // the poller only reconciles while a client is casting
	}
	sessDirty.Store(false)
	sessWrite()
}

// Page to prefill the http ui url box with, empty if there is none
func sessLastURL() string {
	if sessPath() == "" {
		return ""
	}
	d := sessSnap()
	if d.Active < 0 || d.Active >= len(d.Tabs) || !sessURL(d.Tabs[d.Active]) {
		return ""
	}
	return d.Tabs[d.Active]
}

func sessNavigate(ctx context.Context, u string) {
	if err := vncRun(ctx, sessNavTO, vncNav(u)); err != nil {
		log.Printf("Session: navigate %v: %v", u, err)
	}
}

// Reopen the last page in http only mode, in the background so a slow site
// cannot hold up the listener. browserMu keeps it out of the way of requests.
func sessRestoreURL() {
	u := sessLastURL()
	if u == "" {
		return
	}
	log.Printf("Session: reopening %v", u)
	go func() {
		browserMu.Lock()
		defer browserMu.Unlock()
		sessNavigate(brCtx(), u)
	}()
}

// Reopen last session's tabs, false if there is nothing to restore and the
// caller should fall back to the start page. Explicit -vncpage wins.
func sessRestoreTabs() bool {
	if sessPath() == "" || sessSet["vncpage"] {
		return false
	}
	sessMu.Lock()
	urls, act := sess.Tabs, sess.Active
	sessMu.Unlock()
	if len(urls) > sessMaxTabs {
		log.Printf("Session: %v saved tabs, restoring the first %v", len(urls), sessMaxTabs)
		urls = urls[:sessMaxTabs]
	}
	if !slices.ContainsFunc(urls, sessURL) {
		return false
	}
	vncTabMu.Lock()
	defer vncTabMu.Unlock()
	for i, u := range urls {
		if i > 0 {
			if _, err := vncAddTab(); err != nil {
				log.Printf("Session: restore tab: %v", err)
				break
			}
		}
		vncSrv.Lock()
		var t *vncTab
		if i < len(vncSrv.tabs) {
			t = vncSrv.tabs[i]
			if sessURL(u) {
				t.url = u // show it in the tab bar before the first sync
			}
		}
		vncSrv.Unlock()
		if t == nil {
			break
		}
		if sessURL(u) {
			go sessNavigate(t.ctx, u) // tabs load in parallel, none of them blocks startup
		}
	}
	vncSrv.Lock()
	n := len(vncSrv.tabs)
	vncSrv.Unlock()
	if act < 0 || act >= n {
		act = 0
	}
	log.Printf("Session: reopened %v tab(s), active %v", n, act)
	vncActivate(act)
	if act < len(urls) && sessURL(urls[act]) {
		vncSetURL(urls[act])
	}
	return true
}
