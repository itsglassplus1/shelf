package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testLibrary builds a library in a temp dir:
//
//	comics/Zip Series/c1..c3.cbz   ComicInfo: shared title "Chapter", no <Number>
//	comics/Rar Series/ch1.cbr      the normal RAR fixture (page 1 = PAGE-ONE)
//	comics/Folder Series/001/      a folder chapter: p1.png, p2.png
//	comics/Empty/                  no chapters
//	manga/Other/v1.cbz             numbered 5 in ComicInfo
func testLibrary(t *testing.T) (*Catalog, *httptest.Server) {
	t.Helper()
	log.SetOutput(io.Discard) // keep test output readable
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	root, data := t.TempDir(), t.TempDir()
	comics := filepath.Join(root, "comics")
	info := `<ComicInfo><Series>Zip Series Title</Series><Title>Chapter</Title>` +
		`<ty:PublishingStatusTachiyomi xmlns:ty="x">Ongoing</ty:PublishingStatusTachiyomi></ComicInfo>`
	for i := 1; i <= 3; i++ {
		writeZip(t, filepath.Join(comics, "Zip Series", fmt.Sprintf("c%d.cbz", i)),
			"001.png", fmt.Sprintf("zip %d page 1", i), "002.png", fmt.Sprintf("zip %d page 2", i), "ComicInfo.xml", info)
	}
	writeRar(t, filepath.Join(comics, "Rar Series"), "ch1.cbr", rarNormal)
	folder := filepath.Join(comics, "Folder Series", "001")
	os.MkdirAll(folder, 0o755)
	os.WriteFile(filepath.Join(folder, "p2.png"), []byte("folder page 2"), 0o644)
	os.WriteFile(filepath.Join(folder, "p1.png"), []byte("folder page 1"), 0o644)
	os.MkdirAll(filepath.Join(comics, "Empty"), 0o755)
	writeZip(t, filepath.Join(root, "manga", "Other", "v1.cbz"),
		"a.png", "other", "ComicInfo.xml", `<ComicInfo><Number>5</Number></ComicInfo>`)

	cfg := config{
		root: root, categories: []string{"comics", "manga"},
		cacheDir: filepath.Join(data, "cache"), seriesDir: filepath.Join(data, "series"), cacheMax: 1 << 20,
	}
	cat := newCatalog(cfg)
	if err := cat.refresh(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newServer(cat, nil))
	t.Cleanup(srv.Close)
	return cat, srv
}

