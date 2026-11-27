// Updates for Archaic, the WRP client for old systems
// (github.com/tenox7/archaic). Old systems have no HTTPS, so WRP gets
// Archaic's files from the GitHub release for them.
//
// Archaic asks for the newest Archaic for its system:
//
//	GET /archaic-update?p=3b1&v=0.8
//
// and WRP answers in plain text, a line each:
//
//	archaic-update 1
//	version 0.9
//	file archaic-3b1
//	size 67027
//	crc 1f2e3d4c
//	get archaic-update?p=3b1&f=0.9
//
// With no file to offer, a "note" line says why, and there is no version.
// Archaic compares the version with its own, and gets the file from the
// "get" address (relative to the question): the bytes of the release's
// file archaic-SYSTEM (or archaic-SYSTEM.prg, a program for the Atari),
// with their size and CRC-32 (IEEE) as above. A system's old name (in
// archaicOldSys) is the same system: Archaic 0.14 and older on the 3B1
// ask for unixpc, and get archaic-3b1.
//
// Archaic's programs carry "@(#)Archaic VERSION" and "@(#)Archaic for
// SYSTEM". The version is read from the file itself, not from the
// release's tag: a release also carries older builds for the systems
// that were not built again. A file for another system, or one without
// these marks (Archaic 0.8 and older), is not offered.
//
// For a first install, with curl or wget, WRP also gives the release's
// files as they are (older builds too):
//
//	curl -O http://wrp:8080/archaic-download/archaic-hpux9
//
// and /archaic-download alone lists them.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	archaicTTL    = 15 * time.Minute // how long the release info is kept: GitHub allows 60 asks an hour
	archaicErrTTL = 2 * time.Minute  // after an error
	archaicMax    = 8 << 20          // a larger file is not Archaic
	archaicIdent  = "@(#)Archaic "
	archaicSysID  = "@(#)Archaic for "
)

type archaicAsset struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	URL     string `json:"browser_download_url"`
	Digest  string `json:"digest"` // "sha256:...", in newer GitHub answers
	Updated string `json:"updated_at"`
}

type archaicRelease struct {
	Tag    string         `json:"tag_name"`
	Assets []archaicAsset `json:"assets"`
}

// a release file, checked, as it is served to Archaic
type archaicFile struct {
	key     string // the asset's address and time: a new upload is a new file
	name    string
	version string // its marks: "" if it has none
	sys     string
	data    []byte
	crc     uint32
	err     error     // why it is not served: kept for archaicErrTTL
	at      time.Time // when err came
}

var archaicUpd struct {
	sync.Mutex
	rel    *archaicRelease
	relErr error
	relAt  time.Time
	files  map[string]*archaicFile
	client *http.Client
}

// the newest release, from GitHub or from the cache
func archaicLatest() (*archaicRelease, error) {
	u := &archaicUpd
	ttl := archaicTTL
	if u.relErr != nil {
		ttl = archaicErrTTL
	}
	if !u.relAt.IsZero() && time.Since(u.relAt) < ttl {
		return u.rel, u.relErr
	}
	u.rel, u.relErr = archaicFetchRelease()
	u.relAt = time.Now()
	if u.relErr != nil {
		log.Printf("Archaic update: %v", u.relErr)
	}
	return u.rel, u.relErr
}

func archaicGet(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "WRP/"+version)
	return archaicUpd.client.Do(req)
}

func archaicFetchRelease() (*archaicRelease, error) {
	resp, err := archaicGet(*archaicRel)
	if err != nil {
		return nil, fmt.Errorf("release: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release: HTTP %d from GitHub", resp.StatusCode)
	}
	var rel archaicRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("release: %v", err)
	}
	return &rel, nil
}

// the token at the start of b: the letters, digits and dots of a version
// or a system name
func archaicWord(b []byte) string {
	n := 0
	for n < len(b) && n < 32 && (b[n] >= '0' && b[n] <= '9' || b[n] >= 'a' && b[n] <= 'z' ||
		b[n] >= 'A' && b[n] <= 'Z' || b[n] == '.') {
		n++
	}
	return string(b[:n])
}

// The version and the system that a program of Archaic carries, or ""
func archaicMarks(b []byte) (ver, sys string) {
	if i := bytes.Index(b, []byte(archaicSysID)); i >= 0 {
		sys = archaicWord(b[i+len(archaicSysID):])
	}
	for o := 0; ; {
		i := bytes.Index(b[o:], []byte(archaicIdent))
		if i < 0 {
			break
		}
		o += i + len(archaicIdent)
		if o < len(b) && b[o] >= '0' && b[o] <= '9' {
			ver = archaicWord(b[o:])
			break
		}
	}
	return
}

