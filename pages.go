package main

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// imageTypes are the image types served as pages and covers, by extension
// or by content (see images.go). Other files in a chapter aren't pages.
var imageTypes = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
	".jxl":  "image/jxl",
	".avif": "image/avif",
	".heic": "image/heif", ".heif": "image/heif",
}

// A page is a plain file on disk (image folder or unpacked RAR: path is
// set), a file inside a tar archive (inTar: its data is at tarOffset), or an
// entry inside a zip archive (otherwise: zipIndex is used).
type pageEntry struct {
	name      string // file name in the folder, or entry path in the archive
	mediaType string
	path      string
	zipIndex  int
	inTar     bool
	tarOffset int64
	tarSize   int64
}

type bookPages struct {
	modTime time.Time
	size    int64
	pages   []pageEntry
}

// pageCache keeps page lists per book, invalidated when the book's file or
// folder changes. Entries are tiny; the cache is simply cleared if it grows large.
type pageCache struct {
	mu sync.Mutex
	m  map[string]*bookPages
}

const pageCacheMax = 5000

var errNoPage = errors.New("page not found")

// pagesFor returns b's pages in reading order, listing them again only when
// the file or folder changed. A RAR chapter is unpacked on first use.
func (c *Catalog) pagesFor(b *Book) ([]pageEntry, error) {
	fi, err := os.Stat(b.Path)
	if err != nil {
		return nil, err
	}
	c.pages.mu.Lock()
	cached := c.pages.m[b.ID]
	c.pages.mu.Unlock()
	if cached != nil && cached.modTime.Equal(fi.ModTime()) && cached.size == fi.Size() {
		return cached.pages, nil
	}

	var pages []pageEntry
	if b.IsDir {
		pages, err = listDirPages(b.Path)
	} else {
		var format string
		if format, err = archiveFormat(b.Path); err == nil {
			switch format {
			case "zip":
				pages, err = c.listZipPages(b)
			case "tar":
				pages, err = listTarPages(b.Path)
			default:
				pages, err = c.rar.pages(b, fi)
			}
		}
	}
	if err != nil {
		return nil, err
	}

	c.pages.mu.Lock()
	if len(c.pages.m) >= pageCacheMax {
		c.pages.m = map[string]*bookPages{}
	}
	c.pages.m[b.ID] = &bookPages{modTime: fi.ModTime(), size: fi.Size(), pages: pages}
	c.pages.mu.Unlock()
	return pages, nil
}

// hasPageList reports whether b's page list is cached (for a RAR chapter:
// it is unpacked, or was recently), so opening a page is cheap.
func (c *Catalog) hasPageList(b *Book) bool {
	c.pages.mu.Lock()
	defer c.pages.mu.Unlock()
	return c.pages.m[b.ID] != nil
}

// pageListCurrent reports whether b's cached page list was made from the
// file as it is now (fi).
func (c *Catalog) pageListCurrent(b *Book, fi os.FileInfo) bool {
	c.pages.mu.Lock()
	defer c.pages.mu.Unlock()
	cached := c.pages.m[b.ID]
	return cached != nil && cached.modTime.Equal(fi.ModTime()) && cached.size == fi.Size()
}

func (c *Catalog) forgetPages(b *Book) {
	c.pages.mu.Lock()
	delete(c.pages.m, b.ID)
	c.pages.mu.Unlock()
}

func listDirPages(dir string) ([]pageEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var pages []pageEntry
	for _, e := range entries {
		n, p := e.Name(), filepath.Join(dir, e.Name())
		if strings.HasPrefix(n, ".") || isDirEntryDir(dir, e) {
			continue
		}
		if mt := imageMediaType(n, func() []byte { return fileHead(p) }); mt != "" {
			pages = append(pages, pageEntry{name: n, mediaType: mt, path: p})
		}
	}
	slices.SortFunc(pages, func(a, b pageEntry) int { return naturalCompare(a.name, b.name) })
	return pages, nil
}

// ---- open zip archives ------------------------------------------------------

// zipCache keeps recently used .cbz archives open, so reading a chapter page
// by page doesn't reopen the file and re-read its table of contents for every
// page (noticeable on a slow disk). Archives are closed when pushed out by
// newer ones, when they change on disk, or after zipIdle without use.
type zipCache struct {
	mu sync.Mutex
	m  map[string]*openZip // by book ID
}

const (
	zipCacheMax = 8
	zipIdle     = 2 * time.Minute
)

type openZip struct {
	*zip.ReadCloser
	modTime  time.Time
	size     int64
	lastUsed time.Time
	refs     int  // pages being read from it
	dropped  bool // no longer in the cache: close when refs reaches 0
}

// acquire returns b's open archive, opening it if needed. Call release when
// done with it (and with every file opened from it).
func (zc *zipCache) acquire(b *Book) (z *openZip, release func(), err error) {
	fi, err := os.Stat(b.Path)
	if err != nil {
		return nil, nil, err
	}
	zc.mu.Lock()
	defer zc.mu.Unlock()
	if zc.m == nil {
		zc.m = map[string]*openZip{}
	}
	z = zc.m[b.ID]
	if z != nil && (!z.modTime.Equal(fi.ModTime()) || z.size != fi.Size()) {
		zc.dropLocked(b.ID) // file changed: reopen
		z = nil
	}
	if z == nil {
		r, err := zip.OpenReader(b.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", b.Path, err)
		}
		z = &openZip{ReadCloser: r, modTime: fi.ModTime(), size: fi.Size()}
		zc.m[b.ID] = z
	}
	// Mark it used before making room, so it is never the one pushed out.
	z.refs++
	z.lastUsed = time.Now()
	for len(zc.m) > zipCacheMax {
		zc.dropOldestLocked()
	}
	return z, func() {
		zc.mu.Lock()
		defer zc.mu.Unlock()
		if z.refs--; z.refs == 0 && z.dropped {
			z.Close()
		}
	}, nil
}