func fetch(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func fetchJSON(t *testing.T, method, url, body string, v any) {
	t.Helper()
	code, b := fetch(t, method, url, body)
	if code != 200 {
		t.Fatalf("%s %s: status %d: %s", method, url, code, b)
	}
	if err := json.Unmarshal([]byte(b), v); err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
}

// seriesByName lists all series and returns them by folder name.
func seriesByName(t *testing.T, base string) map[string]seriesDTO {
	var page pageWrapper[seriesDTO]
	fetchJSON(t, "POST", base+"/api/v1/series/list?size=100", `{}`, &page)
	out := map[string]seriesDTO{}
	for _, s := range page.Content {
		out[s.Name] = s
	}
	return out
}

func booksOf(t *testing.T, base, seriesID string) []bookDTO {
	var page pageWrapper[bookDTO]
	fetchJSON(t, "GET", base+"/api/v1/series/"+seriesID+"/books?unpaged=true", "", &page)
	return page.Content
}

func TestServerBrowse(t *testing.T) {
	_, srv := testLibrary(t)
	base := srv.URL

	var libs []libraryDTO
	fetchJSON(t, "GET", base+"/api/v1/libraries", "", &libs)
	if len(libs) != 2 {
		t.Fatalf("libraries = %+v", libs)
	}

	all := seriesByName(t, base)
	if len(all) != 5 {
		t.Fatalf("series = %d, want 5", len(all))
	}
	zs := all["Zip Series"]
	if zs.Metadata.Title != "Zip Series Title" || zs.Metadata.Status != "ONGOING" || zs.BooksCount != 3 {
		t.Errorf("Zip Series metadata = %+v (want title from ComicInfo, status ONGOING, 3 books)", zs.Metadata)
	}
	if all["Empty"].Metadata.Status != "UNKNOWN" {
		t.Errorf("series without metadata: status %q, want UNKNOWN", all["Empty"].Metadata.Status)
	}

	// Search by title, and filter by library.
	var page pageWrapper[seriesDTO]
	fetchJSON(t, "POST", base+"/api/v1/series/list", `{"fullTextSearch":"zip series"}`, &page)
	if page.TotalElements != 1 || page.Content[0].Name != "Zip Series" {
		t.Errorf("search: %+v", page.Content)
	}
	cond := fmt.Sprintf(`{"condition":{"libraryId":{"operator":"is","value":%q}}}`, all["Other"].LibraryID)
	fetchJSON(t, "POST", base+"/api/v1/series/list", cond, &page)
	if page.TotalElements != 1 || page.Content[0].Name != "Other" {
		t.Errorf("library filter: %+v", page.Content)
	}
	fetchJSON(t, "POST", base+"/api/v1/series/list?page=9223372036854775807", `{}`, &page)
	if len(page.Content) != 0 {
		t.Errorf("huge page number: %d results", len(page.Content))
	}

	// Shared ComicInfo titles become file names; no <Number>: numbers from
	// the file names. Newest first.
	books := booksOf(t, base, zs.ID)
	if len(books) != 3 {
		t.Fatalf("Zip Series: %d books, want 3", len(books))
	}
	for i, b := range books {
		num := 3 - i
		wantTitle, wantNum := fmt.Sprintf("c%d", num), fmt.Sprint(num)
		if b.Metadata.Title != wantTitle || b.Metadata.Number != wantNum || b.Metadata.NumberSort != float64(num) {
			t.Errorf("book %d: title %q number %q/%v; want %q %s", i, b.Metadata.Title, b.Metadata.Number, b.Metadata.NumberSort, wantTitle, wantNum)
		}
	}
	other := booksOf(t, base, all["Other"].ID)
	if len(other) != 1 || other[0].Metadata.Number != "5" {
		t.Errorf("Other: %+v (want ComicInfo number 5)", other)
	}
	var one bookDTO
	fetchJSON(t, "GET", base+"/api/v1/books/"+books[1].ID, "", &one)
	if one.ID != books[1].ID || one.Metadata.Title != "c2" {
		t.Errorf("book detail = %+v", one.Metadata)
	}

	// Values for the filter menus, and endpoints this server doesn't have.
	var authors []authorDTO
	fetchJSON(t, "GET", base+"/api/v1/authors", "", &authors)
	for _, ep := range []string{"/api/v1/collections", "/api/v1/readlists"} {
		var empty pageWrapper[struct{}]
		fetchJSON(t, "GET", base+ep, "", &empty)
		if !empty.Empty {
			t.Errorf("%s not empty", ep)
		}
	}
	if code, _ := fetch(t, "GET", base+"/api/v1/nope", ""); code != 404 {
		t.Errorf("unknown route: %d", code)
	}
	if code, _ := fetch(t, "GET", base+"/api/v1/series/NOPE", ""); code != 404 {
		t.Errorf("unknown series: %d", code)
	}
}

func TestServerPages(t *testing.T) {
	_, srv := testLibrary(t)
	base := srv.URL
	all := seriesByName(t, base)

	for _, tc := range []struct {
		series string
		pages  []string
	}{
		{"Zip Series", []string{"zip 1 page 1", "zip 1 page 2"}},
		{"Folder Series", []string{"folder page 1", "folder page 2"}}, // natural order, not directory order
		{"Rar Series", []string{"PAGE-ONE", "PAGE-TWO"}},              // natural order, not archive order
	} {
		books := booksOf(t, base, all[tc.series].ID)
		b := books[len(books)-1] // the first chapter: books are listed newest first
		var list []pageDTO
		fetchJSON(t, "GET", base+"/api/v1/books/"+b.ID+"/pages", "", &list)
		if len(list) != len(tc.pages) {
			t.Fatalf("%s: %d pages listed, want %d", tc.series, len(list), len(tc.pages))
		}
		for i, want := range tc.pages {
			code, got := fetch(t, "GET", fmt.Sprintf("%s/api/v1/books/%s/pages/%d", base, b.ID, i+1), "")
			if code != 200 || got != want {
				t.Errorf("%s page %d: %d %q, want %q", tc.series, i+1, code, got, want)
			}
		}
		for _, n := range []string{"0", "99", "x"} {
			if code, _ := fetch(t, "GET", base+"/api/v1/books/"+b.ID+"/pages/"+n, ""); code != 404 {
				t.Errorf("%s page %s: %d, want 404", tc.series, n, code)
			}
		}
	}
}

func TestServerCovers(t *testing.T) {
	cat, srv := testLibrary(t)
	base := srv.URL
	all := seriesByName(t, base)

	// A RAR cover reads only page 1: nothing is unpacked into the cache.
	code, got := fetch(t, "GET", base+"/api/v1/series/"+all["Rar Series"].ID+"/thumbnail", "")
	if code != 200 || got != "PAGE-ONE" {
		t.Errorf("RAR cover: %d %q", code, got)
	}
	if entries, _ := os.ReadDir(cat.cfg.cacheDir); len(entries) != 0 {
		t.Errorf("RAR cover unpacked %d chapter(s) into the cache", len(entries))
	}
	if _, err := os.Stat(filepath.Join(cat.seriesRoot, "Rar Series", "cover.png")); err != nil {
		t.Errorf("cover copy not saved: %v", err)
	}
	if code, got := fetch(t, "GET", base+"/api/v1/series/"+all["Zip Series"].ID+"/thumbnail", ""); code != 200 || got != "zip 1 page 1" {
		t.Errorf("zip cover: %d %q", code, got)
	}

	// Empty series: 404, with copies enabled and disabled (regression: panic).
	empty := all["Empty"].ID
	if code, _ := fetch(t, "GET", base+"/api/v1/series/"+empty+"/thumbnail", ""); code != 404 {
		t.Errorf("empty series cover: %d", code)
	}
	cat.seriesRoot = "" // copies disabled
	if code, _ := fetch(t, "GET", base+"/api/v1/series/"+empty+"/thumbnail", ""); code != 404 {
		t.Errorf("empty series cover, copies disabled: %d", code)
	}
	if code, got := fetch(t, "GET", base+"/api/v1/series/"+all["Folder Series"].ID+"/thumbnail", ""); code != 200 || got != "folder page 1" {
		t.Errorf("cover with copies disabled: %d %q", code, got)
	}
}

func TestStartupRejectsBadLibrary(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "a", "Dup"), 0o755)
	os.MkdirAll(filepath.Join(root, "b", "Dup"), 0o755)
	data := t.TempDir()
	for _, cats := range [][]string{
		{"a", "b"},       // series name collision
		{"a", "missing"}, // missing category
		{"a", "a"},       // listed twice
		{"a", "a/Dup"},   // nested
	} {
		cat := newCatalog(config{root: root, categories: cats,
			cacheDir: filepath.Join(data, "cache"), seriesDir: filepath.Join(data, "series"), cacheMax: 1 << 20})
		if err := cat.refresh(); err == nil {
			t.Errorf("categories %q: expected the scan to fail", cats)
		}
	}
}

func TestTrackerEndpointsQuiet(t *testing.T) {
	_, srv := testLibrary(t)
	var logs bytes.Buffer
	log.SetOutput(&logs)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/v2/series/X/read-progress/tachiyomi"},
		{"PUT", "/api/v2/series/X/read-progress/tachiyomi"},
		{"GET", "/api/v1/readlists/X/read-progress/tachiyomi"},
	} {
		if code, _ := fetch(t, tc.method, srv.URL+tc.path, `{"lastBookNumberSortRead":3}`); code != 404 {
			t.Errorf("%s %s: %d, want 404", tc.method, tc.path, code)
		}
	}
	if logs.Len() != 0 {
		t.Errorf("tracker requests logged: %q", logs.String())
	}
	fetch(t, "GET", srv.URL+"/api/v1/something-new", "")
	if !strings.Contains(logs.String(), "unhandled request GET /api/v1/something-new") {
		t.Errorf("unknown request not logged: %q", logs.String())
	}
}