// Archaic's file for system sys, checked, for an update
func archaicFor(sys string) (*archaicFile, error) {
	f, tag, err := archaicAssetFile(sys)
	if err != nil {
		return nil, err
	}
	if archaicSysName(f.sys) != archaicSysName(sys) {
		if f.sys == "" {
			return nil, fmt.Errorf("%s in release %s: an older build, with no update marks", f.name, tag)
		}
		return nil, fmt.Errorf("%s in release %s is for %s", f.name, tag, f.sys)
	}
	if f.version == "" {
		return nil, fmt.Errorf("%s in release %s: no version in it", f.name, tag)
	}
	return f, nil
}

// The release's file for system sys, with its size and SHA-256 checked:
// from the cache, or from GitHub. Also the release's tag.
func archaicAssetFile(sys string) (*archaicFile, string, error) {
	u := &archaicUpd
	u.Lock()
	defer u.Unlock()
	rel, err := archaicLatest()
	if err != nil {
		return nil, "", err
	}
	var a *archaicAsset
	for i := range rel.Assets {
		if archaicAssetSys(rel.Assets[i].Name) == archaicSysName(sys) {
			a = &rel.Assets[i]
			break
		}
	}
	if a == nil {
		return nil, rel.Tag, fmt.Errorf("no archaic-%s in release %s", archaicSysName(sys), rel.Tag)
	}
	name := a.Name
	key := a.URL + " " + a.Updated
	if f := u.files[name]; f != nil && f.key == key {
		if f.err == nil {
			return f, rel.Tag, nil
		}
		if time.Since(f.at) < archaicErrTTL { // not a download for each question
			return nil, rel.Tag, f.err
		}
	}
	f, err := archaicLoad(rel, a)
	if err != nil {
		f = &archaicFile{err: err, at: time.Now()}
	}
	f.key = key
	if u.files == nil {
		u.files = make(map[string]*archaicFile)
	}
	u.files[name] = f
	if err != nil {
		return nil, rel.Tag, err
	}
	return f, rel.Tag, nil
}

// release rel's file a, from GitHub: its size and SHA-256 must be the
// release's. Its marks are read, if it has them.
func archaicLoad(rel *archaicRelease, a *archaicAsset) (*archaicFile, error) {
	name := a.Name
	if a.Size <= 0 || a.Size > archaicMax {
		return nil, fmt.Errorf("%s in release %s: size %d", name, rel.Tag, a.Size)
	}
	resp, err := archaicGet(a.URL)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d from GitHub", name, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, archaicMax+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %v", name, err)
	}
	if int64(len(data)) != a.Size {
		return nil, fmt.Errorf("%s: %d bytes, the release says %d", name, len(data), a.Size)
	}
	if d, ok := strings.CutPrefix(a.Digest, "sha256:"); ok {
		s := sha256.Sum256(data)
		if !strings.EqualFold(d, hex.EncodeToString(s[:])) {
			return nil, fmt.Errorf("%s: its SHA-256 is not the release's", name)
		}
	}
	ver, sys := archaicMarks(data)
	f := &archaicFile{name: name, version: ver, sys: sys, data: data, crc: crc32.ChecksumIEEE(data)}
	log.Printf("Archaic: got %s %q from release %s, %d bytes, crc %08x", name, ver, rel.Tag, len(data), f.crc)
	return f, nil
}

// the systems' old names, and their new ones
var archaicOldSys = map[string]string{"unixpc": "3b1"}

// a system's name now, for its old name too
func archaicSysName(s string) string {
	if n, ok := archaicOldSys[s]; ok {
		return n
	}
	return s
}

// the system of a release's file: archaic-NAME, or archaic-NAME.prg (a
// program for the Atari). "" for other files.
func archaicAssetSys(name string) string {
	s, ok := strings.CutPrefix(name, "archaic-")
	if !ok {
		return ""
	}
	s = strings.TrimSuffix(s, ".prg")
	if !archaicSysOK(s) {
		return ""
	}
	return archaicSysName(s)
}

