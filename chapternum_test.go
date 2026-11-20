package main

import (
	"fmt"
	"slices"
	"testing"
)

// Ported from Mihon's ChapterRecognitionTest
// (domain/src/test/java/tachiyomi/domain/chapter/service/ChapterRecognitionTest.kt).

type nameNumbering struct {
	name string
	want chapterNumbering
}

// n builds an expectation: volume and chapter, "" for none.
func n(name, volume, chapter string) nameNumbering {
	return nameNumbering{name, chapterNumbering{volume, chapter}}
}

// assertNumbers checks the numbers recognized for chapters with the given
// names, listed from the most to the least recent.
func assertNumbers(t *testing.T, seriesTitle string, expected ...nameNumbering) {
	t.Helper()
	chapters := make([]numberedChapter, len(expected))
	want := make([]chapterNumbering, len(expected))
	for i, e := range expected {
		chapters[i] = numberedChapter{name: e.name, number: -1}
		want[i] = e.want
	}
	if got := parseChapterNumbers(seriesTitle, chapters); !slices.Equal(got, want) {
		t.Errorf("%q:\n got  %v\n want %v", names(expected), got, want)
	}
}

func names(e []nameNumbering) (out []string) {
	for _, x := range e {
		out = append(out, x.name)
	}
	return
}

