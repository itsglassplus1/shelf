package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ---- JSON shapes ------------------------------------------------------------
// These mirror the Komga extension's Dto.kt. Every field it declares without a
// default must be present, so all of them are always sent.

type pageWrapper[T any] struct {
	Content          []T  `json:"content"`
	Empty            bool `json:"empty"`
	First            bool `json:"first"`
	Last             bool `json:"last"`
	Number           int  `json:"number"`
	NumberOfElements int  `json:"numberOfElements"`
	Size             int  `json:"size"`
	TotalElements    int  `json:"totalElements"`
	TotalPages       int  `json:"totalPages"`
}

type libraryDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type authorDTO struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

type seriesMetadataDTO struct {
	Status               string   `json:"status"`
	Created              string   `json:"created"`
	LastModified         string   `json:"lastModified"`
	Title                string   `json:"title"`
	TitleSort            string   `json:"titleSort"`
	Summary              string   `json:"summary"`
	SummaryLock          bool     `json:"summaryLock"`
	ReadingDirection     string   `json:"readingDirection"`
	ReadingDirectionLock bool     `json:"readingDirectionLock"`
	Publisher            string   `json:"publisher"`
	PublisherLock        bool     `json:"publisherLock"`
	AgeRating            *int     `json:"ageRating"`
	AgeRatingLock        bool     `json:"ageRatingLock"`
	Language             string   `json:"language"`
	LanguageLock         bool     `json:"languageLock"`
	Genres               []string `json:"genres"`
	GenresLock           bool     `json:"genresLock"`
	Tags                 []string `json:"tags"`
	TagsLock             bool     `json:"tagsLock"`
	TotalBookCount       *int     `json:"totalBookCount"`
}

type booksMetadataDTO struct {
	Authors       []authorDTO `json:"authors"`
	Tags          []string    `json:"tags"`
	ReleaseDate   *string     `json:"releaseDate"`
	Summary       string      `json:"summary"`
	SummaryNumber string      `json:"summaryNumber"`
	Created       string      `json:"created"`
	LastModified  string      `json:"lastModified"`
}

type seriesDTO struct {
	ID               string            `json:"id"`
	LibraryID        string            `json:"libraryId"`
	Name             string            `json:"name"`
	Created          string            `json:"created"`
	LastModified     string            `json:"lastModified"`
	FileLastModified string            `json:"fileLastModified"`
	BooksCount       int               `json:"booksCount"`
	Metadata         seriesMetadataDTO `json:"metadata"`
	BooksMetadata    booksMetadataDTO  `json:"booksMetadata"`
}

type mediaDTO struct {
	Status               string `json:"status"`
	MediaType            string `json:"mediaType"`
	PagesCount           int    `json:"pagesCount"`
	MediaProfile         string `json:"mediaProfile"`
	EpubDivinaCompatible bool   `json:"epubDivinaCompatible"`
}

type bookMetadataDTO struct {
	Title           string      `json:"title"`
	TitleLock       bool        `json:"titleLock"`
	Summary         string      `json:"summary"`
	SummaryLock     bool        `json:"summaryLock"`
	Number          string      `json:"number"`
	NumberLock      bool        `json:"numberLock"`
	NumberSort      float64     `json:"numberSort"`
	NumberSortLock  bool        `json:"numberSortLock"`
	ReleaseDate     *string     `json:"releaseDate"`
	ReleaseDateLock bool        `json:"releaseDateLock"`
	Authors         []authorDTO `json:"authors"`
	AuthorsLock     bool        `json:"authorsLock"`
	Tags            []string    `json:"tags"`
	TagsLock        bool        `json:"tagsLock"`
}

type bookDTO struct {
	ID               string          `json:"id"`
	SeriesID         string          `json:"seriesId"`
	SeriesTitle      string          `json:"seriesTitle"`
	LibraryID        string          `json:"libraryId"`
	Name             string          `json:"name"`
	Number           float64         `json:"number"`
	Created          string          `json:"created"`
	LastModified     string          `json:"lastModified"`
	FileLastModified string          `json:"fileLastModified"`
	SizeBytes        int64           `json:"sizeBytes"`
	Size             string          `json:"size"`
	Media            mediaDTO        `json:"media"`
	Metadata         bookMetadataDTO `json:"metadata"`
}

