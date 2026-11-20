package main

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// EPUB chapters, read like Mihon's local source does (EpubReader.kt and
// EpubReaderExtensions.kt): an .epub is a zip whose pages are the images of
// its XHTML pages, in reading order, and whose metadata comes from its
// package document (OPF), not from a ComicInfo.xml.
//
//   - META-INF/container.xml names the package document (else
//     OEBPS/content.opf);
//   - its spine lists the XHTML pages in reading order; the pages of a
//     chapter are every <img src> and <svg:image xlink:href> in them, in order;
//   - dc:title is the chapter name, dc:publisher (else dc:creator) its
//     scanlator, dc:date (else dcterms:modified) its date; dc:creator and
//     dc:description are the series author and description when the series
//     has no ComicInfo.xml.
//
// Unlike Mihon, images that aren't in the archive are skipped instead of
// failing the chapter, %-escaped paths are decoded, and an unreadable package
// document falls back to all images in natural order. The extension only
// lists EPUBs offered as image pages, which is how they are offered here.

// isEpub reports whether a chapter is an EPUB (by extension, like Mihon).
func isEpub(b *Book) bool {
	return !b.IsDir && strings.EqualFold(filepath.Ext(b.Path), ".epub")
}

type epubContainer struct {
	Rootfiles []struct {
		FullPath string `xml:"full-path,attr"`
	} `xml:"rootfiles>rootfile"`
}

// epubPackage is the part of an OPF package document shelf uses. Elements
// match by local name, so dc:title and title are both found.
type epubPackage struct {
	Metadata struct {
		Titles       []string `xml:"title"`
		Creators     []string `xml:"creator"`
		Publishers   []string `xml:"publisher"`
		Descriptions []string `xml:"description"`
		Dates        []string `xml:"date"`
		Metas        []struct {
			Property string `xml:"property,attr"`
			Value    string `xml:",chardata"`
		} `xml:"meta"`
	} `xml:"metadata"`
	Items []struct {
		ID        string `xml:"id,attr"`
		Href      string `xml:"href,attr"`
		MediaType string `xml:"media-type,attr"`
	} `xml:"manifest>item"`
	Spine []struct {
		IDRef string `xml:"idref,attr"`
	} `xml:"spine>itemref"`
}

// epubZip gives access to an EPUB's files by normalized path ("/" separated,
// as some EPUBs use "\").
type epubZip struct {
	files map[string]int // normalized name -> index in zr.File
	zr    *zip.Reader
}

