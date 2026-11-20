package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
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
	files := []string{"a.cbz", "b.cbz", "c.cbz"}
	none := []string{"-", "-", "-"}
	for _, tc := range []struct {
		name     string
		numbers  []string
		wantNums []float64
		wantStrs []string
		wantLog  string // substring of the log lines; "" means no log
	}{
		{"all valid and distinct: ComicInfo kept, decimals too",
			[]string{"101", "102", "102.5"}, []float64{101, 102, 102.5}, []string{"101", "102", "102.5"}, ""},
		{"none have a number: filename order, no log",
			none, []float64{1, 2, 3}, []string{"1", "2", "3"}, ""},
		{"one missing: whole series falls back",
			[]string{"101", "-", "103"}, []float64{1, 2, 3}, []string{"1", "2", "3"}, "missing or not a number in 1 of 3 chapters: b.cbz"},
		{"repeated: whole series falls back",
			[]string{"1", "1", "1"}, []float64{1, 2, 3}, []string{"1", "2", "3"}, "repeated 1: a.cbz, b.cbz, c.cbz"},
		{"non-numeric",
			[]string{"1", "Oneshot", "3"}, []float64{1, 2, 3}, []string{"1", "2", "3"}, `b.cbz ("Oneshot")`},
		{"negative and NaN are not usable",
			[]string{"-1", "NaN", "3"}, []float64{1, 2, 3}, []string{"1", "2", "3"}, "2 of 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chs := testChapters(files, none, tc.numbers)
			why := strings.Join(chapterNumbers(chs), "\n")
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
	wantAuthors := []authorDTO{{"Jane", "writer"}, {"John", "writer"}, {"Ann", "penciller"}} // duplicate "jane" dropped
	if !slices.Equal(ci.Authors, wantAuthors) {
		t.Errorf("authors %v, want %v", ci.Authors, wantAuthors)
	}
	if ci.ReleaseDate != "2024-02-01" { // 31 February falls back to the 1st
		t.Errorf("release date %q", ci.ReleaseDate)
	}

	if _, err := parseComicInfo(strings.NewReader(`<?xml version="1.0" encoding="shift_jis"?><ComicInfo/>`)); err == nil {
		t.Error("non-UTF-8 encoding: expected an error")
	}
	if _, err := parseComicInfo(strings.NewReader(`<ComicInfo><Title>broken`)); err == nil {
		t.Error("truncated XML: expected an error")
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
		"Ongoing": "ONGOING", "Completed": "ENDED", "Publishing finished": "ENDED",
		"Cancelled": "ABANDONED", "On hiatus": "HIATUS", "Unknown": "", "": "",
	} {
		if got := komgaStatus(in); got != want {
			t.Errorf("komgaStatus(%q) = %q, want %q", in, got, want)
		}
	}
}
