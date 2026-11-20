package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ComicInfo.xml support.
//
// Series metadata comes from <series>/ComicInfo.xml in the library, else from
// the copy in the data dir's series folder, else from the latest chapter's
// ComicInfo.xml, which is then copied there (Suwayomi repeats the series info
// in every chapter it downloads). Chapter metadata (title, number,
// translator, date) comes from each chapter's own ComicInfo.xml, read when a
// series' chapter list is requested and cached until the chapter changes.
// In a solid RAR that means decoding the archive up to ComicInfo.xml (often
// the last file), so a series of solid .cbr chapters is slow to list the
// first time after a restart; other archives read just that one file.

const comicInfoName = "ComicInfo.xml"

// comicInfoXML matches elements by local name, so namespaced extras such as
// <ty:PublishingStatusTachiyomi> are picked up too.
type comicInfoXML struct {
	Title       string `xml:"Title"`
	Series      string `xml:"Series"`
	Number      string `xml:"Number"`
	Summary     string `xml:"Summary"`
	Writer      string `xml:"Writer"`
	Penciller   string `xml:"Penciller"`
	Inker       string `xml:"Inker"`
	Colorist    string `xml:"Colorist"`
	Letterer    string `xml:"Letterer"`
	CoverArtist string `xml:"CoverArtist"`
	Editor      string `xml:"Editor"`
	Translator  string `xml:"Translator"`
	Publisher   string `xml:"Publisher"`
	Genre       string `xml:"Genre"`
	Tags        string `xml:"Tags"`
	Year        string `xml:"Year"`
	Month       string `xml:"Month"`
	Day         string `xml:"Day"`
	Status      string `xml:"PublishingStatusTachiyomi"`
}

type comicInfo struct {
	Title, Series, Number, Summary, Publisher string
	Status                                    string // Komga status: ONGOING, ENDED, ABANDONED, HIATUS, or ""
	Genres, Tags                              []string
	Authors                                   []authorDTO // creators (not translators)
	Translators                               []string
	ReleaseDate                               string // YYYY-MM-DD or ""
}

func parseComicInfo(r io.Reader) (*comicInfo, error) {
	var x comicInfoXML
	dec := xml.NewDecoder(io.LimitReader(r, 1<<20))
	dec.CharsetReader = func(cs string, in io.Reader) (io.Reader, error) {
		if strings.EqualFold(cs, "utf-8") || strings.EqualFold(cs, "utf8") || cs == "" {
			return in, nil
		}
		return nil, fmt.Errorf("unsupported encoding %q", cs)
	}
	if err := dec.Decode(&x); err != nil {
		return nil, err
	}

	ci := &comicInfo{
		Title:     strings.TrimSpace(x.Title),
		Series:    strings.TrimSpace(x.Series),
		Number:    strings.TrimSpace(x.Number),
		Summary:   strings.TrimSpace(x.Summary),
		Publisher: strings.TrimSpace(x.Publisher),
		Status:    komgaStatus(x.Status),
		Genres:    splitList(x.Genre),
		Tags:      splitList(x.Tags),
	}
	// Role names as Komga uses them; the extension shows writer as Author
	// and penciller as Artist.
	for _, r := range []struct{ role, names string }{
		{"writer", x.Writer}, {"penciller", x.Penciller}, {"inker", x.Inker},
		{"colorist", x.Colorist}, {"letterer", x.Letterer}, {"cover", x.CoverArtist},
		{"editor", x.Editor},
	} {
		for _, n := range splitList(r.names) {
			ci.Authors = append(ci.Authors, authorDTO{Name: n, Role: r.role})
		}
	}
	ci.Translators = splitList(x.Translator)
	ci.ReleaseDate = releaseDate(x.Year, x.Month, x.Day)
	return ci, nil
}

// splitList splits a comma-separated ComicInfo field, dropping blanks and duplicates.
func splitList(s string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if k := strings.ToLower(p); p != "" && !seen[k] {
			seen[k] = true
			out = append(out, p)
		}
	}
	return out
}

// komgaStatus maps Mihon's PublishingStatusTachiyomi values to Komga's.
func komgaStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ongoing":
		return "ONGOING"
	case "completed", "publishing finished":
		return "ENDED"
	case "cancelled", "canceled":
		return "ABANDONED"
	case "on hiatus", "hiatus":
		return "HIATUS"
	}
	return ""
}

func releaseDate(y, m, d string) string {
	year, err := strconv.Atoi(strings.TrimSpace(y))
	if err != nil || year < 1 || year > 9999 {
		return ""
	}
	month, err := strconv.Atoi(strings.TrimSpace(m))
	if err != nil || month < 1 || month > 12 {
		month = 1
	}
	day, err := strconv.Atoi(strings.TrimSpace(d))
	if err != nil || day < 1 || day > 31 {
		day = 1
	}
	t := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if t.Day() != day { // e.g. 31 February
		t = time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	}
	return t.Format("2006-01-02")
}

