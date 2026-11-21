package main

import (
	"bytes"
	"encoding/json"
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

	"github.com/nwaples/rardecode/v2"
)

// RAR archives can't jump to a page: reaching page N means decoding
// everything before it. So the first time a RAR chapter is opened, its images
// are unpacked into a disk cache and served from there as plain files.
// The cache is capped in size; the least recently read chapters are evicted.
//
// Covers are the exception: rarFirstPage reads just the first page, so
// browsing a grid of RAR series doesn't unpack a chapter per series.

// rarCacheVersion is part of every unpacked chapter's folder name. Raise it
// when unpacking changes (which entries are pages, their order, the
// manifest), so chapters unpacked by an older shelf are unpacked again; the
// old folders are removed as they are replaced, or evicted.
const rarCacheVersion = 1

const (
	manifestName  = "manifest.json"
	rarMaxDictMem = 256 << 20 // refuse archives needing a larger decode window
	evictGrace    = 10 * time.Minute
	maxCoverBytes = 64 << 20 // refuse absurdly large first pages
)

// rarDecodes limits sequential RAR decoding (unpacking a chapter, or reading
// one file from a solid archive) to 2 at once: each may hold a large decode
// window in memory and reads the whole archive, which a slow disk feels.
var rarDecodes = make(chan struct{}, 2)

func decodeSlot() (release func()) {
	rarDecodes <- struct{}{}
	return func() { <-rarDecodes }
}

type rarCache struct {
	dir     string
	max     int64
	chapter keyedMutex // one extraction per chapter; different chapters run in parallel
	evictMu sync.Mutex // one eviction scan at a time
}

type manifestEntry struct {
	Name      string `json:"name"` // entry path inside the archive
	MediaType string `json:"mediaType"`
	File      string `json:"file"` // file name in the cache folder
}

// newRarCache prepares the cache folder. If it can't be used, RAR chapters
// fail with an error but everything else keeps working.
func newRarCache(dir string, max int64) *rarCache {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("WARN: cache dir %s unusable, RAR chapters disabled: %v", dir, err)
		return nil
	}
	// Remove half-finished extractions left by a crash.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	return &rarCache{dir: dir, max: max}
}

// keyedMutex hands out one mutex per key, dropping it when unused.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*refMutex
}

type refMutex struct {
	sync.Mutex
	refs int
}

// lock locks key and returns its unlock function.
func (k *keyedMutex) lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*refMutex{}
	}
	m := k.m[key]
	if m == nil {
		m = &refMutex{}
		k.m[key] = m
	}
	m.refs++
	k.mu.Unlock()

	m.Lock()
	return func() {
		m.Unlock()
		k.mu.Lock()
		if m.refs--; m.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}

// archiveFormat identifies an archive ("zip", "rar" or "tar") by its first
// bytes, not its extension (misnamed .cbr/.cbz files are common).
func archiveFormat(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, 512) // a tar header block
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	switch {
	case strings.HasPrefix(string(head), "PK\x03\x04"), strings.HasPrefix(string(head), "PK\x05\x06"):
		return "zip", nil
	case strings.HasPrefix(string(head), "Rar!\x1a\x07"):
		return "rar", nil
	case isTarHeader(head):
		return "tar", nil
	}
	if c := compressedFormat(head); c != "" {
		return "", fmt.Errorf("%s: %s-compressed, not a zip, rar or plain tar archive (unpack a compressed .cbt to a plain tar)", file, c)
	}
	return "", fmt.Errorf("%s: not a zip, rar or tar archive", file)
}

// rarCandidate reports whether an archive entry may be a page, and its
// normalized name: a file that isn't hidden, isn't macOS junk, and may be an
// image by its name (see imageMediaType; whether it is may need its
// content). Shared by extraction and rarFirstPage so both agree on which
// entry is page 1.
func rarCandidate(h *rardecode.FileHeader) (name string, ok bool) {
	name = strings.ReplaceAll(h.Name, "\\", "/")
	base := path.Base(name)
	if h.IsDir || strings.HasPrefix(name, "__MACOSX/") || strings.HasPrefix(base, ".") ||
		notImageExts[strings.ToLower(path.Ext(base))] {
		return "", false
	}
	return name, true
}