func TestChapterRecognitionSingleNames(t *testing.T) {
	for _, tc := range []struct {
		title string
		n     nameNumbering
	}{
		// Basic Ch prefix
		{"Mokushiroku Alice", n("Mokushiroku Alice Vol.1 Ch.4: Misrepresentation", "1", "4")},
		{"Mokushiroku Alice", n("Mokushiroku Alice Vol. 1 Ch. 4: Misrepresentation", "1", "4")},
		{"Mokushiroku Alice", n("Mokushiroku Alice Vol.1 Ch.4.1: Misrepresentation", "1", "4.1")},
		{"Mokushiroku Alice", n("Mokushiroku Alice Vol.1 Ch.4.4: Misrepresentation", "1", "4.4")},
		{"Mokushiroku Alice", n("Mokushiroku Alice Vol.1 Ch.4.a: Misrepresentation", "1", "4a")},
		{"Mokushiroku Alice", n("Mokushiroku Alice Vol.1 Ch.4.b: Misrepresentation", "1", "4b")},
		{"Mokushiroku Alice", n("Mokushiroku Alice Vol.1 Ch.4.extra: Misrepresentation", "1", "4.99")},
		// Name containing one number
		{"Bleach", n("Bleach 567 Down With Snowwhite", "", "567")},
		{"Bleach", n("Bleach 567.1 Down With Snowwhite", "", "567.1")},
		{"Bleach", n("Bleach 567.4 Down With Snowwhite", "", "567.4")},
		{"Bleach", n("Bleach 567.a Down With Snowwhite", "", "567a")},
		{"Bleach", n("Bleach 567.b Down With Snowwhite", "", "567b")},
		{"Bleach", n("Bleach 567.extra Down With Snowwhite", "", "567.99")},
		// Chapter containing manga title and number
		{"Solanin", n("Solanin 028 Vol. 2", "2", "28")},
		{"Solanin", n("Solanin 028.1 Vol. 2", "2", "28.1")},
		{"Solanin", n("Solanin 028.4 Vol. 2", "2", "28.4")},
		{"Solanin", n("Solanin 028.a Vol. 2", "2", "28a")},
		{"Solanin", n("Solanin 028.b Vol. 2", "2", "28b")},
		{"Solanin", n("Solanin 028.extra Vol. 2", "2", "28.99")},
		// Extreme case
		{"Onepunch-Man", n("Onepunch-Man Punch Ver002 028", "", "28")},
		{"Onepunch-Man", n("Onepunch-Man Punch Ver002 028.1", "", "28.1")},
		{"Onepunch-Man", n("Onepunch-Man Punch Ver002 028.4", "", "28.4")},
		{"Onepunch-Man", n("Onepunch-Man Punch Ver002 028.a", "", "28a")},
		{"Onepunch-Man", n("Onepunch-Man Punch Ver002 028.b", "", "28b")},
		{"Onepunch-Man", n("Onepunch-Man Punch Ver002 028.extra", "", "28.99")},
		// Chapter containing dot v2
		{"random", n("Vol.1 Ch.5v.2: Alones", "1", "5")},
		// Number in manga title
		{"Ayame 14", n("Ayame 14 1 - The summer of 14", "", "1")},
		// Space between ch x
		{"Mokushiroku Alice", n("Mokushiroku Alice Vol.1 Ch. 4: Misrepresentation", "1", "4")},
		// Chapter title with ch substring
		{"Ayame 14", n("Vol.1 Ch.1: March 25 (First Day Cohabiting)", "1", "1")},
		// Chapter containing multiple zeros
		{"random", n("Vol.001 Ch.003: Kaguya Doesn't Know Much", "1", "3")},
		// Chapter with version before number
		{"Onepunch-Man", n("Onepunch-Man Punch Ver002 086 : Creeping Darkness [3]", "", "86")},
		// Version attached to chapter number
		{"Ansatsu Kyoushitsu", n("Ansatsu Kyoushitsu 011v002: Assembly Time", "", "11")},
		// Number after manga title with chapter in chapter title case
		{"Tokyo ESP", n("Tokyo ESP 027: Part 002: Chapter 001", "", "27b")},
		// Number after unwanted tag
		{"One-punch Man", n("Mag Version 195.5", "", "195.5")},
		// Unparseable chapter
		{"random", n("Foo", "", "")},
		// Chapter with time in title
		{"random", n("Fairy Tail 404: 00:00", "", "404")},
		// Chapter with alpha without dot
		{"random", n("Asu No Yoichi 19a", "", "19a")},
		// Chapter title containing extra/omake/special and vol
		{"Fairy Tail", n("Fairy Tail 404.extravol002", "", "404.99")},
		{"Fairy Tail", n("Fairy Tail 404 extravol002", "", "404.99")},
		{"Fairy Tail", n("Fairy Tail 404.omakevol002", "", "404.98")},
		{"Fairy Tail", n("Fairy Tail 404 omakevol002", "", "404.98")},
		{"Fairy Tail", n("Fairy Tail 404.specialvol002", "", "404.97")},
		{"Fairy Tail", n("Fairy Tail 404 specialvol002", "", "404.97")},
		// Chapter title containing commas
		{"One Piece", n("One Piece 300,a", "", "300a")},
		{"One Piece", n("One Piece Ch,123,extra", "", "123.99")},
		{"One Piece", n("One Piece the sunny, goes swimming 024,005", "", "24.005")},
		// Chapter title containing hyphens
		{"Solo Leveling", n("ch 122-a", "", "122a")},
		{"Solo Leveling", n("Solo Leveling Ch.123-extra", "", "123.99")},
		{"Solo Leveling", n("Solo Leveling, 024-005", "", "24.005")},
		{"Solo Leveling", n("Ch.191-200 Read Online", "", "191.2")},
		// Chapters containing season
		{"D.I.C.E", n("D.I.C.E[Season 001] Ep. 007", "1", "7")},
		// Chapters in format sx - chapter xx
		{"The Gamer", n("S3 - Chapter 20", "3", "20")},
		// Chapters ending with s
		{"One Outs", n("One Outs 001", "", "1")},
		// Chapters containing ordinals
		{"The Sister of the Woods with a Thousand Young", n("The 1st Night", "", "1")},
		{"The Sister of the Woods with a Thousand Young", n("The 2nd Night", "", "2")},
		{"The Sister of the Woods with a Thousand Young", n("The 3rd Night", "", "3")},
		{"The Sister of the Woods with a Thousand Young", n("The 4th Night", "", "4")},
		// Parts are letters
		{"", n("Chapter 1.5 Part I", "", "1.5a")},
		{"", n("Chapter 1.5 Part II", "", "1.5b")},
		{"", n("Chapter 12 Part 3", "", "12c")},
		{"", n("Vol.2 Chapter 7 - Part IV", "2", "7d")},
		{"", n("Part 3", "", "3")},
		// Volume is not taken as the chapter number
		{"Mokushiroku Alice", n("Mokushiroku Alice Vol.3", "3", "")},
		{"Mokushiroku Alice", n("Vol.3 12", "3", "12")},
		// Seasons are volumes
		{"", n("Vol-3 Chapter 2", "3", "2")},
		{"", n("Mob's 100", "", "100")},
	} {
		assertNumbers(t, tc.title, tc.n)
	}
}

