package main

import (
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Image heads for tests: just the signature bytes each format starts with.
var (
	pngHead  = "\x89PNG\r\n\x1a\nrest"
	jpegHead = "\xff\xd8\xff\xe0rest"
	// ftyp box: size 24, major brand, minor version, compatible brands.
	avifHead = "\x00\x00\x00\x18ftypmif1\x00\x00\x00\x00mif1avif"
	heifHead = "\x00\x00\x00\x18ftypheic\x00\x00\x00\x00mif1heic"
)

func TestSniffImage(t *testing.T) {
	for head, want := range map[string]string{
		jpegHead:                         "image/jpeg",
		pngHead:                          "image/png",
		"GIF89a...":                      "image/gif",
		"GIF87a...":                      "image/gif",
		"RIFF\x10\x00\x00\x00WEBPVP8 ":   "image/webp",
		"RIFF\x10\x00\x00\x00WAVEfmt ":   "",
		"\xff\x0a\xfa":                   "image/jxl",
		"\x00\x00\x00\x0cJXL \r\n\x87\n": "image/jxl",
		avifHead:                         "image/avif", // AVIF brand among HEIF's general ones
		heifHead:                         "image/heif",
		"\x00\x00\x00\x14ftypmif1\x00\x00\x00\x00mif1": "image/heif",
		"\x00\x00\x00\x14ftypisom\x00\x00\x00\x00mp41": "", // an MP4 video
		"\x00\x00\x00\x18ftyp":                         "", // truncated
		"<?xml version":                                "",
		"":                                             "",
	} {
		if got := sniffImage([]byte(head)); got != want {
			t.Errorf("sniffImage(%q) = %q, want %q", head, got, want)
		}
	}
}

func TestImageMediaType(t *testing.T) {
	noRead := func() []byte { t.Error("head read although the name tells"); return nil }
	if got := imageMediaType("a/P01.JPG", noRead); got != "image/jpeg" {
		t.Errorf("by extension: %q", got)
	}
	if got := imageMediaType("ComicInfo.xml", noRead); got != "" {
		t.Errorf("never-image extension: %q", got)
	}
	for name, head := range map[string]string{"page.jfif": jpegHead, "001": pngHead, "scan.bin": avifHead} {
		if got := imageMediaType(name, func() []byte { return []byte(head) }); got == "" {
			t.Errorf("%s: not found by content", name)
		}
	}
	if got := imageMediaType("readme", func() []byte { return []byte("plain text") }); got != "" {
		t.Errorf("text without extension: %q", got)
	}
	if got := imageMediaType("gone", func() []byte { return nil }); got != "" {
		t.Errorf("unreadable: %q", got)
	}
}

func TestPagesFoundByContent(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	root, data := t.TempDir(), t.TempDir()
	series := filepath.Join(root, "c", "S")

	// Folder: "1" and "3.jfif" are images by content; "2.png" by name;
	// "notes" and "credits.txt" aren't images.
	dir := filepath.Join(series, "ch1")
	os.MkdirAll(dir, 0o755)
	for name, body := range map[string]string{
		"1": pngHead + "A", "2.png": "B", "3.jfif": jpegHead + "C", "notes": "text", "credits.txt": pngHead,
	} {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	// Zip and tar: the same, as archive entries.
	writeZip(t, filepath.Join(series, "ch2.cbz"),
		"1", pngHead+"A", "2.png", "B", "3.jfif", jpegHead+"C", "notes", "text", "ComicInfo.xml", "<ComicInfo/>")
	writeTar(t, filepath.Join(series, "ch3.cbt"),
		tarFile{name: "1", body: pngHead + "A"}, tarFile{name: "2.png", body: "B"},
		tarFile{name: "3.jfif", body: jpegHead + "C"}, tarFile{name: "notes", body: "text"})
	// The series cover: "cover" without extension, an image by content.
	// "COVER.dat" comes first in name order but isn't an image; "cover.txt"
	// is never taken as one.
	os.WriteFile(filepath.Join(series, "COVER.dat"), []byte("not an image"), 0o644)
	os.WriteFile(filepath.Join(series, "cover"), []byte(avifHead+"cover"), 0o644)
	os.WriteFile(filepath.Join(series, "cover.txt"), []byte(pngHead), 0o644)

	cat := newCatalog(config{root: root, categories: []string{"c"},
		cacheDir: filepath.Join(data, "cache"), seriesDir: filepath.Join(data, "series"), cacheMax: 1 << 20})
	if err := cat.refresh(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newServer(cat, nil))
	t.Cleanup(srv.Close)
	base := srv.URL

	s := seriesByName(t, base)["S"]
	wantTypes := []string{"image/png", "image/png", "image/jpeg"}
	wantBodies := []string{pngHead + "A", "B", jpegHead + "C"}
	for _, b := range booksOf(t, base, s.ID) {
		var list []pageDTO
		fetchJSON(t, "GET", base+"/api/v1/books/"+b.ID+"/pages", "", &list)
		if len(list) != 3 {
			t.Errorf("%s: %d pages, want 3: %+v", b.Name, len(list), list)
			continue
		}
		for i, p := range list {
			code, got := fetch(t, "GET", fmt.Sprintf("%s/api/v1/books/%s/pages/%d", base, b.ID, i+1), "")
			if p.MediaType != wantTypes[i] || code != 200 || got != wantBodies[i] {
				t.Errorf("%s page %d (%s): %s, %d %q; want %s %q", b.Name, i+1, p.FileName, p.MediaType, code, got, wantTypes[i], wantBodies[i])
			}
		}
		if b.Name == "ch1" && b.SizeBytes != int64(len(pngHead+"A")+len("B")+len(jpegHead+"C")) {
			t.Errorf("ch1 size %d counts more than its pages", b.SizeBytes)
		}
	}

	code, got := fetch(t, "GET", base+"/api/v1/series/"+s.ID+"/thumbnail", "")
	if code != 200 || got != avifHead+"cover" {
		t.Errorf("cover: %d %q, want the \"cover\" file", code, got)
	}
}

func TestCoverCopyNamedByContent(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	root, data := t.TempDir(), t.TempDir()
	// No cover file: the copy is made from page 1, which has no extension.
	writeZip(t, filepath.Join(root, "c", "S", "ch1.cbz"), "001", jpegHead+"X")
	cat := newCatalog(config{root: root, categories: []string{"c"},
		cacheDir: filepath.Join(data, "cache"), seriesDir: filepath.Join(data, "series"), cacheMax: 1 << 20})
	if err := cat.refresh(); err != nil {
		t.Fatal(err)
	}
	cover, err := cat.seriesCover(cat.current().series[0])
	if err != nil || !strings.HasSuffix(cover, string(filepath.Separator)+"cover.jpg") {
		t.Errorf("cover copy %q, %v; want cover.jpg", cover, err)
	}
}
