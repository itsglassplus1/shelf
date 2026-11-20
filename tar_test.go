package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tarFile is an entry for writeTar; dir entries have no body.
type tarFile struct {
	name, body string
	dir        bool
}

// writeTar creates a tar at p with the given entries, in order.
func writeTar(t *testing.T, p string, files ...tarFile) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		h := &tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg, ModTime: time.Unix(1e9, 0)}
		if f.dir {
			h.Typeflag, h.Size, h.Mode = tar.TypeDir, 0, 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		io.WriteString(tw, f.body)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A long name needs a PAX header block before the file's own header.
var longTarName = "pages/" + strings.Repeat("x", 120) + "/p03.png"

func testTarFiles() []tarFile {
	return []tarFile{
		{name: "./pages/", dir: true},
		{name: "./pages/p10.png", body: "PAGE-TEN"},
		{name: "./pages/p2.png", body: "PAGE-TWO"},
		{name: longTarName, body: "PAGE-THREE"},
		{name: "./pages/.hidden.png", body: "hidden"},
		{name: "__MACOSX/pages/._p2.png", body: "junk"},
		{name: "notes.txt", body: "not a page"},
		{name: "ComicInfo.xml", body: `<ComicInfo><Title>Tar Title</Title><Number>12.5</Number></ComicInfo>`},
		{name: "./pages/p1.png", body: "PAGE-ONE"},
	}
}

func TestTarEntries(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ch.cbt")
	writeTar(t, p, testTarFiles()...)
	entries, err := tarEntries(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 8 { // all regular files, not the directory
		t.Fatalf("%d entries, want 8: %+v", len(entries), entries)
	}
	data, _ := os.ReadFile(p)
	want := map[string]string{}
	for _, f := range testTarFiles() {
		want[strings.TrimPrefix(f.name, "./")] = f.body
	}
	for _, e := range entries {
		if got := string(data[e.offset : e.offset+e.size]); got != want[e.name] {
			t.Errorf("%s at %d: %q, want %q", e.name, e.offset, got, want[e.name])
		}
	}

	pages, err := listTarPages(p)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, pg := range pages {
		names = append(names, pg.name)
	}
	if got, want := strings.Join(names, " "), "pages/p1.png pages/p2.png pages/p10.png "+longTarName; got != want {
		t.Errorf("pages %q, want %q", got, want)
	}

	raw, err := readComicInfoTar(p)
	if err != nil {
		t.Fatal(err)
	}
	if ci, err := parseComicInfo(bytes.NewReader(raw)); err != nil || ci.Title != "Tar Title" || ci.Number != "12.5" {
		t.Errorf("ComicInfo %+v, %v", ci, err)
	}
	if raw, err := readComicInfoTar(writeTarAt(t, tarFile{name: "a.png", body: "x"})); raw != nil || err != nil {
		t.Errorf("no ComicInfo: %q, %v; want nil, nil", raw, err)
	}
}

func writeTarAt(t *testing.T, files ...tarFile) string {
	p := filepath.Join(t.TempDir(), "x.cbt")
	writeTar(t, p, files...)
	return p
}

func TestArchiveFormatTar(t *testing.T) {
	p := writeTarAt(t, tarFile{name: "a.png", body: "x"})
	if got, err := archiveFormat(p); err != nil || got != "tar" {
		t.Errorf("tar: %q, %v", got, err)
	}

	// Gzipped tar: refused, saying why.
	data, _ := os.ReadFile(p)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(data)
	zw.Close()
	os.WriteFile(p, gz.Bytes(), 0o644)
	if _, err := archiveFormat(p); err == nil || !strings.Contains(err.Error(), "gzip") {
		t.Errorf("gzipped tar: %v, want an error naming gzip", err)
	}
}

func TestServerTar(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	root, data := t.TempDir(), t.TempDir()
	ch := filepath.Join(root, "comics", "Tar Series", "ch.cbt")
	writeTar(t, ch, testTarFiles()...)
	writeTar(t, filepath.Join(root, "comics", "Tar Series", "old.tar"), tarFile{name: "a.png", body: "OLD"})

	cat := newCatalog(config{root: root, categories: []string{"comics"},
		cacheDir: filepath.Join(data, "cache"), seriesDir: filepath.Join(data, "series"), cacheMax: 1 << 20})
	if err := cat.refresh(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newServer(cat, nil))
	t.Cleanup(srv.Close)
	base := srv.URL

	series := seriesByName(t, base)["Tar Series"]
	if series.BooksCount != 2 {
		t.Fatalf("Tar Series: %d books, want 2 (.cbt and .tar)", series.BooksCount)
	}
	var b bookDTO
	for _, x := range booksOf(t, base, series.ID) {
		if x.Name == "ch" {
			b = x
		}
	}
	if b.Metadata.Title != "Tar Title" || b.Metadata.NumberSort != 12.5 || b.Media.MediaType != "application/x-tar" {
		t.Errorf("book %q: title %q number %v media %q", b.Name, b.Metadata.Title, b.Metadata.NumberSort, b.Media.MediaType)
	}

	var list []pageDTO
	fetchJSON(t, "GET", base+"/api/v1/books/"+b.ID+"/pages", "", &list)
	for i, want := range []string{"PAGE-ONE", "PAGE-TWO", "PAGE-TEN", "PAGE-THREE"} {
		code, got := fetch(t, "GET", fmt.Sprintf("%s/api/v1/books/%s/pages/%d", base, b.ID, i+1), "")
		if code != 200 || got != want {
			t.Errorf("page %d: %d %q, want %q", i+1, code, got, want)
		}
	}
	if len(list) != 4 {
		t.Errorf("%d pages listed, want 4", len(list))
	}

	// Pages are served with Range support, like plain files.
	req, _ := http.NewRequest("GET", base+"/api/v1/books/"+b.ID+"/pages/1", nil)
	req.Header.Set("Range", "bytes=5-")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(part) != "ONE" {
		t.Errorf("range: %d %q, want 206 \"ONE\"", resp.StatusCode, part)
	}

	// Cover: the first page of the first chapter (ch.cbt sorts before old.tar).
	if code, got := fetch(t, "GET", base+"/api/v1/series/"+series.ID+"/thumbnail", ""); code != 200 || got != "PAGE-ONE" {
		t.Errorf("cover: %d %q", code, got)
	}

	// A changed archive is relisted, not served from stale offsets.
	writeTar(t, ch, tarFile{name: "header-padding.txt", body: strings.Repeat("-", 3000)}, tarFile{name: "p1.png", body: "NEW-ONE"})
	os.Chtimes(ch, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	if code, got := fetch(t, "GET", base+"/api/v1/books/"+b.ID+"/pages/1", ""); code != 200 || got != "NEW-ONE" {
		t.Errorf("after change: %d %q, want NEW-ONE", code, got)
	}
}