func newEpubZip(zr *zip.Reader) *epubZip {
	e := &epubZip{files: map[string]int{}, zr: zr}
	for i, f := range zr.File {
		e.files[strings.ReplaceAll(f.Name, `\`, "/")] = i
	}
	return e
}

// read returns a file of at most 4 MB, or an error if it is missing.
func (e *epubZip) read(name string) ([]byte, error) {
	i, ok := e.files[name]
	if !ok {
		return nil, fmt.Errorf("%s: not in the archive", name)
	}
	rc, err := e.zr.File[i].Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, 4<<20+1))
	if err == nil && len(data) > 4<<20 {
		err = fmt.Errorf("%s: larger than 4 MB", name)
	}
	return data, err
}

func xmlDecoder(data []byte) *xml.Decoder {
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.CharsetReader = func(cs string, in io.Reader) (io.Reader, error) {
		if strings.EqualFold(cs, "utf-8") || strings.EqualFold(cs, "utf8") || cs == "" {
			return in, nil
		}
		return nil, fmt.Errorf("unsupported encoding %q", cs)
	}
	return dec
}

// packageDocument returns the path and contents of the package document.
func (e *epubZip) packageDocument() (string, *epubPackage, error) {
	ref := "OEBPS/content.opf"
	if data, err := e.read("META-INF/container.xml"); err == nil {
		var c epubContainer
		if xmlDecoder(data).Decode(&c) == nil && len(c.Rootfiles) > 0 && c.Rootfiles[0].FullPath != "" {
			ref = strings.ReplaceAll(c.Rootfiles[0].FullPath, `\`, "/")
		}
	}
	data, err := e.read(ref)
	if err != nil {
		return "", nil, err
	}
	var pkg epubPackage
	if err := xmlDecoder(data).Decode(&pkg); err != nil {
		return "", nil, fmt.Errorf("%s: %w", ref, err)
	}
	return ref, &pkg, nil
}

// resolveEpubPath resolves href (as written in a file at from) to a file
// path in the archive: relative to from's folder, or to the root if it
// starts with "/". With asURL, href is read as a URL: fragments and queries
// are dropped and %-escapes decoded; without, it is taken literally, as
// Mihon does.
func resolveEpubPath(from, href string, asURL bool) string {
	href = strings.ReplaceAll(href, `\`, "/")
	if asURL {
		if i := strings.IndexAny(href, "#?"); i >= 0 {
			href = href[:i]
		}
		if u, err := url.PathUnescape(href); err == nil {
			href = u
		}
	}
	if !strings.HasPrefix(href, "/") {
		href = path.Join("/"+path.Dir(from), href)
	}
	return strings.TrimPrefix(path.Clean(href), "/") // rooted: ".." stops at the root
}

// find returns the archive file href (in a file at from) refers to: read as
// a URL, as the EPUB spec has it, else taken literally like Mihon does, so
// every file Mihon finds is found too (e.g. one really named "a%20b.png").
func (e *epubZip) find(from, href string) (string, bool) {
	for _, asURL := range []bool{true, false} {
		if name := resolveEpubPath(from, href, asURL); name != "" {
			if _, ok := e.files[name]; ok {
				return name, true
			}
		}
	}
	return "", false
}

// epubPages returns the page images of an EPUB in reading order.
func epubPages(zr *zip.Reader) ([]pageEntry, error) {
	e := newEpubZip(zr)
	ref, pkg, err := e.packageDocument()
	if err != nil {
		return nil, err
	}
	xhtml := map[string]string{} // manifest id -> href of an XHTML page
	for _, it := range pkg.Items {
		if it.MediaType == "application/xhtml+xml" {
			xhtml[it.ID] = it.Href
		}
	}
	pages := []pageEntry{}
	for _, ir := range pkg.Spine {
		href, ok := xhtml[ir.IDRef]
		if !ok {
			continue
		}
		pagePath, ok := e.find(ref, href)
		if !ok {
			continue // a missing page: Mihon fails the chapter; skip it
		}
		data, err := e.read(pagePath)
		if err != nil {
			continue
		}
		for _, src := range imageRefs(data) {
			name, ok := e.find(pagePath, src)
			mt := imageTypes[strings.ToLower(path.Ext(name))]
			if !ok || mt == "" {
				continue // not in the archive (e.g. a URL), or not an image the extension takes
			}
			i := e.files[name]
			pages = append(pages, pageEntry{name: zr.File[i].Name, mediaType: mt, zipIndex: i})
		}
	}
	return pages, nil
}

// imageRefs returns the image references of an XHTML page, in document
// order: <img src> and <image xlink:href> (or SVG 2's plain href). The page
// is read leniently, like an HTML parser would; reading stops at the first
// error it can't get past.
func imageRefs(page []byte) []string {
	dec := xmlDecoder(page)
	dec.Strict = false
	dec.AutoClose = xml.HTMLAutoClose
	dec.Entity = xml.HTMLEntity
	var refs []string
	for {
		tok, err := dec.Token()
		if err != nil {
			return refs
		}
		el, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		for _, a := range el.Attr {
			switch {
			// HTML names are case-insensitive (SVG's aren't, but no one writes <IMAGE>).
			case strings.EqualFold(el.Name.Local, "img") && strings.EqualFold(a.Name.Local, "src"),
				strings.EqualFold(el.Name.Local, "image") && strings.EqualFold(a.Name.Local, "href"):
				refs = append(refs, strings.TrimSpace(a.Value))
			}
		}
	}
}

// ---- metadata ----------------------------------------------------------------

var htmlTag = regexp.MustCompile(`<[^>]*>`)

// epubText returns the first non-blank value, with HTML tags (common in
// dc:description) removed.
func epubText(values []string) string {
	for _, v := range values {
		v = strings.TrimSpace(html.UnescapeString(htmlTag.ReplaceAllString(v, " ")))
		if v = strings.Join(strings.Fields(v), " "); v != "" {
			return v
		}
	}
	return ""
}

var epubDatePrefix = regexp.MustCompile(`^\s*(\d{4})(?:-(\d{2})(?:-(\d{2}))?)?`)

// readEpubInfo returns an EPUB's metadata as a comicInfo: chapter Title,
// Translators (the scanlator), ReleaseDate; and for the series Authors (the
// creator, as writer) and Summary.
func readEpubInfo(file string) (*comicInfo, error) {
	r, err := zip.OpenReader(file)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	_, pkg, err := newEpubZip(&r.Reader).packageDocument()
	if err != nil {
		return nil, err
	}
	m := pkg.Metadata
	ci := &comicInfo{
		Title:   epubText(m.Titles),
		Summary: epubText(m.Descriptions),
		Genres:  []string{}, Tags: []string{}, Authors: []authorDTO{}, Translators: []string{},
	}
	creator := epubText(m.Creators)
	if creator != "" {
		ci.Authors = append(ci.Authors, authorDTO{Name: creator, Role: "writer"})
	}
	if scanlator := epubText(m.Publishers); scanlator != "" {
		ci.Translators = append(ci.Translators, scanlator)
	} else if creator != "" {
		ci.Translators = append(ci.Translators, creator)
	}
	date := epubText(m.Dates)
	if date == "" {
		for _, meta := range m.Metas {
			if meta.Property == "dcterms:modified" {
				date = strings.TrimSpace(meta.Value)
				break
			}
		}
	}
	if d := epubDatePrefix.FindStringSubmatch(date); d != nil {
		ci.ReleaseDate = releaseDate(d[1], d[2], d[3])
	}
	return ci, nil
}