// readComicInfoZip returns the raw ComicInfo.xml of a zip, preferring one at
// the root. Returns nil, nil if the archive has none.
func readComicInfoZip(p string) ([]byte, error) {
	r, err := zip.OpenReader(p)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var found *zip.File
	for _, f := range r.File {
		if strings.EqualFold(path.Base(f.Name), comicInfoName) && !strings.HasPrefix(f.Name, "__MACOSX/") {
			if found == nil || !strings.Contains(f.Name, "/") {
				found = f
			}
		}
	}
	if found == nil {
		return nil, nil
	}
	rc, err := found.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, 1<<20+1))
	if err == nil && len(data) > 1<<20 {
		err = errors.New("ComicInfo.xml larger than 1 MB")
	}
	return data, err
}

// comicInfoSource is the file a chapter's ComicInfo would come from: the
// XML inside a chapter folder, or the archive itself.
func comicInfoSource(b *Book) string {
	if b.IsDir {
		return filepath.Join(b.Path, comicInfoName)
	}
	return b.Path
}

// readBookComicInfoRaw returns a chapter's raw ComicInfo.xml, or nil, nil if
// it has none.
func readBookComicInfoRaw(b *Book) ([]byte, error) {
	if b.IsDir {
		data, err := readSmallFile(comicInfoSource(b))
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return data, err
	}
	format, err := archiveFormat(b.Path)
	if err != nil {
		return nil, err
	}
	if format == "rar" {
		return readComicInfoRar(b.Path)
	}
	return readComicInfoZip(b.Path)
}

// readBookComicInfo returns a chapter's parsed ComicInfo, or nil, nil if it has none.
func readBookComicInfo(b *Book) (*comicInfo, error) {
	data, err := readBookComicInfoRaw(b)
	if err != nil || data == nil {
		return nil, err
	}
	return parseComicInfo(bytes.NewReader(data))
}

// ---- chapter metadata cache -------------------------------------------------

type cachedInfo struct {
	modTime time.Time
	size    int64
	info    *comicInfo // nil: chapter has no (readable) ComicInfo
}

type infoCache struct {
	mu sync.Mutex
	m  map[string]*cachedInfo
}

// chapterInfo returns a chapter's ComicInfo (or nil), re-reading it only when
// its source file changed.
func (c *Catalog) chapterInfo(b *Book) *comicInfo {
	src := comicInfoSource(b)
	var mod time.Time
	var size int64
	if fi, err := os.Stat(src); err == nil {
		mod, size = fi.ModTime(), fi.Size()
	}
	c.infos.mu.Lock()
	cached := c.infos.m[b.ID]
	c.infos.mu.Unlock()
	if cached != nil && cached.modTime.Equal(mod) && cached.size == size {
		return cached.info
	}

	var info *comicInfo
	if !mod.IsZero() {
		var err error
		if info, err = readBookComicInfo(b); err != nil {
			log.Printf("WARN: ignoring unreadable ComicInfo in %s: %v", b.Path, err)
			info = nil
		}
	}
	c.infos.mu.Lock()
	if len(c.infos.m) >= pageCacheMax {
		c.infos.m = map[string]*cachedInfo{}
	}
	c.infos.m[b.ID] = &cachedInfo{modTime: mod, size: size, info: info}
	c.infos.mu.Unlock()
	return info
}

// ---- chapter list -----------------------------------------------------------

// chapter is a book plus its ComicInfo (nil if none), displayed title and number.
type chapter struct {
	*Book
	info      *comicInfo
	title     string
	number    float64
	numberStr string
}

// chapters returns a series' chapters in filename order with their metadata.
// Numbers come from ComicInfo <Number> only if every chapter has a distinct
// numeric one; otherwise the whole series is numbered by filename order (see
// chapterNumbers). Titles are made unique (see uniqueTitles).
func (c *Catalog) chapters(s *Series) []chapter {
	out := make([]chapter, len(s.Books))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8) // read up to 8 archives at once
	for i, b := range s.Books {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, b *Book) {
			defer func() { <-sem; wg.Done() }()
			out[i] = chapter{Book: b, info: c.chapterInfo(b)}
		}(i, b)
	}
	wg.Wait()

	for i := range out {
		ch := &out[i]
		ch.title = ch.Book.Title
		if ch.info != nil && ch.info.Title != "" {
			ch.title = ch.info.Title
		}
	}
	c.notes.report(s, "numbers",
		"chapters numbered by filename order, because ComicInfo <Number> is not usable:",
		chapterNumbers(out))
	c.notes.report(s, "titles",
		"chapters sharing a title were renamed so Suwayomi keeps them apart:",
		uniqueTitles(out))
	return out
}

