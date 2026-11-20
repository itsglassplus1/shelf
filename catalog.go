package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// While shelf is in use, a background loop checks every recheckInterval for
// new or removed series and chapters, so requests never wait for it. After
// idleAfter without requests the loop stops, leaving the disk alone (a slow
// HDD can spin down); the next browse request then checks once before
// answering and starts the loop again.
const (
	recheckInterval = 5 * time.Second
	idleAfter       = 10 * time.Minute
)

type Library struct {
	ID, Name, Path string
}

type Series struct {
	ID, Name, Dir, LibraryID string
	Cover                    string    // path to cover.<ext> in the series folder, or ""
	DirMtime                 time.Time // series folder mtime when scanned
	Created, Modified        time.Time
	Books                    []*Book // natural filename order; Number = position (1-based)

	Meta       seriesMeta
	MetaSrc    string    // file the metadata came from, or the copy path to watch ("" if none)
	MetaMtime  time.Time // its mtime when read (zero: it didn't exist)
	metaCopied bool      // a ComicInfo.xml copy was made during this scan
}

// seriesMeta is what the API reports about a series. Title is the ComicInfo
// <Series> if present, else the folder name; IDs always use the folder name.
type seriesMeta struct {
	Title, Summary, Status, Publisher string
	PublishingFinished                bool // see comicInfo
	Genres, Tags                      []string
	Authors                           []authorDTO
}

type Book struct {
	ID, Title, Path string
	IsDir           bool
	Size            int64
	Modified        time.Time
	Number          int
	Series          *Series
}

type index struct {
	libs       []*Library
	series     []*Series
	seriesByID map[string]*Series
	booksByID  map[string]*Book
	catMtimes  map[string]time.Time // category dir -> mtime when scanned
}

type Catalog struct {
	cfg config

	mu  sync.RWMutex
	idx *index

	refreshMu sync.Mutex
	lastError string // last logged rescan error, to avoid repeating it

	lastRequest atomic.Int64 // UnixNano of the latest request
	watchMu     sync.Mutex
	watching    bool // the background check loop is running

	pages      pageCache
	zips       zipCache
	infos      infoCache
	notes      seriesLog // once-per-change chapter numbering/title notes
	rar        *rarCache // nil if the cache dir is unusable
	seriesRoot string    // <data>/series, "" if unusable
}

func newCatalog(cfg config) *Catalog {
	return &Catalog{
		cfg:        cfg,
		pages:      pageCache{m: map[string]*bookPages{}},
		infos:      infoCache{m: map[string]*cachedInfo{}},
		rar:        newRarCache(cfg.cacheDir, cfg.cacheMax),
		seriesRoot: newSeriesRoot(cfg.seriesDir),
	}
}

func (c *Catalog) current() *index {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.idx
}

// makeID derives a stable, URL-safe ID from its parts.
func makeID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(h.Sum(nil)[:10])
}

// noteRequest records activity. For browse requests it also makes sure the
// library is being watched: if the background loop is stopped (startup, or
// idle), it checks for changes before returning, so the answer is current,
// and starts the loop.
func (c *Catalog) noteRequest(browse bool) {
	c.lastRequest.Store(time.Now().UnixNano())
	if !browse {
		return
	}
	c.watchMu.Lock()
	if c.watching {
		c.watchMu.Unlock()
		return
	}
	c.watching = true
	c.watchMu.Unlock()
	c.checkForChanges()
	go c.watch()
}

// watch checks for changes every recheckInterval until shelf has been idle
// for idleAfter. It also closes archives that are no longer being read.
func (c *Catalog) watch() {
	t := time.NewTicker(recheckInterval)
	defer t.Stop()
	for range t.C {
		c.zips.closeIdle(false)
		idle := func() bool { return time.Since(time.Unix(0, c.lastRequest.Load())) > idleAfter }
		if idle() {
			c.watchMu.Lock()
			if idle() { // re-checked under the lock: a request may have just arrived
				c.watching = false
				c.watchMu.Unlock()
				c.zips.closeIdle(true)
				return
			}
			c.watchMu.Unlock()
		}
		c.checkForChanges()
	}
}

