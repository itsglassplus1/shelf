package main

import (
	"archive/zip"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const testOPF = `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Ep Title</dc:title>
    <dc:creator>Author A</dc:creator>
    <dc:publisher>Pub</dc:publisher>
    <dc:description>&lt;p&gt;Some &amp;amp; &lt;b&gt;desc&lt;/b&gt;&lt;/p&gt;</dc:description>
    <dc:date>2021-03-04T00:00:00Z</dc:date>
    <meta property="dcterms:modified">2022-01-01T00:00:00Z</meta>
  </metadata>
  <manifest>
    <item id="p2" href="text/p2.xhtml" media-type="application/xhtml+xml"/>
    <item id="p1" href="text/p1.xhtml" media-type="application/xhtml+xml"/>
    <item id="svg" href="images/a.jpg" media-type="image/jpeg"/>
  </manifest>
  <spine><itemref idref="p1"/><itemref idref="svg"/><itemref idref="missing"/><itemref idref="p2"/></spine>
</package>`

// Page 1: an %-escaped relative image, an SVG-wrapped image, and images that
// aren't in the archive. Page 2: HTML entities, unclosed tags, an absolute path.
const testPage1 = `<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:xlink="http://www.w3.org/1999/xlink"><body>
<img src="../images/z%20last.png#frag"/>
<svg><image width="1" xlink:href="../images/a.jpg"/></svg>
<img src="http://example.com/remote.png"/><img src="../images/gone.png"/>
<img src="../images/odd%20name.png"/><img src="../images/odd#1.png"/>
</body></html>`

const testPage2 = `<html><body><p>&nbsp;text<br><IMG SRC="/OPS/images/b.png"></body></html>`

// writeEpub creates an EPUB at p whose reading order (z last, a, odd…, b)
// differs from name order.
func writeEpub(t *testing.T, p string) {
	t.Helper()
	writeZip(t, p,
		"mimetype", "application/epub+zip",
		"META-INF/container.xml", `<container><rootfiles><rootfile full-path="OPS/package.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`,
		"OPS/package.opf", testOPF,
		"OPS/text/p1.xhtml", testPage1,
		"OPS/text/p2.xhtml", testPage2,
		"OPS/images/z last.png", "IMG-Z",
		"OPS/images/a.jpg", "IMG-A",
		"OPS/images/b.png", "IMG-B",
		"OPS/images/odd%20name.png", "IMG-PCT", // names Mihon finds taken literally
		"OPS/images/odd#1.png", "IMG-HASH",
		"OPS/images/extra.png", "EXTRA",
	)
}

func pageNames(t *testing.T, p string) []string {
	t.Helper()
	r, err := zip.OpenReader(p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	pages, err := epubPages(&r.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, pg := range pages {
		if r.File[pg.zipIndex].Name != pg.name {
			t.Errorf("page %s: zipIndex points at %s", pg.name, r.File[pg.zipIndex].Name)
		}
		names = append(names, pg.name)
	}
	return names
}

func TestEpubPages(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ch.epub")
	writeEpub(t, p)
	want := []string{"OPS/images/z last.png", "OPS/images/a.jpg", "OPS/images/odd%20name.png", "OPS/images/odd#1.png", "OPS/images/b.png"}
	if got := pageNames(t, p); !slices.Equal(got, want) {
		t.Errorf("pages %q, want %q", got, want)
	}
}

func TestResolveEpubPath(t *testing.T) {
	for _, tc := range [][3]string{
		{"OPS/text/p.xhtml", "../img/a.png", "OPS/img/a.png"},
		{"OPS/text/p.xhtml", "a%20b.png#x", "OPS/text/a b.png"},
		{"OPS/text/p.xhtml", "/OPS/img/a.png", "OPS/img/a.png"},
		{"OPS/package.opf", `text\p.xhtml`, "OPS/text/p.xhtml"},
		{"package.opf", "p.xhtml", "p.xhtml"},
		{"OPS/p.xhtml", "../../../a.png", "a.png"},
	} {
		if got := resolveEpubPath(tc[0], tc[1], true); got != tc[2] {
			t.Errorf("resolveEpubPath(%q, %q) = %q, want %q", tc[0], tc[1], got, tc[2])
		}
	}
}

func TestReadEpubInfo(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ch.epub")
	writeEpub(t, p)
	ci, err := readEpubInfo(p)
	if err != nil {
		t.Fatal(err)
	}
	if ci.Title != "Ep Title" || ci.Summary != "Some & desc" || ci.ReleaseDate != "2021-03-04" {
		t.Errorf("title %q summary %q date %q", ci.Title, ci.Summary, ci.ReleaseDate)
	}
	if !slices.Equal(ci.Translators, []string{"Pub"}) || !slices.Equal(ci.Authors, []authorDTO{{"Author A", "writer"}}) {
		t.Errorf("scanlator %q authors %v", ci.Translators, ci.Authors)
	}

	// No publisher: the creator is the scanlator; no dc:date: dcterms:modified.
	writeZip(t, p,
		"META-INF/container.xml", `<container><rootfiles><rootfile full-path="c.opf"/></rootfiles></container>`,
		"c.opf", `<package><metadata><dc:creator>Solo</dc:creator><meta property="dcterms:modified">2019-07</meta></metadata></package>`)
	if ci, err = readEpubInfo(p); err != nil || !slices.Equal(ci.Translators, []string{"Solo"}) || ci.ReleaseDate != "2019-07-01" {
		t.Errorf("fallbacks: %+v, %v", ci, err)
	}
}

func TestServerEpub(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	root, data := t.TempDir(), t.TempDir()
	writeEpub(t, filepath.Join(root, "books", "Epub Series", "Ch 1.epub"))
	// No package document: all images, in name order.
	writeZip(t, filepath.Join(root, "books", "Epub Series", "Ch 2.epub"), "mimetype", "application/epub+zip",
		"i/2.png", "TWO", "i/10.png", "TEN", "i/1.png", "ONE")

	cat := newCatalog(config{root: root, categories: []string{"books"},
		cacheDir: filepath.Join(data, "cache"), seriesDir: filepath.Join(data, "series"), cacheMax: 1 << 20})
	if err := cat.refresh(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newServer(cat, nil))
	t.Cleanup(srv.Close)
	base := srv.URL

	series := seriesByName(t, base)["Epub Series"]
	// Series author and description come from the newest EPUB, Ch 2, which
	// has none; then nothing (not Ch 1's): like ComicInfo, only the newest counts.
	if series.BooksCount != 2 || series.Metadata.Summary != "" {
		t.Errorf("series: %d books, summary %q", series.BooksCount, series.Metadata.Summary)
	}
	books := booksOf(t, base, series.ID) // newest first
	if len(books) != 2 {
		t.Fatalf("%d books", len(books))
	}
	ch1, ch2 := books[1], books[0]
	if ch1.Metadata.Title != "Ep Title" || ch1.Metadata.NumberSort != 1 || ch1.Media.MediaType != "application/epub+zip" ||
		ch1.Media.MediaProfile != "DIVINA" || ch1.Metadata.ReleaseDate == nil || *ch1.Metadata.ReleaseDate != "2021-03-04" {
		t.Errorf("Ch 1: %+v %+v", ch1.Metadata, ch1.Media)
	}
	if !slices.Contains(ch1.Metadata.Authors, authorDTO{"Pub", "translator"}) {
		t.Errorf("Ch 1 scanlator: %v", ch1.Metadata.Authors)
	}
	for book, want := range map[string][]string{ch1.ID: {"IMG-Z", "IMG-A", "IMG-PCT", "IMG-HASH", "IMG-B"}, ch2.ID: {"ONE", "TWO", "TEN"}} {
		for i, w := range want {
			if code, got := fetch(t, "GET", fmt.Sprintf("%s/api/v1/books/%s/pages/%d", base, book, i+1), ""); code != 200 || got != w {
				t.Errorf("book %s page %d: %d %q, want %q", book, i+1, code, got, w)
			}
		}
	}
	// Cover: the first page of the first chapter, in reading order.
	if code, got := fetch(t, "GET", base+"/api/v1/series/"+series.ID+"/thumbnail", ""); code != 200 || got != "IMG-Z" {
		t.Errorf("cover: %d %q", code, got)
	}

	// With only Ch 1, the series takes its author and description.
	os.Remove(filepath.Join(root, "books", "Epub Series", "Ch 2.epub"))
	cat.checkForChanges()
	series = seriesByName(t, base)["Epub Series"]
	if series.Metadata.Summary != "Some & desc" || !slices.Contains(series.BooksMetadata.Authors, authorDTO{"Author A", "writer"}) {
		t.Errorf("series from EPUB: summary %q authors %v", series.Metadata.Summary, series.BooksMetadata.Authors)
	}
}