type pageDTO struct {
	Number    int    `json:"number"`
	FileName  string `json:"fileName"`
	MediaType string `json:"mediaType"`
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func toSeriesDTO(s *Series) seriesDTO {
	m := s.Meta
	status := m.Status
	if status == "" {
		status = "UNKNOWN" // shown as Unknown by the extension
	}
	return seriesDTO{
		ID: s.ID, LibraryID: s.LibraryID, Name: s.Name,
		Created: ts(s.Created), LastModified: ts(s.Modified), FileLastModified: ts(s.DirMtime),
		BooksCount: len(s.Books),
		Metadata: seriesMetadataDTO{
			Status: status, Created: ts(s.Created), LastModified: ts(s.Modified),
			Title: m.Title, TitleSort: m.Title, Summary: m.Summary, Publisher: m.Publisher,
			Genres: m.Genres, Tags: m.Tags,
		},
		// The extension takes Author/Artist from the books' aggregated authors.
		BooksMetadata: booksMetadataDTO{
			Authors: m.Authors, Tags: []string{},
			Created: ts(s.Created), LastModified: ts(s.Modified),
		},
	}
}

func toBookDTO(ch chapter) bookDTO {
	b := ch.Book
	mediaType := "application/zip"
	switch ext := strings.ToLower(filepath.Ext(b.Path)); {
	case b.IsDir:
		mediaType = "application/x-directory"
	case ext == ".cbr" || ext == ".rar":
		mediaType = "application/x-rar-compressed"
	}
	authors := []authorDTO{}
	var releaseDate *string
	if ch.info != nil {
		authors = append(authors, ch.info.Authors...)
		for _, t := range ch.info.Translators {
			authors = append(authors, authorDTO{Name: t, Role: "translator"}) // shown as scanlator
		}
		if ch.info.ReleaseDate != "" {
			releaseDate = &ch.info.ReleaseDate // used as the chapter date
		}
	}
	return bookDTO{
		ID: b.ID, SeriesID: b.Series.ID, SeriesTitle: b.Series.Meta.Title, LibraryID: b.Series.LibraryID,
		Name: b.Title, Number: ch.number,
		Created: ts(b.Modified), LastModified: ts(b.Modified), FileLastModified: ts(b.Modified),
		SizeBytes: b.Size, Size: humanSize(b.Size),
		// pagesCount is not used by the extension; counting would mean opening every archive.
		Media: mediaDTO{Status: "READY", MediaType: mediaType, MediaProfile: "DIVINA"},
		Metadata: bookMetadataDTO{
			Title: ch.title, Number: ch.numberStr, NumberSort: ch.number,
			ReleaseDate: releaseDate, Authors: authors, Tags: []string{},
		},
	}
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// paginate slices items into Komga's page wrapper. size <= 0 means unpaged.
func paginate[T any](items []T, page, size int) pageWrapper[T] {
	total := len(items)
	if size <= 0 {
		page, size = 0, max(total, 1)
	}
	// Past the last page the result is empty; capping page keeps page*size
	// from overflowing (and going negative) on absurd ?page= values.
	page = min(max(page, 0), total/size+1)
	start := min(page*size, total)
	end := min(start+size, total)
	content := items[start:end]
	if content == nil {
		content = []T{}
	}
	totalPages := (total + size - 1) / size
	return pageWrapper[T]{
		Content: content, Empty: len(content) == 0, First: page == 0, Last: end >= total,
		Number: page, NumberOfElements: len(content), Size: size,
		TotalElements: total, TotalPages: totalPages,
	}
}

// ---- HTTP -------------------------------------------------------------------

type server struct {
	cat  *Catalog
	auth *authenticator // nil: no login required (SHELF_AUTH=off)
	mux  *http.ServeMux
}

func newServer(cat *Catalog, auth *authenticator) http.Handler {
	s := &server{cat: cat, auth: auth, mux: http.NewServeMux()}
	m := s.mux

	m.HandleFunc("GET /api/v1/libraries", s.libraries)
	m.HandleFunc("POST /api/v1/series/list", s.seriesList)
	m.HandleFunc("GET /api/v1/series/{id}", s.seriesDetail)
	m.HandleFunc("GET /api/v1/series/{id}/books", s.seriesBooks)
	m.HandleFunc("GET /api/v1/series/{id}/thumbnail", s.seriesThumbnail)
	m.HandleFunc("GET /api/v1/books/{id}", s.bookDetail)
	m.HandleFunc("GET /api/v1/books/{id}/pages", s.bookPages)
	m.HandleFunc("GET /api/v1/books/{id}/pages/{n}", s.bookPage)
	m.HandleFunc("GET /api/v1/books/{id}/thumbnail", s.bookThumbnail)

	// Features this server doesn't have (book search, collections, read
	// lists): answer with empty results so they simply stay empty.
	emptyPage := func(w http.ResponseWriter, r *http.Request) { writeJSON(w, paginate([]struct{}{}, 0, 20)) }
	m.HandleFunc("POST /api/v1/books/list", emptyPage)
	m.HandleFunc("GET /api/v1/collections", emptyPage)
	m.HandleFunc("GET /api/v1/readlists", emptyPage)

	// Values for the extension's filter menus, collected from series metadata.
	m.HandleFunc("GET /api/v1/genres", s.stringFacet(func(m seriesMeta) []string { return m.Genres }))
	m.HandleFunc("GET /api/v1/tags", s.stringFacet(func(m seriesMeta) []string { return m.Tags }))
	m.HandleFunc("GET /api/v1/publishers", s.stringFacet(func(m seriesMeta) []string {
		if m.Publisher == "" {
			return nil
		}
		return []string{m.Publisher}
	}))
	m.HandleFunc("GET /api/v1/authors", s.authors)

	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("WARN: unhandled request %s %s", r.Method, r.URL.RequestURI())
		http.NotFound(w, r)
	})
	return s
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/healthz" {
		// Checked first, so refused requests cause no library checks or disk reads.
		if !s.auth.allow(w, r) {
			return
		}
		s.cat.noteRequest(false) // reading counts as activity; browse handlers also check for changes
	}
	s.mux.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("ERROR: writing response: %v", err)
	}
}

