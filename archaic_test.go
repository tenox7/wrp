package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// a program of Archaic: its update marks among other bytes
func archaicProg(ver, sys string) []byte {
	b := []byte("\x7fELF\x00\x01junk @(#)Archaic%s, 12 KB\x00")
	if sys != "" {
		b = append(b, "\x00\x00@(#)Archaic for "+sys+"\x00\x03"...)
	}
	if ver != "" {
		b = append(b, "\x00@(#)Archaic "+ver+"\x00\xff\xfe"...)
	}
	return append(b, make([]byte, 300)...)
}

// A fake GitHub: the release info and its files. bad: files whose
// digest is wrong.
type fakeGitHub struct {
	srv   *httptest.Server
	files map[string][]byte
	bad   map[string]bool
	asks  atomic.Int32 // release info asked for
	gets  atomic.Int32 // files sent
}

func newFakeGitHub(t *testing.T, files map[string][]byte) *fakeGitHub {
	g := &fakeGitHub{files: files, bad: map[string]bool{}}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/release" {
			g.asks.Add(1)
			rel := archaicRelease{Tag: "v0.9"}
			for n, b := range g.files {
				s := sha256.Sum256(b)
				d := "sha256:" + hex.EncodeToString(s[:])
				if g.bad[n] {
					d = "sha256:" + strings.Repeat("0", 64)
				}
				rel.Assets = append(rel.Assets, archaicAsset{Name: n, Size: int64(len(b)),
					URL: g.srv.URL + "/download/" + n, Digest: d, Updated: "2026-11-23T10:00:00Z"})
			}
			json.NewEncoder(w).Encode(rel)
			return
		}
		if n, ok := strings.CutPrefix(r.URL.Path, "/download/"); ok && g.files[n] != nil {
			http.Redirect(w, r, "/cdn/"+n, http.StatusFound) // as GitHub does
			return
		}
		if n, ok := strings.CutPrefix(r.URL.Path, "/cdn/"); ok && g.files[n] != nil {
			g.gets.Add(1)
			w.Write(g.files[n])
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(g.srv.Close)
	old := *archaicRel
	*archaicRel = g.srv.URL + "/release"
	t.Cleanup(func() { *archaicRel = old })
	archaicUpd.rel, archaicUpd.relErr, archaicUpd.relAt, archaicUpd.files = nil, nil, time.Time{}, nil
	return g
}

func archaicAsk(t *testing.T, q string) (int, string, http.Header) {
	t.Helper()
	w := httptest.NewRecorder()
	archaicServer(w, httptest.NewRequest("GET", "/archaic-update?"+q, nil))
	b, _ := io.ReadAll(w.Result().Body)
	return w.Code, string(b), w.Result().Header
}

func TestArchaicMarks(t *testing.T) {
	v, s := archaicMarks(archaicProg("0.10", "unixpc"))
	if v != "0.10" || s != "unixpc" {
		t.Errorf("marks: got %q %q, want 0.10 unixpc", v, s)
	}
	if v, s := archaicMarks(archaicProg("", "")); v != "" || s != "" {
		t.Errorf("no marks: got %q %q", v, s)
	}
}

func TestArchaicUpdate(t *testing.T) {
	prog := archaicProg("0.9", "unixpc")
	g := newFakeGitHub(t, map[string][]byte{
		"archaic-unixpc": prog,
		"archaic-idris":  archaicProg("", ""),        // a build of 0.8: no marks
		"archaic-sun4":   archaicProg("0.9", "sun3"), // the wrong file
		"archaic-hpux9":  archaicProg("0.9", "hpux9"),
	})
	g.bad["archaic-hpux9"] = true

	code, body, h := archaicAsk(t, "p=unixpc&v=0.8")
	want := fmt.Sprintf("archaic-update 1\nversion 0.9\nfile archaic-unixpc\nsize %d\ncrc %08x\nget archaic-update?p=unixpc&f=0.9\n",
		len(prog), crc32.ChecksumIEEE(prog))
	if code != 200 || body != want || h.Get("Content-Type") != "text/plain" {
		t.Errorf("answer: %d %q\n%s\nwant:\n%s", code, h.Get("Content-Type"), body, want)
	}
	code, body, h = archaicAsk(t, "p=unixpc&f=0.9")
	if code != 200 || body != string(prog) || h.Get("Content-Length") != fmt.Sprint(len(prog)) {
		t.Errorf("file: %d, %d bytes, Content-Length %s; want %d bytes", code, len(body), h.Get("Content-Length"), len(prog))
	}
	if code, _, _ = archaicAsk(t, "p=unixpc&f=0.8"); code != 404 {
		t.Errorf("file of another version: %d, want 404", code)
	}

	for _, c := range []struct{ q, note string }{
		{"p=idris&v=0.8", "an older build"},
		{"p=sun4&v=0.8", "is for sun3"},
		{"p=hpux9&v=0.8", "SHA-256"},
		{"p=dgux&v=0.8", "no archaic-dgux in release v0.9"},
		{"p=../x&v=0.8", "no system name"},
		{"v=0.8", "no system name"},
	} {
		code, body, _ := archaicAsk(t, c.q)
		if code != 200 || !strings.HasPrefix(body, "archaic-update 1\nnote ") || !strings.Contains(body, c.note) ||
			strings.Contains(body, "version") {
			t.Errorf("%s: %d %q, want a note with %q", c.q, code, body, c.note)
		}
		if code, _, _ := archaicAsk(t, strings.Replace(c.q, "v=0.8", "f=0.9", 1)); code != 404 {
			t.Errorf("%s: the file: %d, want 404", c.q, code)
		}
	}
	if n := g.asks.Load(); n != 1 {
		t.Errorf("GitHub was asked %d times, want once: the release info is kept", n)
	}
	// each file came once: the good ones are kept, a bad one's error too
	if n := g.gets.Load(); n != 4 {
		t.Errorf("GitHub sent %d files, want 4: one each", n)
	}
	archaicUpd.files["archaic-hpux9"].at = time.Now().Add(-archaicErrTTL - time.Second)
	archaicAsk(t, "p=hpux9&v=0.8")
	archaicAsk(t, "p=hpux9&v=0.8")
	if n := g.gets.Load(); n != 5 {
		t.Errorf("a bad file, its error older than %v: GitHub sent %d files, want 5", archaicErrTTL, n)
	}

	*archaicRel = ""
	if _, body, _ := archaicAsk(t, "p=unixpc&v=0.8"); !strings.Contains(body, "note updates are off") {
		t.Errorf("-archaic \"\": %q", body)
	}
}

