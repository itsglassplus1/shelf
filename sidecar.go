package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// The series folder (<data>/series/<series folder name>/) holds per-series
// files kept outside the read-only library:
//
//	cover.<ext>    first page of the first chapter, made on first request
//	ComicInfo.xml  copy of the latest chapter's ComicInfo.xml, made on scan
//
// Copies are made once and then used as they are, like Suwayomi does: delete
// one to have it made again. Files placed there by hand are used the same way
// (a cover or ComicInfo.xml in the library's series folder still wins).

// newSeriesRoot prepares <data>/series; "" means copies are disabled and
// everything is served straight from the library.
func newSeriesRoot(dir string) string {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("WARN: series dir %s unusable, covers and metadata won't be copied: %v", dir, err)
		return ""
	}
	return dir
}

// logUnusedSeriesFolders lists folders in <data>/series that belong to no
// current series (left behind by a renamed or removed series). They are
// never deleted: they may hold a cover or ComicInfo.xml placed by hand, and
// a series moved out of the library for a while would lose them.
func (c *Catalog) logUnusedSeriesFolders() {
	if c.seriesRoot == "" {
		return
	}
	names := map[string]bool{}
	for _, s := range c.current().series {
		names[s.Name] = true
	}
	entries, _ := os.ReadDir(c.seriesRoot)
	var unused []string
	for _, e := range entries {
		if e.IsDir() && !names[e.Name()] {
			unused = append(unused, "    "+filepath.Join(c.seriesRoot, e.Name()))
		}
	}
	if len(unused) == 0 {
		return
	}
	const limit = 20
	count, more := len(unused), ""
	if count > limit {
		more = fmt.Sprintf("\n    ... and %d more", count-limit)
		unused = unused[:limit]
	}
	log.Printf("series: %d folder(s) in %s match no series (renamed or removed?); delete them if unneeded:\n%s%s",
		count, c.seriesRoot, strings.Join(unused, "\n"), more)
}

// writeAtomic writes r to dir/name via a temp file, so readers never see a
// partial file.
func writeAtomic(dir, name string, r io.Reader) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	_, err = io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(dir, name))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// findCover returns the first cover file in dir (see coverMediaType), or "".
func findCover(dir string) string {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if !e.IsDir() && coverMediaType(p, e.Name()) != "" {
			return p
		}
	}
	return ""
}

// seriesCover returns the cover file to serve for a series: the library's
// cover.<ext>, else the copy in the series folder, else one made now from the
// first page of the first chapter. It returns "" (and no error) when copies
// are disabled or failed; the caller then serves the first page directly.
func (c *Catalog) seriesCover(s *Series) (string, error) {
	if s.Cover != "" {
		return s.Cover, nil
	}
	dir := ""
	if c.seriesRoot != "" {
		dir = filepath.Join(c.seriesRoot, s.Name)
		if p := findCover(dir); p != "" {
			return p, nil
		}
		// Concurrent requests for the same cover (a grid loading) wait for
		// the first to make it instead of each reading page 1.
		defer c.covers.lock(s.ID)()
		if p := findCover(dir); p != "" {
			return p, nil
		}
	}
	// Checked before the copies-disabled return below: the caller then
	// serves s.Books[0] directly, so it must exist.
	if len(s.Books) == 0 {
		return "", errNoPage
	}
	if dir == "" {
		return "", nil
	}
	pg, err := c.firstPage(s.Books[0])
	if err != nil {
		return "", err
	}
	defer pg.Close()
	name := "cover" + imageFileExt(pg.name, pg.mediaType)
	if err := writeAtomic(dir, name, pg); err != nil {
		log.Printf("WARN: series %q: could not save cover: %v", s.Name, err)
		return "", nil
	}
	return filepath.Join(dir, name), nil
}

// readSmallFile reads a file of at most 1 MB (ComicInfo.xml).
func readSmallFile(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err == nil && len(data) > 1<<20 {
		err = errors.New("larger than 1 MB")
	}
	return data, err
}