// checkForChanges rescans when a category or series folder changed since the
// last scan. A failed rescan (e.g. a new name collision) is logged and the
// previous index stays in use.
func (c *Catalog) checkForChanges() {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if !c.changed() && c.lastError == "" {
		return
	}
	// While a rescan error is outstanding, retry on every check, but only log
	// when the error changes or goes away.
	if err := c.refreshLocked(); err != nil {
		if msg := err.Error(); msg != c.lastError {
			log.Printf("ERROR: rescan failed, keeping previous library state:\n%v", err)
			c.lastError = msg
		}
	} else if c.lastError != "" {
		log.Printf("rescan: problem resolved, library updated")
		c.lastError = ""
	}
}

func (c *Catalog) changed() bool {
	idx := c.current()
	differs := func(p string, mt time.Time) bool {
		fi, err := os.Stat(p)
		return err != nil || !fi.ModTime().Equal(mt)
	}
	for p, mt := range idx.catMtimes {
		if differs(p, mt) {
			return true
		}
	}
	for _, s := range idx.series {
		if differs(s.Dir, s.DirMtime) || !s.metaUnchanged() {
			return true
		}
	}
	return false
}

// metaUnchanged reports whether the file the series metadata came from (or
// the copy path being watched) is unchanged: not edited, deleted or created.
// Edits don't change the series folder's mtime, so they are checked here.
func (s *Series) metaUnchanged() bool {
	if s.MetaSrc == "" {
		return true
	}
	fi, err := os.Stat(s.MetaSrc)
	if err != nil {
		return s.MetaMtime.IsZero() // still absent
	}
	return !s.MetaMtime.IsZero() && fi.ModTime().Equal(s.MetaMtime)
}

func (c *Catalog) refresh() error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	return c.refreshLocked()
}

// refreshLocked builds a complete new index and swaps it in only if it is
// valid. Series whose folder is unchanged are reused without rescanning.
func (c *Catalog) refreshLocked() error {
	var errs []error
	old := c.current()
	prev := map[string]*Series{}
	if old != nil {
		for _, s := range old.series {
			prev[s.Dir] = s
		}
	}

	idx := &index{
		seriesByID: map[string]*Series{},
		booksByID:  map[string]*Book{},
		catMtimes:  map[string]time.Time{},
	}

	// Validate categories: present, not duplicated, not nested.
	for i, a := range c.cfg.categories {
		for _, b := range c.cfg.categories[i+1:] {
			if a == b {
				errs = append(errs, fmt.Errorf("category listed twice: %s", a))
			} else if strings.HasPrefix(b+"/", a+"/") || strings.HasPrefix(a+"/", b+"/") {
				errs = append(errs, fmt.Errorf("categories are nested: %s <-> %s", a, b))
			}
		}
		if strings.Contains("/"+a+"/", "/../") {
			errs = append(errs, fmt.Errorf("category must not contain '..': %s", a))
		}
	}

	origin := map[string]string{} // series name -> dir, for collision messages
	copied := 0                   // ComicInfo.xml copies made during this scan
	for _, cat := range c.cfg.categories {
		dir := filepath.Join(c.cfg.root, cat)
		fi, err := os.Stat(dir)
		if err != nil || !fi.IsDir() {
			errs = append(errs, fmt.Errorf("category does not exist or is not a directory: %s", dir))
			continue
		}
		idx.catMtimes[dir] = fi.ModTime()
		lib := &Library{ID: makeID("library", cat), Name: cat, Path: dir}
		idx.libs = append(idx.libs, lib)

		entries, err := os.ReadDir(dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("reading %s: %w", dir, err))
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") || !isDirEntryDir(dir, e) {
				continue // loose files in a category are ignored
			}
			sdir := filepath.Join(dir, name)
			if other, dup := origin[name]; dup {
				errs = append(errs, fmt.Errorf("series name collision for %q:\n    %s\n    %s", name, other, sdir))
				continue
			}
			origin[name] = sdir

			sfi, err := os.Stat(sdir)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			var s *Series
			if p, ok := prev[sdir]; ok && p.DirMtime.Equal(sfi.ModTime()) && p.LibraryID == lib.ID && p.metaUnchanged() {
				s = p // unchanged: reuse
			} else if s, err = scanSeries(sdir, name, lib.ID, sfi.ModTime(), c.seriesRoot); err != nil {
				errs = append(errs, err)
				continue
			}
			if s.metaCopied {
				copied++
				s.metaCopied = false
			}
			idx.series = append(idx.series, s)
			idx.seriesByID[s.ID] = s
			for _, b := range s.Books {
				idx.booksByID[b.ID] = b
			}
		}
	}

	if copied > 0 {
		log.Printf("series: copied ComicInfo.xml from the latest chapter for %d series", copied)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	c.mu.Lock()
	c.idx = idx
	c.mu.Unlock()
	return nil
}