// The 3B1's file is archaic-3b1 (its old name unixpc still works), and
// the Atari's are archaic-tos.prg and archaic-mint.prg
func TestArchaicNames(t *testing.T) {
	old, b3, tos := archaicProg("0.14", "unixpc"), archaicProg("0.15", "3b1"), archaicProg("0.14", "tos")
	newFakeGitHub(t, map[string][]byte{"archaic-3b1": old, "archaic-tos.prg": tos,
		"archaic-mint.prg": archaicProg("0.14", "tos")}) // the wrong file
	for _, p := range []string{"unixpc", "3b1"} {
		code, body, _ := archaicAsk(t, "p="+p+"&v=0.13")
		if code != 200 || !strings.Contains(body, "version 0.14\nfile archaic-3b1\n") ||
			!strings.Contains(body, "get archaic-update?p="+p+"&f=0.14\n") {
			t.Errorf("p=%s: %d %q", p, code, body)
		}
		if code, body, _ := archaicAsk(t, "p="+p+"&f=0.14"); code != 200 || body != string(old) {
			t.Errorf("p=%s, the file: %d, %d bytes", p, code, len(body))
		}
	}
	if _, body, _ := archaicAsk(t, "p=tos&v=0.13"); !strings.Contains(body, "version 0.14\nfile archaic-tos.prg\n") {
		t.Errorf("p=tos: %q", body)
	}
	if _, body, _ := archaicAsk(t, "p=mint&v=0.13"); !strings.Contains(body, "archaic-mint.prg in release v0.9 is for tos") {
		t.Errorf("p=mint: %q", body)
	}
	for _, p := range []string{"/archaic-download/archaic-tos.prg", "/archaic-download/tos", "/archaic-download/archaic-tos"} {
		code, body, h := archaicDl(t, p)
		if code != 200 || body != string(tos) || h.Get("Content-Disposition") != `attachment; filename="archaic-tos.prg"` {
			t.Errorf("%s: %d, %d bytes, %q", p, code, len(body), h.Get("Content-Disposition"))
		}
	}
	for _, p := range []string{"/archaic-download/unixpc", "/archaic-download/archaic-3b1"} {
		if code, body, _ := archaicDl(t, p); code != 200 || body != string(old) {
			t.Errorf("%s: %d, %d bytes", p, code, len(body))
		}
	}
	if _, body, _ := archaicDl(t, "/archaic-download"); !strings.Contains(body, "archaic-tos.prg") ||
		!strings.Contains(body, "archaic-3b1 ") {
		t.Errorf("the list: %q", body)
	}

	// a new build for the 3B1 carries its new name
	newFakeGitHub(t, map[string][]byte{"archaic-3b1": b3})
	for _, p := range []string{"unixpc", "3b1"} {
		if _, body, _ := archaicAsk(t, "p="+p+"&v=0.14"); !strings.Contains(body, "version 0.15\nfile archaic-3b1\n") {
			t.Errorf("new build, p=%s: %q", p, body)
		}
	}
}

