package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"
)

// ---- ordering ----------------------------------------------------------------

func TestNaturalCompare(t *testing.T) {
	// Each list is in the expected order.
	for _, want := range [][]string{
		{"Chapter 1", "Chapter 2", "Chapter 10", "Chapter 100"},
		{"ch1", "ch01.5", "ch2"}, // leading zeros don't matter: 01 == 1, then ".5" sorts after end
		{"a", "B", "c"},          // case-insensitive
		{"x9", "x10", "y1"},
		{"Vol 2 Ch 9", "Vol 2 Ch 10", "Vol 10 Ch 1"},
		{"c3", "c3.cbz"}, // prefix first
	} {
		got := slices.Clone(want)
		slices.Reverse(got)
		slices.SortFunc(got, naturalCompare)
		if !slices.Equal(got, want) {
			t.Errorf("sorted %q, want %q", got, want)
		}
	}
	// Strings equal apart from case or zero padding still compare unequal
	// (deterministic tie-break), and the order is consistent both ways.
	for _, p := range [][2]string{{"A", "a"}, {"ch01", "ch1"}} {
		ab, ba := naturalCompare(p[0], p[1]), naturalCompare(p[1], p[0])
		if ab == 0 || (ab < 0) == (ba < 0) {
			t.Errorf("naturalCompare(%q, %q) = %d, reverse %d: want nonzero and opposite", p[0], p[1], ab, ba)
		}
	}
}

// ---- chapter numbering and titles ---------------------------------------------

// testChapters builds chapters for files with the given ComicInfo titles and
// numbers ("-" means no ComicInfo <Number>; nil info when both are "-").
func testChapters(files []string, titles, numbers []string) []chapter {
	out := make([]chapter, len(files))
	for i, f := range files {
		b := &Book{Title: strings.TrimSuffix(f, ".cbz"), Path: "/lib/S/" + f, Number: i + 1}
		var info *comicInfo
		if titles[i] != "-" || numbers[i] != "-" {
			info = &comicInfo{}
			if titles[i] != "-" {
				info.Title = titles[i]
			}
			if numbers[i] != "-" {
				info.Number = numbers[i]
			}
		}
		out[i] = chapter{Book: b, info: info, title: b.Title}
		if info != nil && info.Title != "" {
			out[i].title = info.Title
		}
	}
	return out
}

func numbersOf(chs []chapter) (nums []float64, strs []string) {
	for _, ch := range chs {
		nums = append(nums, ch.number)
		strs = append(strs, ch.numberStr)
	}
	return
}