// isDirEntryDir reports whether e is a directory, following symlinks.
func isDirEntryDir(parent string, e os.DirEntry) bool {
	if e.IsDir() {
		return true
	}
	if e.Type()&os.ModeSymlink != 0 {
		fi, err := os.Stat(filepath.Join(parent, e.Name()))
		return err == nil && fi.IsDir()
	}
	return false
}

// chapterExts are the archive extensions taken as chapters. The actual
// format is detected from the file's content when it's opened.
var chapterExts = map[string]bool{
	".cbz": true, ".zip": true, ".cbr": true, ".rar": true, ".cbt": true, ".tar": true,
}

func scanSeries(dir, name, libID string, mtime time.Time, seriesRoot string) (*Series, error) {
	s := &Series{
		ID: makeID("series", name), Name: name, Dir: dir, LibraryID: libID,
		DirMtime: mtime, Created: mtime, Modified: mtime,
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	for _, e := range entries {
		fname := e.Name()
		if strings.HasPrefix(fname, ".") {
			continue
		}
		p := filepath.Join(dir, fname)
		fi, err := os.Stat(p) // follows symlinks
		if err != nil {
			log.Printf("WARN: series %q: skipping unreadable entry: %v", name, err)
			continue
		}
		ext := strings.ToLower(path.Ext(fname))
		switch {
		case fi.IsDir():
			s.Books = append(s.Books, &Book{Title: fname, Path: p, IsDir: true, Size: dirSize(p), Modified: fi.ModTime()})
		case chapterExts[ext]:
			s.Books = append(s.Books, &Book{Title: strings.TrimSuffix(fname, path.Ext(fname)), Path: p, Size: fi.Size(), Modified: fi.ModTime()})
		case imageTypes[ext] != "" && strings.EqualFold(strings.TrimSuffix(fname, path.Ext(fname)), "cover"):
			s.Cover = p
		case strings.EqualFold(fname, comicInfoName):
			s.MetaSrc = p // series-level ComicInfo.xml
		}
	}

	slices.SortFunc(s.Books, func(a, b *Book) int { return naturalCompare(filepath.Base(a.Path), filepath.Base(b.Path)) })
	for i, b := range s.Books {
		b.Number = i + 1
		b.Series = s
		b.ID = makeID("book", name, filepath.Base(b.Path))
		if b.Modified.After(s.Modified) {
			s.Modified = b.Modified
		}
		if i == 0 || b.Modified.Before(s.Created) {
			s.Created = b.Modified
		}
	}
	s.loadMeta(seriesRoot)
	return s, nil
}

// loadMeta reads series metadata, in order of preference, from:
//  1. ComicInfo.xml in the library's series folder;
//  2. the copy in <data>/series/<name>/ComicInfo.xml;
//  3. the latest chapter's ComicInfo.xml, which is then copied to (2).
//
// Unreadable files are logged and ignored.
func (s *Series) loadMeta(seriesRoot string) {
	var raw []byte
	var err error
	copyPath := ""
	if seriesRoot != "" {
		copyPath = filepath.Join(seriesRoot, s.Name, comicInfoName)
	}

	switch {
	case s.MetaSrc != "": // (1) set by scanSeries
		raw, err = readSmallFile(s.MetaSrc)
	case copyPath != "" && fileExists(copyPath): // (2)
		s.MetaSrc = copyPath
		raw, err = readSmallFile(copyPath)
	case len(s.Books) > 0: // (3)
		latest := s.Books[len(s.Books)-1]
		raw, err = readBookComicInfoRaw(latest)
		switch {
		case err != nil || raw == nil:
			s.MetaSrc = copyPath // watch for a copy being placed by hand
		case copyPath == "":
			s.MetaSrc = comicInfoSource(latest) // copies disabled: watch the chapter
		default:
			if werr := writeAtomic(filepath.Dir(copyPath), comicInfoName, bytes.NewReader(raw)); werr != nil {
				log.Printf("WARN: series %q: could not save ComicInfo.xml copy: %v", s.Name, werr)
				s.MetaSrc = comicInfoSource(latest)
			} else {
				s.MetaSrc, s.metaCopied = copyPath, true
			}
		}
	default:
		s.MetaSrc = copyPath
	}

	var info *comicInfo
	if err == nil && raw != nil {
		info, err = parseComicInfo(bytes.NewReader(raw))
	}
	if err != nil {
		log.Printf("WARN: ignoring unreadable ComicInfo for series %q (%s): %v", s.Name, s.MetaSrc, err)
		info = nil
	}
	s.MetaMtime = time.Time{}
	if fi, statErr := os.Stat(s.MetaSrc); s.MetaSrc != "" && statErr == nil {
		s.MetaMtime = fi.ModTime()
	}

	s.Meta = seriesMeta{Title: s.Name, Genres: []string{}, Tags: []string{}, Authors: []authorDTO{}}
	if info == nil {
		return
	}
	if info.Series != "" {
		s.Meta.Title = info.Series
	}
	s.Meta.Summary, s.Meta.Status, s.Meta.Publisher = info.Summary, info.Status, info.Publisher
	s.Meta.PublishingFinished = info.PublishingFinished
	s.Meta.Genres, s.Meta.Tags = info.Genres, info.Tags
	if info.Authors != nil {
		s.Meta.Authors = info.Authors
	}
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// dirSize sums the sizes of the image files in a chapter folder.
func dirSize(dir string) int64 {
	var total int64
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if imageTypes[strings.ToLower(path.Ext(e.Name()))] == "" {
			continue
		}
		if fi, err := e.Info(); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// naturalCompare orders strings case-insensitively, comparing digit runs by
// numeric value, so "Chapter 2" < "Chapter 10".
func naturalCompare(a, b string) int {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	i, j := 0, 0
	for i < len(la) && j < len(lb) {
		ca, cb := la[i], lb[j]
		if isDigit(ca) && isDigit(cb) {
			si, sj := i, j
			for i < len(la) && isDigit(la[i]) {
				i++
			}
			for j < len(lb) && isDigit(lb[j]) {
				j++
			}
			na := strings.TrimLeft(la[si:i], "0")
			nb := strings.TrimLeft(lb[sj:j], "0")
			if len(na) != len(nb) {
				return len(na) - len(nb)
			}
			if c := strings.Compare(na, nb); c != 0 {
				return c
			}
			continue
		}
		if ca != cb {
			return int(ca) - int(cb)
		}
		i++
		j++
	}
	if c := (len(la) - i) - (len(lb) - j); c != 0 {
		return c
	}
	return strings.Compare(a, b) // deterministic tie-break
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