// forget closes b's archive (once no longer in use), e.g. after it was found
// to have changed without a visible mtime/size change.
func (zc *zipCache) forget(b *Book) {
	zc.mu.Lock()
	defer zc.mu.Unlock()
	zc.dropLocked(b.ID)
}

// closeIdle closes archives unused for zipIdle (all of them if all is set).
func (zc *zipCache) closeIdle(all bool) {
	zc.mu.Lock()
	defer zc.mu.Unlock()
	for id, z := range zc.m {
		if all || time.Since(z.lastUsed) > zipIdle {
			zc.dropLocked(id)
		}
	}
}

func (zc *zipCache) dropOldestLocked() {
	oldest := ""
	for id, z := range zc.m {
		if oldest == "" || z.lastUsed.Before(zc.m[oldest].lastUsed) {
			oldest = id
		}
	}
	zc.dropLocked(oldest)
}

func (zc *zipCache) dropLocked(id string) {
	z := zc.m[id]
	if z == nil {
		return
	}
	delete(zc.m, id)
	z.dropped = true
	if z.refs == 0 {
		z.Close()
	}
}

type closerFunc func()

func (f closerFunc) Close() error { f(); return nil }

func (c *Catalog) listZipPages(b *Book) ([]pageEntry, error) {
	r, release, err := c.zips.acquire(b)
	if err != nil {
		return nil, err
	}
	defer release()
	if isEpub(b) {
		pages, err := epubPages(&r.Reader)
		if err == nil {
			return pages, nil
		}
		log.Printf("WARN: %s: no usable EPUB package document, listing all images in name order: %v", b.Path, err)
	}
	var pages []pageEntry
	for i, f := range r.File {
		n := f.Name
		base := path.Base(n)
		if f.FileInfo().IsDir() || strings.HasPrefix(n, "__MACOSX/") || strings.HasPrefix(base, ".") {
			continue
		}
		if mt := imageMediaType(base, func() []byte { return zipHead(f) }); mt != "" {
			pages = append(pages, pageEntry{name: n, mediaType: mt, zipIndex: i})
		}
	}
	slices.SortFunc(pages, func(a, b pageEntry) int { return naturalCompare(a.name, b.name) })
	return pages, nil
}

// zipHead returns the first sniffLen bytes of a zip entry, nil if unreadable.
func zipHead(f *zip.File) []byte {
	rc, err := f.Open()
	if err != nil {
		return nil
	}
	defer rc.Close()
	return readHead(rc)
}

// openedPage is a readable page image. seeker is non-nil for plain files so
// they can be served with Range/conditional request support.
type openedPage struct {
	io.ReadCloser
	seeker    io.ReadSeeker
	size      int64
	mediaType string
	modTime   time.Time
	name      string
}

type multiCloser struct {
	io.Reader
	closers []io.Closer
}

func (m multiCloser) Close() error {
	var err error
	for _, c := range m.closers {
		err = errors.Join(err, c.Close())
	}
	return err
}

// openPage opens page n (1-based) of a book.
func (c *Catalog) openPage(b *Book, n int) (*openedPage, error) {
	for attempt := 0; attempt < 2; attempt++ {
		pages, err := c.pagesFor(b)
		if err != nil {
			return nil, err
		}
		if n < 1 || n > len(pages) {
			return nil, errNoPage
		}
		p := pages[n-1]

		if p.path != "" {
			f, err := os.Open(p.path)
			if errors.Is(err, os.ErrNotExist) && attempt == 0 {
				// e.g. an unpacked RAR chapter was evicted from the cache; relist (re-unpack) once.
				c.forgetPages(b)
				continue
			}
			if err != nil {
				return nil, err
			}
			if !b.IsDir {
				touch(filepath.Dir(p.path)) // RAR cache: mark as recently read
			}
			fi, err := f.Stat()
			if err != nil {
				f.Close()
				return nil, err
			}
			return &openedPage{ReadCloser: f, seeker: f, size: fi.Size(), mediaType: p.mediaType, modTime: fi.ModTime(), name: path.Base(p.name)}, nil
		}

		if p.inTar {
			f, err := os.Open(b.Path)
			if err != nil {
				return nil, err
			}
			fi, err := f.Stat()
			if err != nil {
				f.Close()
				return nil, err
			}
			if !c.pageListCurrent(b, fi) {
				// The archive changed since its pages were listed; relist once.
				f.Close()
				c.forgetPages(b)
				continue
			}
			sr := io.NewSectionReader(f, p.tarOffset, p.tarSize)
			return &openedPage{
				ReadCloser: multiCloser{Reader: sr, closers: []io.Closer{f}}, seeker: sr,
				size: p.tarSize, mediaType: p.mediaType, modTime: fi.ModTime(), name: path.Base(p.name),
			}, nil
		}

		r, release, err := c.zips.acquire(b)
		if err != nil {
			return nil, err
		}
		if p.zipIndex >= len(r.File) || r.File[p.zipIndex].Name != p.name {
			// Archive changed under us without a visible mtime/size change; relist once.
			release()
			c.zips.forget(b)
			c.forgetPages(b)
			continue
		}
		zf := r.File[p.zipIndex]
		rc, err := zf.Open() // files of one archive may be read concurrently
		if err != nil {
			release()
			return nil, err
		}
		return &openedPage{
			ReadCloser: multiCloser{Reader: rc, closers: []io.Closer{rc, closerFunc(release)}},
			size:       int64(zf.UncompressedSize64), mediaType: p.mediaType,
			modTime: zf.Modified, name: path.Base(p.name),
		}, nil
	}
	return nil, errNoPage
}