func TestChapterNumbers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		files    []string
		titles   []string // ComicInfo <Title>, "-" for none
		numbers  []string // ComicInfo <Number>, "-" for none
		wantNums []float64
		wantStrs []string
		wantLog  string // substring of the log line; "" means no log
	}{
		{"ComicInfo numbers, decimals too",
			[]string{"a.cbz", "b.cbz", "c.cbz"}, []string{"-", "-", "-"}, []string{"101", "102", "102.5"},
			[]float64{101, 102, 102.5}, []string{"101", "102", "102.5"}, ""},
		{"file names, as Mihon reads them",
			[]string{"Vol.1 Ch.1.cbz", "Vol.1 Ch.2.cbz", "Vol.1 Ch.2.5.cbz", "Vol.2 Ch.3 Part 2.cbz"},
			[]string{"-", "-", "-", "-"}, []string{"-", "-", "-", "-"},
			[]float64{1, 2, 2.5, 3.2}, []string{"1", "2", "2.5", "3.2"}, ""}, // the float is kept, as in Mihon
		{"ComicInfo number per chapter, file names for the rest",
			[]string{"c1.cbz", "c2.cbz", "c3.cbz"}, []string{"-", "-", "-"}, []string{"-", "50", "-"},
			[]float64{1, 50, 3}, []string{"1", "50", "3"}, ""},
		{"ComicInfo number that isn't one: file name",
			[]string{"c7.cbz", "c8.cbz"}, []string{"-", "-"}, []string{"Oneshot", "NaN"},
			[]float64{7, 8}, []string{"7", "8"}, ""},
		{"no number in the file name: read from the title with the whole list",
			[]string{"a.cbz", "b.cbz", "c.cbz"}, []string{"Chapter 1", "Side Story", "Chapter 3"}, []string{"-", "-", "-"},
			[]float64{1, 1.5, 3}, []string{"1", "1.5", "3"}, ""},
		{"numbers shared by every file name are left out",
			[]string{"Group 2 Chapter 385.cbz", "Group 2 Chapter 386.cbz", "Group 2 Chapter 387.cbz"},
			[]string{"-", "-", "-"}, []string{"-", "-", "-"},
			[]float64{385, 386, 387}, []string{"385", "386", "387"}, ""},
		{"a year in every file name is left out",
			[]string{"Series (2017) #001.cbz", "Series (2017) #002.cbz", "Series (2017) #003.cbz"},
			[]string{"-", "-", "-"}, []string{"-", "-", "-"},
			[]float64{1, 2, 3}, []string{"1", "2", "3"}, ""},
		{"file name numbers win over title numbers",
			[]string{"c1.cbz", "c2.cbz"}, []string{"Chapter 10", "Chapter 20"}, []string{"-", "-"},
			[]float64{1, 2}, []string{"1", "2"}, ""},
		{"title number, not a neighbour fill from file names",
			[]string{"c1.cbz", "extra.cbz", "c3.cbz"}, []string{"-", "Chapter 2.5", "-"}, []string{"-", "-", "-"},
			[]float64{1, 2.5, 3}, []string{"1", "2.5", "3"}, ""},
		{"no number anywhere: -2, logged",
			[]string{"Bar.cbz", "Foo.cbz"}, []string{"-", "-"}, []string{"-", "-"},
			[]float64{-2, -2}, []string{"", ""}, "2 of 2 chapters: Bar.cbz, Foo.cbz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chs := testChapters(tc.files, tc.titles, tc.numbers)
			why := strings.Join(chapterNumbers("Series", chs), "\n")
			nums, strs := numbersOf(chs)
			if !slices.Equal(nums, tc.wantNums) || !slices.Equal(strs, tc.wantStrs) {
				t.Errorf("numbers %v %q, want %v %q", nums, strs, tc.wantNums, tc.wantStrs)
			}
			if tc.wantLog == "" && why != "" {
				t.Errorf("unexpected log: %s", why)
			}
			if tc.wantLog != "" && !strings.Contains(why, tc.wantLog) {
				t.Errorf("log %q does not mention %q", why, tc.wantLog)
			}
		})
	}
}

func TestListSome(t *testing.T) {
	if got := listSome([]string{"a", "b"}); got != "a, b" {
		t.Errorf("got %q", got)
	}
	if got := listSome([]string{"1", "2", "3", "4", "5", "6", "7"}); got != "1, 2, 3, 4, 5, and 2 more" {
		t.Errorf("got %q", got)
	}
}

func titlesOf(chs []chapter) (out []string) {
	for _, ch := range chs {
		out = append(out, ch.title)
	}
	return
}

