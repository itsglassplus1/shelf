package main

import (
	"archive/zip"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// writeZip creates a zip at p with the given entries, in order (name, content).
func writeZip(t *testing.T, p string, entries ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for i := 0; i+1 < len(entries); i += 2 {
		w, err := zw.Create(entries[i])
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, entries[i+1])
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// readEntry reads entry i of an acquired archive.
func readEntry(z *openZip, i int) (string, error) {
	rc, err := z.File[i].Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	return string(b), err
}

func zipBooks(t *testing.T, n int) []*Book {
	dir := t.TempDir()
	var books []*Book
	for i := range n {
		p := filepath.Join(dir, fmt.Sprintf("ch%02d.cbz", i))
		writeZip(t, p, "001.png", fmt.Sprintf("chapter %d", i))
		books = append(books, &Book{ID: fmt.Sprintf("B%d", i), Path: p})
	}
	return books
}

// Regression: with the cache full, a newly opened archive was the "oldest"
// (zero last-used time) and was closed before its first read.
func TestZipCacheNewArchiveSurvivesFullCache(t *testing.T) {
	var zc zipCache
	books := zipBooks(t, zipCacheMax+1)
	for _, b := range books[:zipCacheMax] {
		_, release, err := zc.acquire(b)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	z, release, err := zc.acquire(books[zipCacheMax])
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if got, err := readEntry(z, 0); err != nil || got != fmt.Sprintf("chapter %d", zipCacheMax) {
		t.Fatalf("new archive read = %q, %v", got, err)
	}
	if len(zc.m) != zipCacheMax {
		t.Errorf("cache holds %d archives, want %d", len(zc.m), zipCacheMax)
	}
}

func TestZipCacheInUseArchiveStaysOpenUntilReleased(t *testing.T) {
	var zc zipCache
	books := zipBooks(t, zipCacheMax+2)
	held, releaseHeld, err := zc.acquire(books[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range books[1:] { // pushes books[0] out of the cache
		_, release, err := zc.acquire(b)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if _, ok := zc.m[books[0].ID]; ok {
		t.Fatal("books[0] should have been pushed out")
	}
	if got, err := readEntry(held, 0); err != nil || got != "chapter 0" {
		t.Fatalf("pushed-out archive in use: read = %q, %v", got, err)
	}
	releaseHeld()
	if _, err := readEntry(held, 0); err == nil {
		t.Error("pushed-out archive still open after release")
	}
}

func TestZipCacheReopensChangedFile(t *testing.T) {
	var zc zipCache
	b := zipBooks(t, 1)[0]
	_, release, _ := zc.acquire(b)
	release()
	writeZip(t, b.Path, "001.png", "rewritten, and longer") // new size
	z, release, err := zc.acquire(b)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if got, _ := readEntry(z, 0); got != "rewritten, and longer" {
		t.Errorf("changed archive served old content %q", got)
	}
}

func TestZipCacheCloseIdle(t *testing.T) {
	var zc zipCache
	books := zipBooks(t, 3)
	held, releaseHeld, _ := zc.acquire(books[0])
	for _, b := range books[1:] {
		_, release, _ := zc.acquire(b)
		release()
	}
	zc.closeIdle(true)
	if len(zc.m) != 0 {
		t.Errorf("closeIdle(true) left %d archives", len(zc.m))
	}
	if _, err := readEntry(held, 0); err != nil {
		t.Errorf("archive in use was closed: %v", err)
	}
	releaseHeld()
}

func TestZipCacheConcurrent(t *testing.T) {
	var zc zipCache
	books := zipBooks(t, zipCacheMax+4) // more than fit: constant churn
	var wg sync.WaitGroup
	errs := make(chan error, 1000)
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 60 {
				n := (g*7 + i) % len(books)
				z, release, err := zc.acquire(books[n])
				if err != nil {
					errs <- err
					return
				}
				if got, err := readEntry(z, 0); err != nil || got != fmt.Sprintf("chapter %d", n) {
					errs <- fmt.Errorf("book %d: read %q, %v", n, got, err)
				}
				release()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// ---- RAR ----------------------------------------------------------------------

// Made with: rar a -m3 normal.rar p02.png p01.png ComicInfo.xml
// and with -m5 -s for solid.rar (rar reorders: ComicInfo.xml, p01, p02).
// p01.png = "PAGE-ONE", p02.png = "PAGE-TWO"; ComicInfo has Title "Rar Title".
const (
	rarNormal = `UmFyIRoHAQAzkrXlCgEFBgAFAQGAgADjjVTBJQIDC4gABIgApIMCktlCeoAAAQdwMDIucG5nCgMT
cNX/aj85IxNQQUdFLVRXT/c+X20lAgMLiAAEiACkgwIF1eQRgAABB3AwMS5wbmcKAxNw1f9qPzkj
E1BBR0UtT05F1AenyisCAwvVAATZAKSDAoHLu/WAAwENQ29taWNJbmZvLnhtbAoDE3DV/2o/OSMT
wMhSJGVDL3YEOlWg3o8fG8UEHi8FaBVNQ0GMDWQG/0dCSBKe+9+D6qs74GccpsXvcnGPfLyGhqh+
FgnsIqYtpMapDoyz2WyQthOgZRdu3Fd8/sFSgB13VlEDBQQA`
	rarSolid = `UmFyIRoHAQAgtvoRCgEFBgQFAQGAgAADBfBvKwIDC9cABNkApIMCgcu79YAdAQ1Db21pY0luZm8u
eG1sCgMTcNX/aj85IxPGyFQkZUMvdgQ6VaBR6PHTQRIo8ZEWA9lTUNBjAjIDf6MCKDTvvvfg9VWd
8DOOU2FbWHrdxs0QxJxlyNh9Pg4go+eCzfuQ4ZumkMaPYATBmkcGnt5iH7q50KZgJQIDC4kABIgA
pIMCBdXkEcAdAQdwMDEucG5nCgMTcNX/aj85IxNDHwa+q1E6koDoCsPoJQIDC4cABIgApIMCktlC
esAdAQdwMDIucG5nCgMTcNX/aj85IxNAHgSSryUAHXdWUQMFBAA=`
)

func writeRar(t *testing.T, dir, name, b64 string) string {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(b64, "\n", ""))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

var rarFixtures = map[string]string{"normal": rarNormal, "solid": rarSolid}

func TestArchiveFormat(t *testing.T) {
	dir := t.TempDir()
	r := writeRar(t, dir, "x.cbz", rarNormal) // misnamed on purpose
	z := filepath.Join(dir, "y.cbr")
	writeZip(t, z, "a.png", "x")
	junk := filepath.Join(dir, "z.cbz")
	os.WriteFile(junk, []byte("not an archive"), 0o644)
	for p, want := range map[string]string{r: "rar", z: "zip"} {
		if got, err := archiveFormat(p); err != nil || got != want {
			t.Errorf("archiveFormat(%s) = %q, %v; want %q", filepath.Base(p), got, err, want)
		}
	}
	if _, err := archiveFormat(junk); err == nil {
		t.Error("junk file: expected an error")
	}
}

func TestRarFirstPage(t *testing.T) {
	for kind, b64 := range rarFixtures {
		t.Run(kind, func(t *testing.T) {
			p := writeRar(t, t.TempDir(), "ch.cbr", b64)
			name, mt, data, err := rarFirstPage(p)
			if err != nil {
				t.Fatal(err)
			}
			if name != "p01.png" || mt != "image/png" || string(data) != "PAGE-ONE" {
				t.Errorf("got %q %q %q; want p01.png image/png PAGE-ONE", name, mt, data)
			}
		})
	}
}

func TestReadComicInfoRar(t *testing.T) {
	for kind, b64 := range rarFixtures {
		t.Run(kind, func(t *testing.T) {
			p := writeRar(t, t.TempDir(), "ch.cbr", b64)
			raw, err := readComicInfoRar(p)
			if err != nil {
				t.Fatal(err)
			}
			ci, err := parseComicInfo(strings.NewReader(string(raw)))
			if err != nil || ci.Title != "Rar Title" || ci.Number != "7" {
				t.Errorf("parsed %+v, %v", ci, err)
			}
		})
	}
}

func TestRarCacheUnpack(t *testing.T) {
	for kind, b64 := range rarFixtures {
		t.Run(kind, func(t *testing.T) {
			lib, cache := t.TempDir(), t.TempDir()
			p := writeRar(t, lib, "ch.cbr", b64)
			rc := newRarCache(cache, 1<<20)
			b := &Book{ID: "BOOK", Path: p}
			fi, _ := os.Stat(p)
			pages, err := rc.pages(b, fi)
			if err != nil {
				t.Fatal(err)
			}
			if len(pages) != 2 || pages[0].name != "p01.png" || pages[1].name != "p02.png" {
				t.Fatalf("pages %+v, want p01.png, p02.png in natural order", pages)
			}
			for i, want := range []string{"PAGE-ONE", "PAGE-TWO"} {
				if got, _ := os.ReadFile(pages[i].path); string(got) != want {
					t.Errorf("page %d = %q, want %q", i+1, got, want)
				}
			}
			again, err := rc.pages(b, fi) // from the manifest, same folder
			if err != nil || again[0].path != pages[0].path {
				t.Errorf("second call: %v, %v", again, err)
			}
			if entries, _ := os.ReadDir(cache); len(entries) != 1 {
				t.Errorf("cache has %d entries, want 1 (no leftover temp folders)", len(entries))
			}

			// A damaged entry (manifest gone, folder still there) is unpacked again.
			os.Remove(filepath.Join(filepath.Dir(pages[0].path), manifestName))
			if again, err := rc.pages(b, fi); err != nil || len(again) != 2 {
				t.Errorf("after losing the manifest: %v, %v", again, err)
			}
		})
	}

	// Entries from another cache version (or another version of the archive)
	// are replaced, not reused.
	{
		lib, cache := t.TempDir(), t.TempDir()
		p0 := writeRar(t, lib, "ch.cbr", rarNormal)
		fi0, _ := os.Stat(p0)
		// Named as a shelf without cache versions named it, for this very
		// archive, with an empty page list: reusing it would give no pages.
		stale := filepath.Join(cache, fmt.Sprintf("BOOK-%x-%x", fi0.ModTime().UnixNano(), fi0.Size()))
		os.MkdirAll(stale, 0o755)
		os.WriteFile(filepath.Join(stale, manifestName), []byte(`[]`), 0o644)
		pages, err := newRarCache(cache, 1<<20).pages(&Book{ID: "BOOK", Path: p0}, fi0)
		if err != nil || len(pages) != 2 {
			t.Errorf("with a stale entry: %v, %v", pages, err)
		}
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Errorf("stale entry not removed: %v", err)
		}
	}

	// An archive that unpacks to more than the cache cap is refused.
	p := writeRar(t, t.TempDir(), "big.cbr", rarNormal)
	fi, _ := os.Stat(p)
	cache := t.TempDir()
	if _, err := newRarCache(cache, 10).pages(&Book{ID: "BIG", Path: p}, fi); err == nil {
		t.Error("over-cap archive: expected an error")
	}
	if entries, _ := os.ReadDir(cache); len(entries) != 0 {
		t.Errorf("failed unpack left %d entries in the cache", len(entries))
	}
}

func TestKeyedMutex(t *testing.T) {
	var k keyedMutex
	var mu sync.Mutex
	inside := map[string]int{}
	var wg sync.WaitGroup
	for g := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := []string{"a", "b"}[g%2]
			unlock := k.lock(key)
			mu.Lock()
			inside[key]++
			if inside[key] > 1 {
				t.Errorf("two holders of key %q", key)
			}
			mu.Unlock()
			mu.Lock()
			inside[key]--
			mu.Unlock()
			unlock()
		}()
	}
	wg.Wait()
	if len(k.m) != 0 {
		t.Errorf("%d unused mutexes left in the map", len(k.m))
	}
}
