package main

import (
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Chapter number recognition, ported from Mihon's ChapterRecognition
// (domain/src/main/java/tachiyomi/domain/chapter/service/ChapterRecognition.kt)
// so chapters get the numbers Mihon's local source would give them.
//
// Each name is read on its own: the volume or season is taken out, a part
// becomes the letter of a sub-chapter, and the chapter number is the number
// after "ch." or else the first one left. Reading the whole chapter list
// leaves out numbers every similar name shares and fills in chapters without
// a number from their neighbours (see parseChapterNumbers).
//
// Go's regexp has no lookaround, so the two patterns that used it are
// rewritten with capture groups; they match the same text.

// numberPattern is a number with optional decimals and a trailing word or
// letter, e.g. 12, 12.5, 12a, 12.extra.
const numberPattern = `([0-9]+)(\.[0-9]+)?(\.?[a-z]+)?`

var (
	// "Mokushiroku Alice Vol.1 Ch. 4: Misrepresentation" -> 4
	// (Kotlin: (?<=ch\.) *NUMBER)
	recBasic = regexp.MustCompile(`ch\. *` + numberPattern)
	// "Bleach 567: Down With Snowwhite" -> 567
	recNumber = regexp.MustCompile(numberPattern)
	// "Onepunch-Man Punch Ver002 028" -> "Onepunch-Man Punch 028"
	recUnwanted = regexp.MustCompile(`\b(?:ver|version)[^a-z]?[0-9]+`)
	// "One Piece 12 special" -> "One Piece 12special"
	// (Kotlin: \s(?=extra|special|omake), replaced by "")
	recUnwantedWhiteSpace = regexp.MustCompile(`\s(extra|special|omake)`)
	// A volume or season, the short forms only right before the number since
	// "s" also ends words. "Vol.1 Ch. 4" -> 1, "The Gamer S3 - Chapter 20" -> 3
	recVolume = regexp.MustCompile(`\b(?:(?:vol(?:ume)?|season)[^a-z0-9]? *|[vs]\.?)([0-9]+(?:\.[0-9]+)?)\b`)
	// "Chapter 1.5 Part II" -> b
	recPart = regexp.MustCompile(`\bpart\.? *([0-9]+|i{1,3}|iv|vi{0,3}|ix|x)\b`)
	// "Group 2 Chapter 386" -> "Group # Chapter #"
	recAnyNumber = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)?`)
	// A number after this is the chapter number, even when every similar
	// name has the same one. "Oshi no Ko Chapter 5 Part 1" -> 5a
	recChapterPrefix = regexp.MustCompile(`(?:\b(?:ch|chap|chapter|ep|episode)\.? *|#)$`)
	// "Bonus Chapter: Beach Episode"
	recBonus = regexp.MustCompile(`\b(?:extra|bonus|omake|special|side ?story|afterword|interlude)`)
	// "Prologue: The Beginning" -> 0
	recPrologue = regexp.MustCompile(`\b(?:prologue|one.?shot|pilot)\b`)
)

var romanNumerals = []string{"i", "ii", "iii", "iv", "v", "vi", "vii", "viii", "ix", "x"}

// chapterNumbering is the volume and chapter number recognized for a
// chapter, e.g. "10.5" or "19a"; "" when there is none.
type chapterNumbering struct {
	volume, chapter string
}

// recognizedNumber is a number with the letter of a sub-chapter, like the a
// in 19a (0: none). Kept apart from the value so chapters can still be
// compared and numbered after each other.
type recognizedNumber struct {
	value  float64
	letter byte
}

func (n recognizedNumber) String() string {
	s := formatNumber(n.value)
	if n.letter != 0 {
		s += string(n.letter)
	}
	return s
}

// formatNumber formats like Java's DecimalFormat("#.###"): at most three
// decimals (half-even), no trailing zeros, ASCII digits and a dot.
func formatNumber(v float64) string {
	v = math.RoundToEven(v*1000) / 1000
	s := strconv.FormatFloat(v, 'f', 3, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "-0" {
		s = "0"
	}
	return s
}

// chapterNumberToFloat returns a number of a chapterNumbering as a float. A
// letter only counts without decimals, from .1 for a up to .9 for i, e.g.
// 19a -> 19.1 and 1.5a -> 1.5.
func chapterNumberToFloat(number string) float64 {
	var letter byte
	if c := number[len(number)-1]; c >= 'a' && c <= 'z' {
		letter = c
		number = number[:len(number)-1]
	}
	value, _ := strconv.ParseFloat(number, 64)
	if letter != 0 {
		if tenths := int(letter-'a') + 1; tenths < 10 && !strings.Contains(number, ".") {
			value += float64(tenths) / 10
		}
	}
	return value
}

// isKnownNumber reports whether a number counts as given: -1 and below mean
// it wasn't, except -2 which sources use on purpose.
func isKnownNumber(v float64) bool { return v == -2 || v > -1 }

func cleanChapterName(seriesTitle, chapterName string) string {
	return strings.ReplaceAll(strings.ToLower(chapterName), strings.ToLower(seriesTitle), "")
}

// numberedChapter is the input of parseChapterNumbers: a chapter's name and
// the number it already has (see isKnownNumber).
type numberedChapter struct {
	name   string
	number float64
}

// parseChapterNumbers recognizes the numbers of chapters, given from the
// most to the least recent, using the whole list for what a name alone
// doesn't tell: numbers every similar name shares aren't the chapter number,
// and chapters without a number are numbered after the chapters around
// them. Known numbers are kept.
func parseChapterNumbers(seriesTitle string, chapters []numberedChapter) []chapterNumbering {
	return recognizeChapterNumbers(seriesTitle, chapters, true)
}

// recognizeChapterNumbers is parseChapterNumbers, with filling in chapters
// from their neighbours only if fill is set.
func recognizeChapterNumbers(seriesTitle string, chapters []numberedChapter, fill bool) []chapterNumbering {
	names := make([]string, len(chapters))
	for i, c := range chapters {
		names[i] = cleanChapterName(seriesTitle, c.name)
	}
	// Taken out before reading the names, so a number like the 2 in
	// "Group 2 Chapter 386" can't be picked.
	shared := findSharedNumbers(names, chapters)
	numbers := make([]*recognizedNumber, len(chapters))
	for i, c := range chapters {
		if isKnownNumber(c.number) {
			numbers[i] = &recognizedNumber{value: c.number}
		} else if n, ok := parseChapterName(removeRanges(names[i], shared[i])); ok {
			numbers[i] = &n
		}
	}
	// Only from the name: newer chapters may not be in a volume yet, so
	// neighbours don't tell.
	volumes := make([]*float64, len(chapters))
	for i, name := range names {
		if m := recVolume.FindStringSubmatch(name); m != nil {
			v, _ := strconv.ParseFloat(m[1], 64)
			volumes[i] = &v
		}
	}

	if fill {
		fillChapterNumbers(names, numbers, volumes)
	}
	out := make([]chapterNumbering, len(chapters))
	for i := range chapters {
		if volumes[i] != nil {
			out[i].volume = formatNumber(*volumes[i])
		}
		if numbers[i] != nil {
			out[i].chapter = numbers[i].String()
		}
	}
	return out
}

// span is a byte range [start, end) of a string.
type span struct{ start, end int }

// removeRanges replaces each range with a space, from the last so the
// earlier ones stay where they are.
func removeRanges(s string, ranges []span) string {
	ranges = slices.Clone(ranges)
	slices.SortFunc(ranges, func(a, b span) int { return b.start - a.start })
	for _, r := range ranges {
		s = s[:r.start] + " " + s[r.end:]
	}
	return s
}

// findSharedNumbers finds the numbers that are the same in every name of a
// group of at least three names that only differ in their numbers, e.g. the
// 2 in "Group 2 Chapter 385". Chapters with a known number are left out.
func findSharedNumbers(names []string, chapters []numberedChapter) [][]span {
	matches := make([][][]int, len(names))
	for i, n := range names {
		matches[i] = recAnyNumber.FindAllStringIndex(n, -1)
	}
	value := func(i, slot int) string { m := matches[i][slot]; return names[i][m[0]:m[1]] }

	shared := make([][]span, len(names))
	groups := map[string][]int{} // grouped by all but their numbers
	var order []string
	for i := range names {
		if isKnownNumber(chapters[i].number) {
			continue
		}
		key := recAnyNumber.ReplaceAllString(names[i], "#")
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}
	for _, key := range order {
		group := groups[key]
		if len(group) < 3 { // fewer names could share a number by chance
			continue
		}
		first := group[0]
		var sharedSlots []int
		otherSlots := 0
		for slot := range matches[first] {
			same := true
			for _, i := range group {
				if value(i, slot) != value(first, slot) {
					same = false
					break
				}
			}
			if same {
				sharedSlots = append(sharedSlots, slot)
			} else {
				otherSlots++
			}
		}
		if otherSlots == 0 { // something has to be left to tell the chapters apart
			continue
		}
		// Parts of a chapter share its number, e.g. the 5 of "Chapter 5 Part 1"
		// and "Chapter 5 Part 2".
		var removable []int
		for _, slot := range sharedSlots {
			if !recChapterPrefix.MatchString(names[first][:matches[first][slot][0]]) {
				removable = append(removable, slot)
			}
		}
		for _, i := range group {
			for _, slot := range removable {
				m := matches[i][slot]
				shared[i] = append(shared[i], span{m[0], m[1]})
			}
		}
	}
	return shared
}

// fillChapterNumbers numbers runs of chapters without a number after the
// chapters around them, only within a volume since chapter numbers may start
// over in the next one:
//   - between two chapters, the missing whole numbers when they fit, e.g.
//     2, ?, 4 -> 3, or else 10, ?, 11 -> 10.5
//   - after the newest chapter, only bonus chapters, e.g. 100, Afterword -> 100.5
//   - before the oldest chapter, only a prologue, which gets 0, and volume 0
//     when the chapter after it has a volume
func fillChapterNumbers(names []string, numbers []*recognizedNumber, volumes []*float64) {
	at := func(i int) *recognizedNumber {
		// A -2 says nothing about where a chapter is, so it can't be numbered after.
		if i < 0 || i >= len(numbers) || numbers[i] == nil || numbers[i].value < 0 {
			return nil
		}
		return numbers[i]
	}
	volumeAt := func(i int) *float64 {
		if i < 0 || i >= len(volumes) {
			return nil
		}
		return volumes[i]
	}
	for _, run := range nullRuns(numbers) {
		first, last := run.start, run.end-1
		count := run.end - run.start
		newer, older := at(first-1), at(last+1)
		olderVolume := volumeAt(last + 1)
		bonuses := 0
		for i := first; i <= last; i++ {
			if recBonus.MatchString(names[i]) {
				bonuses++
			}
		}
		switch {
		case newer != nil && older != nil:
			sameVolume := sameFloat(volumes[first-1], olderVolume) && withinVolume(volumes, run, olderVolume)
			// Out of order chapters tell nothing about the ones between them.
			if newer.value <= older.value || !sameVolume {
				continue
			}
			whole := newer.letter == 0 && older.letter == 0 &&
				math.Mod(newer.value, 1) == 0 && math.Mod(older.value, 1) == 0
			// Bonus chapters aren't missing chapters even when they would fit,
			// e.g. 2, Bonus, 4 -> 2.5.
			if bonuses == 0 && whole && newer.value-older.value-1 == float64(count) {
				for i := first; i <= last; i++ {
					numbers[i] = &recognizedNumber{value: older.value + float64(last+1-i)}
				}
			} else {
				// Never past the next whole number, which may still be a chapter to come.
				fillBetween(numbers, run, older.value, math.Min(newer.value, math.Floor(older.value)+1))
			}
		case older != nil:
			// Newer chapters without a number may as well be announcements.
			if bonuses < count || !withinVolume(volumes, run, olderVolume) {
				continue
			}
			fillBetween(numbers, run, older.value, math.Floor(older.value)+1)
		case newer != nil && newer.value > 0: // when the first chapter is already 0, that's the prologue
			for i := first; i <= last; i++ {
				if recPrologue.MatchString(names[i]) {
					numbers[i] = &recognizedNumber{value: 0}
					if volumes[i] == nil && volumes[first-1] != nil {
						zero := 0.0
						volumes[i] = &zero
					}
				}
			}
		}
	}
}

func sameFloat(a, b *float64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// withinVolume reports whether the chapters of run are in no volume or in
// volume, the one of the chapters their numbers come from.
func withinVolume(volumes []*float64, run span, volume *float64) bool {
	for i := run.start; i < run.end; i++ {
		if volumes[i] != nil && !sameFloat(volumes[i], volume) {
			return false
		}
	}
	return true
}

// nullRuns returns the ranges of consecutive chapters without a number.
func nullRuns(numbers []*recognizedNumber) []span {
	var runs []span
	for start := 0; start < len(numbers); {
		if numbers[start] != nil {
			start++
			continue
		}
		end := start
		for end < len(numbers) && numbers[end] == nil {
			end++
		}
		runs = append(runs, span{start, end})
		start = end
	}
	return runs
}

// fillBetween numbers run between lower and upper, the least recent getting
// the lowest: .5, .6 and so on after the whole number, e.g. 10, ?, ?, 11 ->
// 10.5, 10.6, or evenly spread when those don't fit.
func fillBetween(numbers []*recognizedNumber, run span, lower, upper float64) {
	count := run.end - run.start
	var tenths []float64
	for t := 5; t <= 9; t++ {
		if v := math.Floor(lower) + float64(t)/10; v > lower && v < upper {
			tenths = append(tenths, v)
		}
	}
	if len(tenths) >= count {
		// Rounded since tenths can't be exact floats, e.g. 10 + 0.6.
		for idx, i := 0, run.end-1; i >= run.start; idx, i = idx+1, i-1 {
			numbers[i] = &recognizedNumber{value: math.RoundToEven(tenths[idx]*10) / 10}
		}
		return
	}
	// Tenths run out after e.g. a 2.9 or past five chapters, and the chapters
	// still have to fit below upper.
	step := (upper - lower) / float64(count+1)
	for i := run.start; i < run.end; i++ {
		numbers[i] = &recognizedNumber{value: math.RoundToEven((lower+step*float64(run.end-i))*1000) / 1000}
	}
}

// parseChapterName returns the chapter number in name, a chapter name with
// the series title removed. A part after the chapter number becomes its
// letter, e.g. Chapter 1.5 Part I -> 1.5a.
func parseChapterName(name string) (recognizedNumber, bool) {
	// The volume can't also be the chapter number.
	withoutVolume := recVolume.ReplaceAllString(name, " ")
	if m := recPart.FindStringSubmatchIndex(withoutVolume); m != nil {
		if p := parsePart(withoutVolume[m[2]:m[3]]); p >= 1 && p <= 26 {
			if n, ok := parseNumber(withoutVolume[:m[0]] + withoutVolume[m[1]:]); ok {
				// A letter written in the number is what the source numbers the chapter by.
				if n.letter == 0 {
					n.letter = byte('a' + p - 1)
				}
				return n, true
			}
		}
	}
	// Without another number, e.g. "Part 3", the part is the chapter number.
	return parseNumber(withoutVolume)
}

func parsePart(part string) int {
	if n, err := strconv.Atoi(part); err == nil {
		return n
	}
	return slices.Index(romanNumerals, part) + 1
}

// parseNumber returns the number after "ch." or else the first one, leaving
// out version tags when there are several.
func parseNumber(name string) (recognizedNumber, bool) {
	// Some sources write decimals and letters with these, e.g. 300,a and 122-a.
	clean := strings.TrimSpace(name)
	clean = strings.NewReplacer(",", ".", "-", ".").Replace(clean)
	clean = recUnwantedWhiteSpace.ReplaceAllString(clean, "$1")

	numbers := recNumber.FindAllStringSubmatch(clean, 2)
	if len(numbers) > 1 {
		withoutTags := recUnwanted.ReplaceAllString(clean, "")
		if m := recBasic.FindStringSubmatch(withoutTags); m != nil {
			return toRecognizedNumber(m[1], m[2], m[3]), true
		}
		if m := recNumber.FindStringSubmatch(withoutTags); m != nil {
			return toRecognizedNumber(m[1], m[2], m[3]), true
		}
	}
	if len(numbers) == 0 {
		return recognizedNumber{}, false
	}
	return toRecognizedNumber(numbers[0][1], numbers[0][2], numbers[0][3]), true
}

// toRecognizedNumber turns the groups of numberPattern into a number:
// 12.5 -> 12.5, 12.extra -> 12.99, 12.a -> 12a.
func toRecognizedNumber(whole, decimal, alpha string) recognizedNumber {
	value, _ := strconv.ParseFloat(whole, 64)
	if decimal != "" {
		d, _ := strconv.ParseFloat(decimal, 64)
		value += d
	}
	switch {
	case decimal == "" && strings.Contains(alpha, "extra"):
		return recognizedNumber{value: value + 0.99}
	case decimal == "" && strings.Contains(alpha, "omake"):
		return recognizedNumber{value: value + 0.98}
	case decimal == "" && strings.Contains(alpha, "special"):
		return recognizedNumber{value: value + 0.97}
	}
	// Only up to i, like the decimals they used to stand for, since others
	// are rather words like 5v2.
	n := recognizedNumber{value: value}
	if a := strings.TrimLeft(alpha, "."); len(a) == 1 && a[0] >= 'a' && a[0] <= 'i' {
		n.letter = a[0]
	}
	return n
}