func TestUniqueTitles(t *testing.T) {
	for _, tc := range []struct {
		name   string
		files  []string
		titles []string
		want   []string
		logs   int
	}{
		{"unique titles kept",
			[]string{"a.cbz", "b.cbz"}, []string{"Intro", "Outro"}, []string{"Intro", "Outro"}, 0},
		{"shared title: file names",
			[]string{"c1.cbz", "c2.cbz", "c3.cbz"}, []string{"Chapter", "Chapter", "Chapter"}, []string{"c1", "c2", "c3"}, 3},
		{"case and spaces count as the same title",
			[]string{"a.cbz", "b.cbz"}, []string{"Chapter", " chapter "}, []string{"a", "b"}, 2},
		{"file name already used by another title: falls back to name with extension",
			[]string{"c1.cbz", "c2.cbz", "c3.cbz"}, []string{"c2", "Chapter", "Chapter"}, []string{"c2", "c2.cbz", "c3"}, 2},
		{"no ComicInfo: file names, already unique",
			[]string{"a.cbz", "b.cbz"}, []string{"-", "-"}, []string{"a", "b"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noNumbers := make([]string, len(tc.files))
			for i := range noNumbers {
				noNumbers[i] = "-"
			}
			chs := testChapters(tc.files, tc.titles, noNumbers)
			logs := uniqueTitles(chs)
			got := titlesOf(chs)
			if !slices.Equal(got, tc.want) {
				t.Errorf("titles %q, want %q", got, tc.want)
			}
			if len(logs) != tc.logs {
				t.Errorf("%d log lines, want %d: %q", len(logs), tc.logs, logs)
			}
		})
	}

	// Folder "c3" and archive "c3.cbz" share a base name; both share a title
	// with a third chapter, so the last resort (title + position) is needed.
	chs := []chapter{
		{Book: &Book{Title: "c3", Path: "/lib/S/c3", Number: 1}, title: "X"},
		{Book: &Book{Title: "c3", Path: "/lib/S/c3.cbz", Number: 2}, title: "X"},
		{Book: &Book{Title: "c3.cbz", Path: "/lib/S/c3.cbz.cbz", Number: 3}, title: "X"},
	}
	uniqueTitles(chs)
	got := titlesOf(chs)
	seen := map[string]bool{}
	for _, title := range got {
		if seen[strings.ToLower(title)] {
			t.Fatalf("titles not unique: %q", got)
		}
		seen[strings.ToLower(title)] = true
	}
}

// ---- paging -------------------------------------------------------------------

func TestPaginate(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}
	for _, tc := range []struct {
		page, size int
		content    []int
		last       bool
		totalPages int
	}{
		{0, 2, []int{1, 2}, false, 3},
		{2, 2, []int{5}, true, 3},
		{3, 2, []int{}, true, 3},                   // past the end: empty
		{0, 0, []int{1, 2, 3, 4, 5}, true, 1},      // unpaged
		{-4, 2, []int{1, 2}, false, 3},             // negative page treated as 0
		{1 << 62, 500, []int{}, true, 1},           // overflow regression: no panic
		{9223372036854775807, 2, []int{}, true, 3}, // max int
	} {
		got := paginate(items, tc.page, tc.size)
		if !slices.Equal(got.Content, tc.content) || got.Last != tc.last || got.TotalPages != tc.totalPages ||
			got.TotalElements != 5 || got.NumberOfElements != len(tc.content) || got.Empty != (len(tc.content) == 0) {
			t.Errorf("paginate(page %d, size %d) = %+v", tc.page, tc.size, got)
		}
	}
	if got := paginate([]int{}, 0, 0); got.Content == nil || !got.Empty || got.TotalPages != 0 {
		t.Errorf("empty unpaged = %+v (content must be [] not null)", got)
	}
}

// ---- search -------------------------------------------------------------------

func TestSearchTerms(t *testing.T) {
	if got := unescapeLucene(`Re\:Zero \(2nd\) a\\b`); got != `Re:Zero (2nd) a\b` {
		t.Errorf("unescapeLucene = %q", got)
	}
	name := "One Punch-Man\none-punch-man"
	for terms, want := range map[string]bool{
		"":              true,
		"punch":         true,
		"PUNCH man":     true,
		"punch and man": true, // lowercased Lucene operators are ignored
		"punch zebra":   false,
		"one-punch-man": true, // folder name is searched too
	} {
		if got := matchesTerms(name, strings.Fields(strings.ToLower(terms))); got != want {
			t.Errorf("matchesTerms(%q) = %v, want %v", terms, got, want)
		}
	}
}