func TestChapterNumberToFloat(t *testing.T) {
	for in, want := range map[string]float64{
		"12": 12, "10.5": 10.5, "19a": 19.1, "19i": 19.9, "1.5a": 1.5, "-2": -2,
	} {
		if got := chapterNumberToFloat(in); got != want {
			t.Errorf("chapterNumberToFloat(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestChapterRecognitionLists(t *testing.T) {
	for i, tc := range [][]nameNumbering{
		// Volumes are only taken from the name
		{n("Vol.2 Ch.5", "2", "5"), n("Ch.4", "", "4"), n("Vol.2 Ch.3", "2", "3")},
		// Numbers every similar name shares are ignored
		{n("Group 2 Chapter 387", "", "387"), n("Group 2 Chapter 386", "", "386"), n("Group 2 Chapter 385", "", "385")},
		// Shared numbers after a chapter prefix are kept
		{n("Chapter 5 Part 3", "", "5c"), n("Chapter 5 Part 2", "", "5b"), n("Chapter 5 Part 1", "", "5a")},
		// Chapters without a number fill the missing whole numbers
		{n("Chapter 4", "", "4"), n("The Return", "", "3"), n("Chapter 2", "", "2")},
		{n("Chapter 5", "", "5"), n("Rain", "", "4"), n("Snow", "", "3"), n("Chapter 2", "", "2")},
		// Chapters without a number go between the chapters around them
		{n("Chapter 11", "", "11"), n("Bonus: Beach Episode", "", "10.5"), n("Chapter 10", "", "10")},
		{n("Chapter 13", "", "13"), n("Intermission", "", "10.5"), n("Chapter 10", "", "10")},
		{n("Chapter 11", "", "11"), n("Rain", "", "10.6"), n("Snow", "", "10.5"), n("Chapter 10", "", "10")},
		{n("Chapter 4", "", "4"), n("Side Story", "", "2.5"), n("Chapter 2", "", "2")},
		{n("Chapter 3", "", "3"), n("Extra", "", "2.6"), n("Chapter 2.5", "", "2.5")},
		{n("Chapter 3", "", "3"), n("Extra", "", "2.95"), n("Chapter 2.9", "", "2.9")},
		// Chapters without a number at the ends
		{n("Afterword", "", "100.5"), n("Chapter 100", "", "100")},
		{n("Hiatus Announcement", "", ""), n("Chapter 100", "", "100")},
		{n("Chapter 1", "", "1"), n("Prologue", "", "0")},
		{n("Chapter 1", "", "1"), n("Character Profiles", "", "")},
		// A prologue before chapters in volumes is in volume 0
		{n("Vol.1 Ch.1", "1", "1"), n("Prologue", "0", "0")},
		{n("Vol.1 Ch.1", "1", "1"), n("Vol.2 Prologue", "2", "0")},
		// Chapters without a number are only numbered within a volume
		{n("Vol.1 Ch.3", "1", "3"), n("Rain", "", "2"), n("Vol.1 Ch.1", "1", "1")},
		{n("Vol.2 Ch.3", "2", "3"), n("Rain", "", ""), n("Vol.1 Ch.1", "1", "1")},
		{n("Vol.2 Ch.3", "2", "3"), n("Vol.2 Rain", "2", ""), n("Ch.1", "", "1")},
		{n("Vol.2 Ch.3", "2", "3"), n("Vol.1 Rain", "1", ""), n("Vol.2 Ch.1", "2", "1")},
		{n("Extra", "", "10.5"), n("Vol.2 Ch.10", "2", "10")},
		{n("Vol.2 Extra", "2", "10.5"), n("Vol.2 Ch.10", "2", "10")},
		{n("Vol.3 Extra", "3", ""), n("Vol.2 Ch.10", "2", "10")},
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) { assertNumbers(t, "", tc...) })
	}
}

func TestChapterRecognitionKeepsKnownNumbers(t *testing.T) {
	got := parseChapterNumbers("", []numberedChapter{
		{name: "Vol.1 Chapter 3", number: 7},
		{name: "Chapter 2", number: -1},
	})
	want := []chapterNumbering{{"1", "7"}, {"", "2"}}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
