package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
	"github.com/lithammer/shortuuid/v4"
)

var (
	dlDir    string
	dlNotify = make(chan struct{}, 1)
)

type dlEvent struct {
	guid     string
	filename string
	done     chan struct{}
}

var dlTrack struct {
	sync.Mutex
	ev *dlEvent
}

type dlFile struct {
	name string
	data []byte
}

var dlCache struct {
	sync.Mutex
	files map[string]dlFile
}

func setupDownloads() {
	if dlDir == "" {
		var err error
		dlDir, err = os.MkdirTemp("", "wrp-dl-")
		if err != nil {
			log.Fatalf("Failed to create download dir: %v", err)
		}
		dlCache.files = make(map[string]dlFile)
	}
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		switch e := ev.(type) {
		case *browser.EventDownloadWillBegin:
			log.Printf("Download started: %s (%s)", e.SuggestedFilename, e.GUID)
			dlTrack.Lock()
			dlTrack.ev = &dlEvent{
				guid:     e.GUID,
				filename: e.SuggestedFilename,
				done:     make(chan struct{}),
			}
			dlTrack.Unlock()
			select {
			case dlNotify <- struct{}{}:
			default:
			}
		case *browser.EventDownloadProgress:
			dlTrack.Lock()
			ev := dlTrack.ev
			dlTrack.Unlock()
			if ev == nil || ev.guid != e.GUID {
				return
			}
			if e.State == browser.DownloadProgressStateCompleted || e.State == browser.DownloadProgressStateCanceled {
				log.Printf("Download %s: %s", e.State, e.GUID)
				close(ev.done)
			}
		}
	})
	err := chromedp.Run(ctx,
		browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorAllowAndName).
			WithDownloadPath(dlDir).
			WithEventsEnabled(true),
	)
	if err != nil {
		log.Printf("Warning: failed to set download behavior: %v", err)
	}
}

func resetDownloadState() {
	dlTrack.Lock()
	dlTrack.ev = nil
	dlTrack.Unlock()
	select {
	case <-dlNotify:
	default:
	}
}

func waitForDownload() string {
	select {
	case <-dlNotify:
	case <-time.After(500 * time.Millisecond):
		return ""
	}
	dlTrack.Lock()
	ev := dlTrack.ev
	dlTrack.Unlock()
	if ev == nil {
		return ""
	}
	log.Printf("Waiting for download: %s", ev.filename)
	select {
	case <-ev.done:
	case <-time.After(60 * time.Second):
		log.Printf("Download timed out: %s", ev.guid)
		dlTrack.Lock()
		dlTrack.ev = nil
		dlTrack.Unlock()
		return ""
	}
	fpath := filepath.Join(dlDir, ev.guid)
	data, err := os.ReadFile(fpath)
	if err != nil {
		log.Printf("Failed to read download %s: %v", fpath, err)
		dlTrack.Lock()
		dlTrack.ev = nil
		dlTrack.Unlock()
		return ""
	}
	os.Remove(fpath)
	id := shortuuid.New()
	dlCache.Lock()
	dlCache.files[id] = dlFile{name: ev.filename, data: data}
	dlCache.Unlock()
	dlTrack.Lock()
	dlTrack.ev = nil
	dlTrack.Unlock()
	log.Printf("Download cached: /dl/%s (%s, %d bytes)", id, ev.filename, len(data))
	return "/dl/" + id
}

func dlServer(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/dl/")
	log.Printf("%s Download request for %s", r.RemoteAddr, id)
	dlCache.Lock()
	f, ok := dlCache.files[id]
	dlCache.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, f.name))
	w.Header().Set("Content-Type", http.DetectContentType(f.data))
	w.Header().Set("Content-Length", strconv.Itoa(len(f.data)))
	w.Write(f.data)
	w.(http.Flusher).Flush()
	dlCache.Lock()
	delete(dlCache.files, id)
	dlCache.Unlock()
}