func TestEvalCondition(t *testing.T) {
	s := &Series{LibraryID: "LIB1", Meta: seriesMeta{
		Genres: []string{"Action", "Drama"}, Tags: []string{"Isekai"}, Publisher: "Pub", Status: "ONGOING",
		Authors: []authorDTO{{Name: "Jane", Role: "writer"}},
	}}
	for cond, want := range map[string]bool{
		``:     true,
		`null`: true,
		`{"libraryId":{"operator":"is","value":"LIB1"}}`:                                                                     true,
		`{"libraryId":{"operator":"is","value":"LIB2"}}`:                                                                     false,
		`{"libraryId":{"operator":"isNot","value":"LIB2"}}`:                                                                  true,
		`{"genre":{"operator":"is","value":"action"}}`:                                                                       true, // case-insensitive
		`{"tag":{"operator":"is","value":"Romance"}}`:                                                                        false,
		`{"publisher":{"operator":"is","value":"pub"}}`:                                                                      true,
		`{"seriesStatus":{"operator":"is","value":"ENDED"}}`:                                                                 false,
		`{"author":{"operator":"is","value":{"name":"jane","role":"writer"}}}`:                                               true,
		`{"author":{"operator":"is","value":{"name":"Jane","role":"penciller"}}}`:                                            false,
		`{"collectionId":{"operator":"is","value":"X"}}`:                                                                     false, // no collections
		`{"readStatus":{"operator":"is","value":"READ"}}`:                                                                    true,  // unknown: ignored
		`{"allOf":[{"genre":{"operator":"is","value":"Action"}},{"tag":{"operator":"is","value":"Isekai"}}]}`:                true,
		`{"allOf":[{"genre":{"operator":"is","value":"Action"}},{"tag":{"operator":"is","value":"Nope"}}]}`:                  false,
		`{"anyOf":[{"genre":{"operator":"is","value":"Nope"}},{"genre":{"operator":"is","value":"Drama"}}]}`:                 true,
		`{"anyOf":[{"genre":{"operator":"is","value":"Nope"}},{"genre":{"operator":"is","value":"Nada"}}]}`:                  false,
		`{"allOf":[{"libraryId":{"operator":"is","value":"LIB1"}},{"anyOf":[{"genre":{"operator":"is","value":"Drama"}}]}]}`: true,
	} {
		if got := evalCondition(json.RawMessage(cond), s); got != want {
			t.Errorf("evalCondition(%s) = %v, want %v", cond, got, want)
		}
	}
}

// ---- ComicInfo ----------------------------------------------------------------

func TestParseComicInfo(t *testing.T) {
	xml := `<?xml version="1.0" encoding="utf-8"?>
<ComicInfo xmlns:ty="http://www.w3.org/2001/XMLSchema">
  <Title> Chapter 5 </Title><Series>Some Series</Series><Number>5</Number>
  <Writer>Jane, John, jane</Writer><Penciller>Ann</Penciller><Translator>Group A, Group B</Translator>
  <Genre>Action, , Drama</Genre><Tags>One</Tags><Publisher>Pub</Publisher>
  <ty:Categories>Reading, one</ty:Categories>
  <Year>2024</Year><Month>2</Month><Day>31</Day>
  <ty:PublishingStatusTachiyomi>Completed</ty:PublishingStatusTachiyomi>
</ComicInfo>`
	ci, err := parseComicInfo(strings.NewReader(xml))
	if err != nil {
		t.Fatal(err)
	}
	if ci.Title != "Chapter 5" || ci.Series != "Some Series" || ci.Number != "5" || ci.Publisher != "Pub" {
		t.Errorf("basic fields: %+v", ci)
	}
	if ci.Status != "ENDED" {
		t.Errorf("namespaced status: got %q, want ENDED", ci.Status)
	}
	if !slices.Equal(ci.Genres, []string{"Action", "Drama"}) || !slices.Equal(ci.Translators, []string{"Group A", "Group B"}) {
		t.Errorf("lists: genres %q translators %q", ci.Genres, ci.Translators)
	}
	if !slices.Equal(ci.Tags, []string{"One", "Reading"}) { // categories join the tags, "one" only once
		t.Errorf("tags %q, want One, Reading", ci.Tags)
	}
	wantAuthors := []authorDTO{{"Jane", "writer"}, {"John", "writer"}, {"Ann", "penciller"}} // duplicate "jane" dropped
	if !slices.Equal(ci.Authors, wantAuthors) {
		t.Errorf("authors %v, want %v", ci.Authors, wantAuthors)
	}
	if ci.ReleaseDate != "2024-02-01" { // 31 February falls back to the 1st
		t.Errorf("release date %q", ci.ReleaseDate)
	}

	// Read as UTF-8 whatever the declaration says, like Mihon; real UTF-16
	// (with a byte order mark) is converted.
	utf16 := func(s string, big bool) string {
		out := []byte{0xff, 0xfe}
		if big {
			out = []byte{0xfe, 0xff}
		}
		for _, u := range utf16.Encode([]rune(s)) {
			if big {
				out = append(out, byte(u>>8), byte(u))
			} else {
				out = append(out, byte(u), byte(u>>8))
			}
		}
		return string(out)
	}
	doc := `<?xml version="1.0" encoding="utf-16"?><ComicInfo><Title>Ünïcode 話</Title></ComicInfo>`
	for name, data := range map[string]string{
		"utf-16 declared, UTF-8 file":     doc,
		"shift_jis declared, ASCII file":  `<?xml version="1.0" encoding="shift_jis"?><ComicInfo><Title>Ünïcode 話</Title></ComicInfo>`,
		"UTF-16 little-endian with a BOM": utf16(doc, false),
		"UTF-16 big-endian with a BOM":    utf16(doc, true),
	} {
		if ci, err := parseComicInfo(strings.NewReader(data)); err != nil || ci.Title != "Ünïcode 話" {
			t.Errorf("%s: %+v, %v", name, ci, err)
		}
	}
	if _, err := parseComicInfo(strings.NewReader("<ComicInfo><Title>caf\xe9</Title></ComicInfo>")); err == nil {
		t.Error("Latin-1 text (not UTF-8): expected an error")
	}
	if _, err := parseComicInfo(strings.NewReader(`<ComicInfo><Title>broken`)); err == nil {
		t.Error("truncated XML: expected an error")
	}
}