// pages returns the page list of a RAR chapter, unpacking it on first use.
func (rc *rarCache) pages(b *Book, fi os.FileInfo) ([]pageEntry, error) {
	if rc == nil {
		return nil, errors.New("RAR chapters need a writable cache dir (cache/ inside SHELF_DATA_DIR)")
	}
	// The key includes mtime and size, so a changed archive is unpacked
	// afresh, and the cache version, so a shelf that unpacks differently does too.
	key := fmt.Sprintf("%s-v%d-%x-%x", b.ID, rarCacheVersion, fi.ModTime().UnixNano(), fi.Size())
	dir := filepath.Join(rc.dir, key)

	// Locked per chapter (not per version), so cleaning up older versions
	// below can't race with another request for the same chapter.
	defer rc.chapter.lock(b.ID)()

	if pages, err := readManifest(dir); err == nil {
		touch(dir)
		return pages, nil
	}
	// Unpack again. A folder without a readable manifest (damaged, or being
	// evicted) would block moving the new one into place: remove it first.
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}

	release := decodeSlot()
	start := time.Now()
	err := rc.extract(b.Path, dir)
	release()
	if err != nil {
		return nil, err
	}
	pages, err := readManifest(dir)
	if err != nil {
		return nil, err
	}
	log.Printf("cache: unpacked %s (%d pages) in %s", b.Path, len(pages), time.Since(start).Round(time.Millisecond))

	// Drop older unpacked versions of this chapter, then enforce the size cap.
	old, _ := filepath.Glob(filepath.Join(rc.dir, b.ID+"-*"))
	for _, o := range old {
		if o != dir {
			os.RemoveAll(o)
		}
	}
	rc.evict(dir)
	return pages, nil
}

func (rc *rarCache) extract(archive, dest string) (err error) {
	tmp, err := os.MkdirTemp(rc.dir, ".tmp-")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(tmp)
		}
	}()

	r, err := rardecode.OpenReader(archive, rardecode.MaxDictionarySize(rarMaxDictMem))
	if err != nil {
		return fmt.Errorf("%s: %w", archive, err)
	}
	defer r.Close()

	var entries []manifestEntry
	var total int64
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
		name, ok := rarCandidate(h)
		if !ok {
			continue // skipped entries are decoded past by Next()
		}
		var head []byte // read from r if the name doesn't tell
		mt := imageMediaType(name, func() []byte { head = readHead(r); return head })
		if mt == "" {
			continue
		}
		file := fmt.Sprintf("%05d%s", len(entries), imageFileExt(name, mt))
		out, err := os.Create(filepath.Join(tmp, file))
		if err != nil {
			return err
		}
		// Guard against archives that unpack to more than the whole cache.
		src := io.MultiReader(bytes.NewReader(head), r)
		n, err := io.Copy(out, io.LimitReader(src, rc.max-total+1))
		out.Close()
		if err != nil {
			return fmt.Errorf("%s: %s: %w", archive, name, err)
		}
		if total += n; total > rc.max {
			return fmt.Errorf("%s: unpacks to more than the cache limit (%d MB)", archive, rc.max>>20)
		}
		entries = append(entries, manifestEntry{Name: name, MediaType: mt, File: file})
	}

	slices.SortFunc(entries, func(a, b manifestEntry) int { return naturalCompare(a.Name, b.Name) })
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, manifestName), data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dest) // appears complete or not at all
}

// rarFirstPage reads page 1 of a RAR chapter (first image in natural order)
// without unpacking the rest. Listing the headers is cheap; in a normal
// archive the entry is then opened directly. In a solid archive entries can
// only be decoded in order, so it decodes up to page 1 and stops; that is
// usually the first entry anyway.
func rarFirstPage(archive string) (name, mediaType string, data []byte, err error) {
	opt := rardecode.MaxDictionarySize(rarMaxDictMem)
	files, err := rardecode.List(archive, opt)
	if err != nil {
		return "", "", nil, fmt.Errorf("%s: %w", archive, err)
	}
	type candidate struct {
		f    *rardecode.File
		name string
	}
	var candidates []candidate
	for _, f := range files {
		if n, ok := rarCandidate(&f.FileHeader); ok {
			candidates = append(candidates, candidate{f, n})
		}
	}
	slices.SortStableFunc(candidates, func(a, b candidate) int { return naturalCompare(a.name, b.name) })
	// Page 1 is the first image in natural order. Files the name doesn't
	// tell about are read to see (rare: most pages have image extensions).
	for _, c := range candidates {
		if mt := imageTypes[strings.ToLower(path.Ext(c.name))]; mt != "" {
			data, err = readRarFile(archive, c.f, maxCoverBytes)
			if err != nil {
				return "", "", nil, err
			}
			return c.name, mt, data, nil
		}
		data, err = readRarFile(archive, c.f, maxCoverBytes)
		if errors.Is(err, errTooLarge) {
			continue // too large for a cover, image or not
		}
		if err != nil {
			return "", "", nil, err
		}
		if mt := sniffImage(data); mt != "" {
			return c.name, mt, data, nil
		}
	}
	return "", "", nil, errNoPage
}

var errTooLarge = errors.New("too large")