// chapterNumbers sets every chapter's number. ComicInfo <Number> values are
// used only if every chapter has one, each is a non-negative number, and no
// two are equal. Otherwise all chapters get their position in filename order
// (1, 2, 3, ...): mixing the two would misplace chapters (position 3 among
// chapters 101-120), and repeated numbers make the extension's order of those
// chapters arbitrary and look like one chapter to trackers.
//
// It returns log lines explaining a fallback; none when the ComicInfo
// numbers were used or when no chapter has a <Number> at all.
func chapterNumbers(out []chapter) []string {
	nums := make([]float64, len(out))
	var missing []string // no ComicInfo, no <Number>, or not a usable number
	byNum := map[float64][]string{}
	present := 0
	for i, ch := range out {
		name := filepath.Base(ch.Book.Path)
		raw := ""
		if ch.info != nil {
			raw = ch.info.Number
		}
		if raw != "" {
			present++
		}
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil || n < 0 || math.IsInf(n, 0) || math.IsNaN(n) {
			if raw == "" {
				missing = append(missing, name)
			} else {
				missing = append(missing, fmt.Sprintf("%s (%q)", name, raw))
			}
			continue
		}
		nums[i] = n
		byNum[n] = append(byNum[n], name)
	}

	var dups []string
	for n, names := range byNum {
		if len(names) > 1 {
			dups = append(dups, fmt.Sprintf("%s: %s", strconv.FormatFloat(n, 'f', -1, 64), listSome(names)))
		}
	}
	slices.SortFunc(dups, naturalCompare)

	if len(missing) == 0 && len(dups) == 0 {
		for i := range out {
			out[i].number, out[i].numberStr = nums[i], out[i].info.Number
		}
		return nil
	}
	for i := range out {
		out[i].number = float64(out[i].Book.Number)
		out[i].numberStr = strconv.Itoa(out[i].Book.Number)
	}
	if present == 0 {
		return nil // no chapter has a <Number>: filename order is simply the plan
	}
	var why []string
	if len(missing) > 0 {
		why = append(why, fmt.Sprintf("    missing or not a number in %d of %d chapters: %s", len(missing), len(out), listSome(missing)))
	}
	for _, d := range dups {
		why = append(why, "    repeated "+d)
	}
	return why
}

// listSome joins up to 5 names, noting how many more there are.
func listSome(names []string) string {
	const limit = 5
	if len(names) <= limit {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(names[:limit], ", "), len(names)-limit)
}

// seriesLog logs a per-series note once, and again only when it changes: the
// chapter list is built on every request, so logging each time would flood
// the journal.
type seriesLog struct {
	mu   sync.Mutex
	last map[string]string // series ID + kind -> lines last logged
}

func (l *seriesLog) report(s *Series, kind, header string, lines []string) {
	key, msg := s.ID+"\x00"+kind, strings.Join(lines, "\n")
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last[key] == msg {
		return
	}
	if l.last == nil {
		l.last = map[string]string{}
	}
	if msg == "" {
		delete(l.last, key) // resolved: log again if it comes back
		return
	}
	l.last[key] = msg
	log.Printf("series %q: %s\n%s", s.Name, header, msg)
}

// uniqueTitles makes chapter titles unique within a series. Suwayomi names a
// chapter's page cache and download folder after the chapter name, so with a
// name template like "{title}", chapters sharing a ComicInfo <Title> (e.g.
// "Chapter") would share one folder and all show whichever was read first.
//
// Titles used by a single chapter are kept. Each chapter in a group sharing a
// title falls back to its file/folder name without extension, then with it,
// then the shared title plus its position. Comparison ignores case and
// surrounding spaces, to be safe on case-insensitive filesystems.
//
// It returns one line per renamed chapter, for the log.
func uniqueTitles(out []chapter) []string {
	var renames []string
	key := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	count := map[string]int{}
	for _, ch := range out {
		count[key(ch.title)]++
	}
	taken := map[string]bool{} // titles already in use, kept or assigned
	for _, ch := range out {
		if count[key(ch.title)] == 1 {
			taken[key(ch.title)] = true
		}
	}
	for i := range out {
		ch := &out[i]
		if count[key(ch.title)] == 1 {
			continue
		}
		candidates := []string{
			ch.Book.Title,
			filepath.Base(ch.Book.Path),
			fmt.Sprintf("%s (%d)", ch.title, ch.Book.Number),
		}
		shared := ch.title
		ch.title = candidates[len(candidates)-1] // positions are unique: last resort
		for _, c := range candidates {
			if c != "" && !taken[key(c)] {
				ch.title = c
				break
			}
		}
		taken[key(ch.title)] = true
		renames = append(renames, fmt.Sprintf("    %s: %q -> %q", filepath.Base(ch.Book.Path), shared, ch.title))
	}
	return renames
}