func TestDisplayAuthors(t *testing.T) {
	in := []authorDTO{
		{"W", "writer"}, {"P", "penciller"}, {"I", "inker"}, {"p", "colorist"},
		{"L", "letterer"}, {"C", "cover"}, {"I", "cover"}, {"E", "editor"},
	}
	got := displayAuthors(in)
	var artists []string
	for _, a := range got {
		if a.Role == "penciller" {
			artists = append(artists, a.Name)
		}
	}
	// Mihon's order, each name once (case-insensitive); editors aren't artists.
	if want := []string{"P", "I", "L", "C"}; !slices.Equal(artists, want) {
		t.Errorf("artists %q, want %q", artists, want)
	}
	if !slices.Equal(got[:len(in)], in) {
		t.Errorf("original authors changed: %v", got[:len(in)])
	}
	if got := displayAuthors([]authorDTO{}); got == nil {
		t.Error("empty authors: got nil, want [] (sent as JSON)")
	}
}

func TestSeriesDTOStatus(t *testing.T) {
	s := &Series{Books: []*Book{{}, {}}, Meta: seriesMeta{Status: "ENDED", PublishingFinished: true, Authors: []authorDTO{}}}
	// The extension shows Publishing finished for ENDED when booksCount < totalBookCount.
	if d := toSeriesDTO(s); d.Metadata.TotalBookCount == nil || d.BooksCount >= *d.Metadata.TotalBookCount {
		t.Errorf("publishing finished: booksCount %d, totalBookCount %v", d.BooksCount, d.Metadata.TotalBookCount)
	}
	s.Meta.PublishingFinished = false // Completed
	if d := toSeriesDTO(s); d.Metadata.TotalBookCount != nil {
		t.Errorf("completed: totalBookCount %d, want null", *d.Metadata.TotalBookCount)
	}
}

func TestReleaseDateAndStatus(t *testing.T) {
	for _, tc := range [][4]string{
		{"2020", "5", "17", "2020-05-17"},
		{"2020", "", "", "2020-01-01"},
		{"2020", "13", "40", "2020-01-01"},
		{"", "5", "17", ""},
		{"abc", "1", "1", ""},
	} {
		if got := releaseDate(tc[0], tc[1], tc[2]); got != tc[3] {
			t.Errorf("releaseDate(%q,%q,%q) = %q, want %q", tc[0], tc[1], tc[2], got, tc[3])
		}
	}
	for in, want := range map[string]string{
		"Ongoing": "ONGOING", "Completed": "ENDED", "Publishing finished": "ENDED+",
		"Cancelled": "ABANDONED", "On hiatus": "HIATUS", "Licensed": "", "Unknown": "", "": "",
	} {
		got, finished := komgaStatus(in)
		if finished {
			got += "+"
		}
		if got != want {
			t.Errorf("komgaStatus(%q) = %q, want %q (+: publishing finished)", in, got, want)
		}
	}
}