// stringFacet answers with the distinct values of one metadata field across
// all series, in natural order.
func (s *server) stringFacet(field func(seriesMeta) []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.cat.noteRequest(true)
		seen := map[string]bool{}
		out := []string{}
		for _, se := range s.cat.current().series {
			for _, v := range field(se.Meta) {
				if !seen[v] {
					seen[v] = true
					out = append(out, v)
				}
			}
		}
		slices.SortFunc(out, naturalCompare)
		writeJSON(w, out)
	}
}

func (s *server) authors(w http.ResponseWriter, r *http.Request) {
	s.cat.noteRequest(true)
	seen := map[authorDTO]bool{}
	out := []authorDTO{}
	for _, se := range s.cat.current().series {
		for _, a := range se.Meta.Authors {
			if !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	slices.SortFunc(out, func(a, b authorDTO) int { return naturalCompare(a.Name, b.Name) })
	writeJSON(w, out)
}

func (s *server) libraries(w http.ResponseWriter, r *http.Request) {
	s.cat.noteRequest(true)
	out := []libraryDTO{}
	for _, l := range s.cat.current().libs {
		out = append(out, libraryDTO{ID: l.ID, Name: l.Name})
	}
	writeJSON(w, out)
}

type searchBody struct {
	Condition      json.RawMessage `json:"condition"`
	FullTextSearch *string         `json:"fullTextSearch"`
}

func (s *server) seriesList(w http.ResponseWriter, r *http.Request) {
	s.cat.noteRequest(true)
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	size, err := strconv.Atoi(q.Get("size"))
	if err != nil || size <= 0 {
		size = 20
	}
	page, size = max(page, 0), min(size, 500)

	var body searchBody
	if r.Body != nil {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
	}
	var terms []string
	if body.FullTextSearch != nil {
		terms = strings.Fields(strings.ToLower(unescapeLucene(*body.FullTextSearch)))
	}

	var matched []*Series
	for _, se := range s.cat.current().series {
		if matchesTerms(se.Meta.Title+"\n"+se.Name, terms) && evalCondition(body.Condition, se) {
			matched = append(matched, se)
		}
	}
	sortSeries(matched, q.Get("sort"))

	out := make([]seriesDTO, len(matched))
	for i, se := range matched {
		out[i] = toSeriesDTO(se)
	}
	writeJSON(w, paginate(out, page, size))
}

// The extension backslash-escapes Lucene syntax; undo that for plain matching.
func unescapeLucene(s string) string {
	var b strings.Builder
	esc := false
	for _, r := range s {
		if r == '\\' && !esc {
			esc = true
			continue
		}
		esc = false
		b.WriteRune(r)
	}
	return b.String()
}

// matchesTerms: every search word must appear in the title or folder name
// (case-insensitive).
func matchesTerms(name string, terms []string) bool {
	lower := strings.ToLower(name)
	for _, t := range terms {
		if t == "and" || t == "or" || t == "not" {
			continue // the extension lowercases Lucene operators; treat them as noise
		}
		if !strings.Contains(lower, t) {
			return false
		}
	}
	return true
}

type operator struct {
	Operator string          `json:"operator"`
	Value    json.RawMessage `json:"value"`
}

// evalCondition evaluates the extension's condition tree against a series.
// Conditions on data this server doesn't have (read status) are treated as
// matching; there are no collections, so a collection condition never matches.
func evalCondition(raw json.RawMessage, s *Series) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	var node map[string]json.RawMessage
	if err := json.Unmarshal(raw, &node); err != nil {
		return true
	}
	for key, val := range node {
		switch key {
		case "allOf", "anyOf":
			var children []json.RawMessage
			if json.Unmarshal(val, &children) != nil || len(children) == 0 {
				continue
			}
			any := false
			for _, c := range children {
				ok := evalCondition(c, s)
				if key == "allOf" && !ok {
					return false
				}
				any = any || ok
			}
			if key == "anyOf" && !any {
				return false
			}
		case "collectionId":
			return false
		case "libraryId", "genre", "tag", "publisher", "seriesStatus", "author":
			var op operator
			if json.Unmarshal(val, &op) != nil || (op.Operator != "is" && op.Operator != "isNot") {
				continue
			}
			if matchLeaf(key, op.Value, s) != (op.Operator == "is") {
				return false
			}
		}
	}
	return true
}

// matchLeaf reports whether a series has the given value for one field.
func matchLeaf(key string, value json.RawMessage, s *Series) bool {
	if key == "author" {
		var a authorDTO
		if json.Unmarshal(value, &a) != nil {
			return false
		}
		return slices.ContainsFunc(s.Meta.Authors, func(x authorDTO) bool {
			return strings.EqualFold(x.Name, a.Name) && strings.EqualFold(x.Role, a.Role)
		})
	}
	var v string
	if json.Unmarshal(value, &v) != nil {
		return false
	}
	eq := func(x string) bool { return strings.EqualFold(x, v) }
	switch key {
	case "libraryId":
		return s.LibraryID == v
	case "genre":
		return slices.ContainsFunc(s.Meta.Genres, eq)
	case "tag":
		return slices.ContainsFunc(s.Meta.Tags, eq)
	case "publisher":
		return eq(s.Meta.Publisher)
	case "seriesStatus":
		return eq(s.Meta.Status)
	}
	return false
}

func sortSeries(list []*Series, sortParam string) {
	field, dir, _ := strings.Cut(sortParam, ",")
	desc := dir == "desc"
	var cmp func(a, b *Series) int
	switch field {
	case "createdDate":
		cmp = func(a, b *Series) int { return a.Created.Compare(b.Created) }
	case "lastModifiedDate":
		cmp = func(a, b *Series) int { return a.Modified.Compare(b.Modified) }
	case "random":
		// Same order for 10 minutes, so paging through results stays consistent.
		seed := uint64(time.Now().Unix() / 600)
		rng := rand.New(rand.NewPCG(seed, seed))
		rng.Shuffle(len(list), func(i, j int) { list[i], list[j] = list[j], list[i] })
		return
	default: // "metadata.titleSort", "relevance", or none: by displayed title
		cmp = func(a, b *Series) int { return naturalCompare(a.Meta.Title, b.Meta.Title) }
	}
	slices.SortStableFunc(list, func(a, b *Series) int {
		if desc {
			return cmp(b, a)
		}
		return cmp(a, b)
	})
}

func (s *server) series(w http.ResponseWriter, r *http.Request) *Series {
	se := s.cat.current().seriesByID[r.PathValue("id")]
	if se == nil {
		http.NotFound(w, r)
	}
	return se
}

func (s *server) book(w http.ResponseWriter, r *http.Request) *Book {
	b := s.cat.current().booksByID[r.PathValue("id")]
	if b == nil {
		http.NotFound(w, r)
	}
	return b
}

func (s *server) seriesDetail(w http.ResponseWriter, r *http.Request) {
	s.cat.noteRequest(true)
	if se := s.series(w, r); se != nil {
		writeJSON(w, toSeriesDTO(se))
	}
}

func (s *server) seriesBooks(w http.ResponseWriter, r *http.Request) {
	s.cat.noteRequest(true)
	se := s.series(w, r)
	if se == nil {
		return
	}
	chapters := s.cat.chapters(se)
	out := make([]bookDTO, len(chapters))
	for i, ch := range chapters {
		out[i] = toBookDTO(ch)
	}
	writeJSON(w, paginate(out, 0, 0)) // always unpaged
}

func (s *server) bookDetail(w http.ResponseWriter, r *http.Request) {
	b := s.book(w, r)
	if b == nil {
		return
	}
	// Numbering depends on the whole series, so build its chapter list.
	for _, ch := range s.cat.chapters(b.Series) {
		if ch.Book == b {
			writeJSON(w, toBookDTO(ch))
			return
		}
	}
	http.NotFound(w, r)
}

func (s *server) bookPages(w http.ResponseWriter, r *http.Request) {
	b := s.book(w, r)
	if b == nil {
		return
	}
	pages, err := s.cat.pagesFor(b)
	if err != nil {
		log.Printf("ERROR: listing pages of %s: %v", b.Path, err)
		http.Error(w, "cannot read chapter", http.StatusInternalServerError)
		return
	}
	out := make([]pageDTO, len(pages))
	for i, p := range pages {
		out[i] = pageDTO{Number: i + 1, FileName: p.name, MediaType: p.mediaType}
	}
	writeJSON(w, out)
}

func (s *server) bookPage(w http.ResponseWriter, r *http.Request) {
	b := s.book(w, r)
	if b == nil {
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.servePage(w, r, b, n)
}

func (s *server) bookThumbnail(w http.ResponseWriter, r *http.Request) {
	if b := s.book(w, r); b != nil {
		s.serveOpened(w, r, b, 1, func() (*openedPage, error) { return s.cat.firstPage(b) })
	}
}

func (s *server) seriesThumbnail(w http.ResponseWriter, r *http.Request) {
	se := s.series(w, r)
	if se == nil {
		return
	}
	cover, err := s.cat.seriesCover(se)
	switch {
	case errors.Is(err, errNoPage):
		http.NotFound(w, r)
	case err != nil:
		log.Printf("ERROR: cover for series %q: %v", se.Name, err)
		http.Error(w, "cannot read cover", http.StatusInternalServerError)
	case cover != "":
		f, err := os.Open(cover)
		if err != nil {
			http.Error(w, "cannot read cover", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			http.Error(w, "cannot read cover", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", imageTypes[strings.ToLower(filepath.Ext(cover))])
		w.Header().Set("Cache-Control", "private, max-age=3600")
		http.ServeContent(w, r, filepath.Base(cover), fi.ModTime(), f)
	default: // copies disabled or failed: serve the first page directly
		b := se.Books[0]
		s.serveOpened(w, r, b, 1, func() (*openedPage, error) { return s.cat.firstPage(b) })
	}
}

func (s *server) servePage(w http.ResponseWriter, r *http.Request, b *Book, n int) {
	s.serveOpened(w, r, b, n, func() (*openedPage, error) { return s.cat.openPage(b, n) })
}

// serveOpened sends page n of b, as opened by open.
func (s *server) serveOpened(w http.ResponseWriter, r *http.Request, b *Book, n int, open func() (*openedPage, error)) {
	p, err := open()
	if errors.Is(err, errNoPage) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		log.Printf("ERROR: reading page %d of %s: %v", n, b.Path, err)
		http.Error(w, "cannot read page", http.StatusInternalServerError)
		return
	}
	defer p.Close()

	w.Header().Set("Content-Type", p.mediaType)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if p.seeker != nil {
		http.ServeContent(w, r, p.name, p.modTime, p.seeker)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(p.size, 10))
	if !p.modTime.IsZero() {
		w.Header().Set("Last-Modified", p.modTime.UTC().Format(http.TimeFormat))
	}
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, p); err != nil {
		log.Printf("WARN: sending page %d of %s: %v", n, b.Path, err)
	}
}