// A new upload of a file in the release is a new file, after the release
// info is asked again
func TestArchaicNewUpload(t *testing.T) {
	g := newFakeGitHub(t, map[string][]byte{"archaic-sun3": archaicProg("0.9", "sun3")})
	if _, body, _ := archaicAsk(t, "p=sun3&v=0.8"); !strings.Contains(body, "version 0.9\n") {
		t.Fatalf("first: %q", body)
	}
	g.files["archaic-sun3"] = archaicProg("0.9.1", "sun3")
	archaicUpd.relAt = time.Now().Add(-archaicTTL - time.Second)
	if _, body, _ := archaicAsk(t, "p=sun3&v=0.9"); !strings.Contains(body, "version 0.9\n") {
		t.Errorf("same upload time: the kept file, got %q", body)
	}
	g.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/release" {
			b := g.files["archaic-sun3"]
			s := sha256.Sum256(b)
			json.NewEncoder(w).Encode(archaicRelease{Tag: "v0.9.1", Assets: []archaicAsset{{Name: "archaic-sun3",
				Size: int64(len(b)), URL: g.srv.URL + "/f", Digest: "sha256:" + hex.EncodeToString(s[:]),
				Updated: "2026-11-24T10:00:00Z"}}})
			return
		}
		w.Write(g.files["archaic-sun3"])
	})
	archaicUpd.relAt = time.Now().Add(-archaicTTL - time.Second)
	if _, body, _ := archaicAsk(t, "p=sun3&v=0.9"); !strings.Contains(body, "version 0.9.1\n") {
		t.Errorf("new upload: %q", body)
	}
}

func archaicDl(t *testing.T, path string) (int, string, http.Header) {
	t.Helper()
	w := httptest.NewRecorder()
	archaicDownload(w, httptest.NewRequest("GET", path, nil))
	b, _ := io.ReadAll(w.Result().Body)
	return w.Code, string(b), w.Result().Header
}

// The release's files as they are, for curl -O and wget, and their list
func TestArchaicDownload(t *testing.T) {
	hp, old := archaicProg("0.9", "hpux9"), archaicProg("", "")
	g := newFakeGitHub(t, map[string][]byte{
		"archaic-hpux9":  hp,
		"archaic-sun3":   old,                        // a build of 0.8: no marks, still given
		"archaic-sun4":   archaicProg("0.9", "sun3"), // the wrong file
		"archaic-unixpc": archaicProg("0.9", "unixpc"),
		"README.txt":     []byte("not Archaic"),
	})
	g.bad["archaic-unixpc"] = true

	for _, p := range []string{"/archaic-download/archaic-hpux9", "/archaic-download/hpux9"} {
		code, body, h := archaicDl(t, p)
		if code != 200 || body != string(hp) || h.Get("Content-Type") != "application/octet-stream" ||
			h.Get("Content-Length") != fmt.Sprint(len(hp)) ||
			h.Get("Content-Disposition") != `attachment; filename="archaic-hpux9"` {
			t.Errorf("%s: %d, %d bytes, %q %q %q", p, code, len(body), h.Get("Content-Type"),
				h.Get("Content-Length"), h.Get("Content-Disposition"))
		}
	}
	if code, body, _ := archaicDl(t, "/archaic-download/archaic-sun3"); code != 200 || body != string(old) {
		t.Errorf("an older build: %d, %d bytes; want it as it is", code, len(body))
	}
	for _, c := range []struct{ path, why string }{
		{"/archaic-download/archaic-sun4", "is for sun3"},
		{"/archaic-download/archaic-unixpc", "SHA-256"},
		{"/archaic-download/archaic-dgux", "no archaic-dgux in release v0.9"},
		{"/archaic-download/README.txt", "no system name"},
		{"/archaic-download/x/y", "no system name"},
	} {
		code, body, _ := archaicDl(t, c.path)
		if code != 404 || !strings.Contains(body, c.why) {
			t.Errorf("%s: %d %q, want 404 with %q", c.path, code, body, c.why)
		}
	}

	code, body, h := archaicDl(t, "/archaic-download")
	for _, s := range []string{"release v0.9", "curl -O http://example.com/archaic-download/archaic-hpux9\n",
		fmt.Sprintf("archaic-hpux9    %8d bytes  http://example.com/archaic-download/archaic-hpux9\n", len(hp)),
		"http://example.com/archaic-download/archaic-sun3\n"} {
		if code != 200 || !strings.Contains(body, s) || h.Get("Content-Type") != "text/plain" {
			t.Errorf("the list: %d %q:\n%s\nwant %q", code, h.Get("Content-Type"), body, s)
		}
	}
	if strings.Contains(body, "README") {
		t.Errorf("the list has a file that is not Archaic:\n%s", body)
	}

	*archaicRel = ""
	if code, body, _ := archaicDl(t, "/archaic-download/archaic-hpux9"); code != 404 || !strings.Contains(body, "off") {
		t.Errorf("-archaic \"\": %d %q", code, body)
	}
	if _, body, _ := archaicDl(t, "/archaic-download/"); !strings.Contains(body, "off") {
		t.Errorf("-archaic \"\", the list: %q", body)
	}
}