func TestSystemFoldersIgnored(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	root, data := t.TempDir(), t.TempDir()
	for _, cat := range []string{"a", "b"} {
		// In every category: would collide (shelf refusing to start) or show as series.
		for _, sys := range []string{"@eaDir", "#recycle", "LOST+FOUND"} {
			os.MkdirAll(filepath.Join(root, cat, sys), 0o755)
			os.WriteFile(filepath.Join(root, cat, sys, "thumb.jpg"), []byte("x"), 0o644)
		}
	}
	writeZip(t, filepath.Join(root, "a", "S", "c1.cbz"), "p1.png", "page")
	os.MkdirAll(filepath.Join(root, "a", "S", "@eaDir", "c1.cbz"), 0o755) // Synology's thumbnails of c1.cbz
	os.WriteFile(filepath.Join(root, "a", "S", "@eaDir", "c1.cbz", "SYNOFILE_THUMB_M.jpg"), []byte("x"), 0o644)

	cat := newCatalog(config{root: root, categories: []string{"a", "b"},
		cacheDir: filepath.Join(data, "cache"), seriesDir: filepath.Join(data, "series"), cacheMax: 1 << 20})
	if err := cat.refresh(); err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	idx := cat.current()
	if len(idx.series) != 1 || idx.series[0].Name != "S" {
		t.Fatalf("series %d, want only S", len(idx.series))
	}
	if books := idx.series[0].Books; len(books) != 1 || books[0].Title != "c1" {
		t.Errorf("S has %d chapters, want only c1", len(books))
	}
}

func TestConcurrentCoverRequests(t *testing.T) {
	cat, _ := testLibrary(t)
	var zs *Series
	for _, s := range cat.current().series {
		if s.Name == "Zip Series" {
			zs = s
		}
	}
	var wg sync.WaitGroup
	paths := make([]string, 16)
	for i := range paths {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := cat.seriesCover(zs)
			if err != nil {
				t.Error(err)
			}
			paths[i] = p
		}(i)
	}
	wg.Wait()
	for _, p := range paths {
		if p != paths[0] || p == "" {
			t.Fatalf("covers %q, want one path for all", paths)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(paths[0]))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Contains(names, "cover.png") || slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(n, ".tmp-") }) {
		t.Errorf("series folder: %q, want cover.png and no leftovers", names)
	}
}

func TestChapterInfoCacheKeepsWholeLibrary(t *testing.T) {
	cat, _ := testLibrary(t)
	var zs *Series
	for _, s := range cat.current().series {
		if s.Name == "Zip Series" {
			zs = s
		}
	}
	// A library bigger than the old 5000-entry cap: nothing is thrown away.
	cat.infos.mu.Lock()
	for i := 0; i < 6000; i++ {
		cat.infos.m[fmt.Sprintf("other-%d", i)] = &cachedInfo{}
	}
	cat.infos.mu.Unlock()
	cat.chapters(zs)
	cat.infos.mu.Lock()
	n := len(cat.infos.m)
	cat.infos.mu.Unlock()
	if n != 6000+len(zs.Books) {
		t.Fatalf("%d cache entries, want %d", n, 6000+len(zs.Books))
	}

	// A rescan forgets chapters no longer in the library.
	gone := zs.Books[0]
	os.Remove(gone.Path)
	if err := cat.refresh(); err != nil {
		t.Fatal(err)
	}
	cat.infos.mu.Lock()
	defer cat.infos.mu.Unlock()
	if cat.infos.m[gone.ID] != nil || cat.infos.m["other-1"] != nil {
		t.Errorf("removed chapter or unknown IDs still cached")
	}
	if cat.infos.m[zs.Books[1].ID] == nil {
		t.Errorf("remaining chapter forgotten")
	}
}