// readRarFile reads one file listed by rardecode.List, refusing more than
// limit bytes. In a normal archive the file is opened directly. In a solid
// archive files can only be decoded in order, so everything before it is
// decoded too (in a decode slot, as that can mean most of the archive).
func readRarFile(archive string, f *rardecode.File, limit int64) ([]byte, error) {
	var src io.Reader
	if !f.Solid {
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", archive, f.Name, err)
		}
		defer rc.Close()
		src = rc
	} else {
		defer decodeSlot()()
		r, err := rardecode.OpenReader(archive, rardecode.MaxDictionarySize(rarMaxDictMem))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", archive, err)
		}
		defer r.Close()
		for {
			h, err := r.Next()
			if err != nil { // io.EOF too: the listed file must be there
				return nil, fmt.Errorf("%s: looking for %s: %w", archive, f.Name, err)
			}
			if h.Name == f.Name && !h.IsDir {
				break
			}
		}
		src = r
	}
	data, err := io.ReadAll(io.LimitReader(src, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = fmt.Errorf("%w: larger than %d MB", errTooLarge, limit>>20)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %w", archive, f.Name, err)
	}
	return data, nil
}

// readComicInfoRar returns the raw ComicInfo.xml of a RAR chapter, preferring
// one at the root, like readComicInfoZip. Returns nil, nil if it has none.
func readComicInfoRar(archive string) ([]byte, error) {
	files, err := rardecode.List(archive, rardecode.MaxDictionarySize(rarMaxDictMem))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", archive, err)
	}
	var found *rardecode.File
	for _, f := range files {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if !f.IsDir && strings.EqualFold(path.Base(name), comicInfoName) && !strings.HasPrefix(name, "__MACOSX/") {
			if found == nil || !strings.Contains(name, "/") {
				found = f
			}
		}
	}
	if found == nil {
		return nil, nil
	}
	return readRarFile(archive, found, 1<<20)
}

// firstPage opens page 1 of a chapter, for covers and thumbnails. A RAR
// chapter that isn't already unpacked is read with rarFirstPage instead of
// being unpacked whole.
func (c *Catalog) firstPage(b *Book) (*openedPage, error) {
	if b.IsDir || c.hasPageList(b) {
		return c.openPage(b, 1)
	}
	if format, err := archiveFormat(b.Path); err != nil || format != "rar" {
		return c.openPage(b, 1) // zip or tar (or an error openPage will report)
	}
	fi, err := os.Stat(b.Path)
	if err != nil {
		return nil, err
	}
	name, mt, data, err := rarFirstPage(b.Path)
	if err != nil {
		return nil, err
	}
	r := bytes.NewReader(data)
	return &openedPage{
		ReadCloser: io.NopCloser(r), seeker: r, size: int64(len(data)),
		mediaType: mt, modTime: fi.ModTime(), name: path.Base(name),
	}, nil
}

func readManifest(dir string) ([]pageEntry, error) {
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, err
	}
	var entries []manifestEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	pages := make([]pageEntry, len(entries))
	for i, e := range entries {
		pages[i] = pageEntry{name: e.Name, mediaType: e.MediaType, path: filepath.Join(dir, e.File)}
	}
	return pages, nil
}

// touch marks an unpacked chapter as recently read (eviction is by folder mtime).
func touch(dir string) {
	now := time.Now()
	os.Chtimes(dir, now, now)
}

// evict deletes the least recently read chapters until the cache fits its
// cap. keep (the chapter just unpacked) and chapters read within evictGrace
// are never deleted.
func (rc *rarCache) evict(keep string) {
	rc.evictMu.Lock()
	defer rc.evictMu.Unlock()
	type item struct {
		dir   string
		size  int64
		mtime time.Time
	}
	var items []item
	var total int64
	entries, _ := os.ReadDir(rc.dir)
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".tmp-") {
			continue
		}
		d := filepath.Join(rc.dir, e.Name())
		fi, err := e.Info()
		if err != nil {
			continue
		}
		var size int64
		files, _ := os.ReadDir(d)
		for _, f := range files {
			if info, err := f.Info(); err == nil {
				size += info.Size()
			}
		}
		items = append(items, item{d, size, fi.ModTime()})
		total += size
	}
	slices.SortFunc(items, func(a, b item) int { return a.mtime.Compare(b.mtime) })
	for _, it := range items {
		if total <= rc.max {
			break
		}
		// Never evict what was read recently: it may still be in use. The cap
		// is therefore soft; it is enforced again by the next extraction.
		// (A chapter being unpacked right now is in a .tmp- folder, skipped
		// above, and is renamed into place with a fresh mtime.)
		if it.dir == keep || time.Since(it.mtime) < evictGrace {
			continue
		}
		if err := os.RemoveAll(it.dir); err == nil {
			total -= it.size
		}
	}
}