// a system's name: what Archaic's file name has after "archaic-"
func archaicSysOK(s string) bool {
	if len(s) < 1 || len(s) > 16 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func archaicServer(w http.ResponseWriter, r *http.Request) {
	sys, have, get := r.FormValue("p"), r.FormValue("v"), r.FormValue("f")
	f, err := archaicOffer(sys)
	if get != "" {
		if err == nil && f.version != get {
			err = fmt.Errorf("the newest is %s", f.version)
		}
		if err != nil {
			log.Printf("%s Archaic update: no file %q for %q: %v", r.RemoteAddr, get, sys, err)
			http.NotFound(w, r)
			return
		}
		log.Printf("%s Archaic update: sending %s %s, %d bytes", r.RemoteAddr, f.name, f.version, len(f.data))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(f.data)))
		w.Header().Set("Content-Disposition", `attachment; filename="`+f.name+`"`)
		w.Write(f.data)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, "archaic-update 1\n")
	if err != nil {
		log.Printf("%s Archaic update: %q at %q: %v", r.RemoteAddr, sys, have, err)
		fmt.Fprintf(w, "note %s\n", err)
		return
	}
	log.Printf("%s Archaic update: %s at %q, the newest is %s", r.RemoteAddr, sys, have, f.version)
	fmt.Fprintf(w, "version %s\nfile %s\nsize %d\ncrc %08x\nget archaic-update?p=%s&f=%s\n",
		f.version, f.name, len(f.data), f.crc, sys, f.version)
}

// the file for system sys, or why there is none
func archaicOffer(sys string) (*archaicFile, error) {
	switch {
	case *archaicRel == "":
		return nil, fmt.Errorf("updates are off in this WRP")
	case !archaicSysOK(sys):
		return nil, fmt.Errorf("no system name (p=)")
	}
	return archaicFor(sys)
}

// The release's files as they are, for a first install with curl or
// wget: /archaic-download/archaic-hpux9 (or /archaic-download/hpux9).
// /archaic-download alone: a text with the list of the files.
func archaicDownload(w http.ResponseWriter, r *http.Request) {
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/archaic-download"), "/")
	if name == "" {
		archaicList(w, r)
		return
	}
	if !strings.HasPrefix(name, "archaic-") {
		name = "archaic-" + name
	}
	sys := strings.TrimSuffix(strings.TrimPrefix(name, "archaic-"), ".prg")
	var f *archaicFile
	err := fmt.Errorf("updates and downloads are off in this WRP")
	if *archaicRel != "" {
		err = fmt.Errorf("no system name %q", sys)
		if archaicSysOK(sys) {
			var tag string
			f, tag, err = archaicAssetFile(sys)
			if err == nil && f.sys != "" && archaicSysName(f.sys) != archaicSysName(sys) {
				err = fmt.Errorf("%s in release %s is for %s", f.name, tag, f.sys)
			}
		}
	}
	if err != nil {
		log.Printf("%s Archaic download: %s: %v", r.RemoteAddr, name, err)
		http.Error(w, "archaic-download: "+err.Error(), http.StatusNotFound)
		return
	}
	log.Printf("%s Archaic download: sending %s %q, %d bytes", r.RemoteAddr, f.name, f.version, len(f.data))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(len(f.data)))
	w.Header().Set("Content-Disposition", `attachment; filename="`+f.name+`"`)
	w.Write(f.data)
}

// the list of Archaic's files in the release, and how to get one
func archaicList(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); host == "" && ok {
		host = a.String()
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "no-store")
	if *archaicRel == "" {
		fmt.Fprintf(w, "Archaic's updates and downloads are off in this WRP.\n")
		return
	}
	archaicUpd.Lock()
	rel, err := archaicLatest()
	archaicUpd.Unlock()
	if err != nil {
		fmt.Fprintf(w, "No Archaic here: %v\n", err)
		return
	}
	var names []string
	for _, a := range rel.Assets {
		if archaicAssetSys(a.Name) != "" {
			names = append(names, fmt.Sprintf("%-16s %8d bytes  http://%s/archaic-download/%s\n", a.Name, a.Size, host, a.Name))
		}
	}
	if len(names) == 0 {
		fmt.Fprintf(w, "No Archaic files in release %s.\n", rel.Tag)
		return
	}
	fmt.Fprintf(w, "Archaic, release %s. Get the file for your system, then install it as archaic, like:\n\n", rel.Tag)
	fmt.Fprintf(w, "  curl -O http://%s/archaic-download/archaic-hpux9\n", host)
	fmt.Fprintf(w, "  chmod 755 archaic-hpux9\n  mv archaic-hpux9 /usr/local/bin/archaic\n\n")
	for _, s := range names {
		fmt.Fprint(w, s)
	}
}

func init() {
	archaicUpd.client = &http.Client{Timeout: 60 * time.Second}
}
