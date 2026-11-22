// Package library loads markdown books, parses their structure and indexes them for search.
package library

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Book statuses.
const (
	StatusOK     = "ok"
	StatusNoText = "no_text" // scanned PDF, conversion produced (almost) no text; needs OCR
	StatusStub   = "stub"    // tiny placeholder / generic summary, not the real book content
)

// Section is one heading of a book and the line range it covers.
type Section struct {
	ID     int    // index in Book.Sections
	Title  string // cleaned heading text
	Level  int    // raw markdown level (1..6)
	Rank   int    // normalized depth: 0 = top-most heading level used by this book
	Parent int    // enclosing section ID, -1 if none
	Start  int    // 0-based line of the heading
	End    int    // 0-based exclusive end line (start of next heading of same/higher level)
	Chars  int    // bytes covered by [Start, End)
}

// Book is a single markdown book.
type Book struct {
	ID        string // file stem, unique
	Title     string
	Category  string // e.g. "04_Programming_Languages/Go/Foundations_and_Idiomatic"
	RelPath   string // path relative to the library root
	Pages     int
	SourcePDF string
	Status    string
	Chars     int // non-whitespace characters of body text
	Lines     []string
	BodyStart int // first line after the generated metadata header
	Sections  []Section
}

// Library is the loaded, indexed collection.
type Library struct {
	Root     string
	Books    []*Book
	byID     map[string]*Book
	index    *Index
	titles   *titleIndex
	LoadTime time.Duration
}

type indexEntry struct {
	Category     string `json:"category"`
	Title        string `json:"title"`
	MarkdownFile string `json:"markdown_file"`
	PDFPath      string `json:"pdf_path"`
	Pages        int    `json:"pages"`
}

// Load reads all books under root (metadata from INDEX.json) and indexes them.
func Load(root string) (*Library, error) {
	t0 := time.Now()
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	meta := map[string]indexEntry{} // rel path -> metadata
	if data, err := os.ReadFile(filepath.Join(root, "INDEX.json")); err == nil {
		var entries []indexEntry
		if err := json.Unmarshal(data, &entries); err != nil {
			return nil, fmt.Errorf("parse INDEX.json: %w", err)
		}
		for _, e := range entries {
			meta[filepath.ToSlash(filepath.Join(e.Category, e.MarkdownFile))] = e
		}
	}

	var paths []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".md") {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no .md books found under %s", root)
	}
	sort.Strings(paths)

	books := make([]*Book, len(paths))
	errs := make([]error, len(paths))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for i, p := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			books[i], errs[i] = loadBook(p, rel, meta[rel])
		}()
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return nil, e
		}
	}

	lib := &Library{Root: root, Books: books, byID: map[string]*Book{}}
	for _, b := range books {
		if _, dup := lib.byID[strings.ToLower(b.ID)]; dup {
			b.ID = filepath.Base(b.Category) + "__" + b.ID
		}
		lib.byID[strings.ToLower(b.ID)] = b
	}
	lib.index = buildIndex(books)
	lib.titles = buildTitleIndex(books)
	lib.LoadTime = time.Since(t0)
	return lib, nil
}

func loadBook(path, rel string, meta indexEntry) (*Book, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	b := &Book{
		ID:        strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel)),
		RelPath:   rel,
		Category:  filepath.ToSlash(filepath.Dir(rel)),
		Title:     meta.Title,
		Pages:     meta.Pages,
		SourcePDF: meta.PDFPath,
		Lines:     strings.Split(text, "\n"),
	}
	if meta.Category != "" {
		b.Category = meta.Category
	}
	// Skip the generated header ("# Title", "> **Source Document**", "---").
	for i := 0; i < len(b.Lines) && i < 15; i++ {
		l := strings.TrimSpace(b.Lines[i])
		if i == 0 && b.Title == "" {
			if m := reHeading.FindStringSubmatch(l); m != nil {
				b.Title = cleanInline(m[2])
			}
		}
		if l == "---" {
			b.BodyStart = i + 1
			break
		}
	}
	if b.Title == "" {
		b.Title = strings.ReplaceAll(b.ID, "_", " ")
	}
	for _, l := range b.Lines[b.BodyStart:] {
		for _, r := range l {
			if r != ' ' && r != '\t' {
				b.Chars++
			}
		}
	}
	switch {
	case b.Chars < 1500:
		b.Status = StatusNoText
	case b.Chars < 5000 || strings.Contains(text, "System Design Infographic"):
		b.Status = StatusStub
	default:
		b.Status = StatusOK
	}
	b.Sections = parseSections(b.Lines, b.BodyStart)
	return b, nil
}

// parseSections finds the real headings and computes each section's span and rank.
func parseSections(lines []string, start int) []Section {
	var secs []Section
	inFence := false
	for i := start; i < len(lines); i++ {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "```") || strings.HasPrefix(l, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence || len(l) < 3 || l[0] != '#' {
			continue
		}
		m := reHeading.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		title := cleanInline(m[2])
		if isNoiseHeading(title, m[2], len(m[1])) {
			continue
		}
		secs = append(secs, Section{ID: len(secs), Title: title, Level: len(m[1]), Start: i, Parent: -1})
	}

	// Repair letter-spaced titles using the book's own vocabulary.
	var voc *vocab
	for i := range secs {
		if isSpacedLetters(secs[i].Title) {
			if voc == nil {
				voc = buildVocab(lines[start:])
			}
			secs[i].Title = voc.despace(secs[i].Title)
		}
	}
	// Merge a bare "Chapter 7" label into the title that follows it.
	merged := secs[:0]
	for i := 0; i < len(secs); i++ {
		s := secs[i]
		if reBareChapter.MatchString(s.Title) && i+1 < len(secs) && secs[i+1].Start-s.Start <= 4 {
			next := &secs[i+1]
			if chapterKind(next.Title) == 0 {
				next.Title = s.Title + ": " + next.Title
			}
			next.Start = s.Start
			next.Level = min(next.Level, s.Level)
			continue
		}
		s.ID = len(merged)
		merged = append(merged, s)
	}
	secs = merged

	for i := 0; i+1 < len(secs); i++ {
		s, next := &secs[i], secs[i+1]
		if chapterKind(s.Title) != 0 || next.Level <= s.Level || (s.Title[0] >= '0' && s.Title[0] <= '9') {
			continue
		}
		if m := reSubOne.FindStringSubmatch(next.Title); m != nil {
			s.Title = "Chapter " + m[1] + ": " + s.Title
		}
	}

	// Parts rank above chapters, chapters above other headings of the same level.
	eff := func(s Section) int { return s.Level * 3 }
	nPart, nChap := 0, 0
	for _, s := range secs {
		switch chapterKind(s.Title) {
		case 2:
			nPart++
		case 1:
			nChap++
		}
	}
	if nPart >= 2 || nChap >= 2 {
		eff = func(s Section) int {
			k := chapterKind(s.Title)
			if (k == 2 && nPart < 2) || (k == 1 && nChap < 2) {
				k = 0
			}
			return s.Level*3 - k
		}
	}

	// Rank = number of structural levels (used at least twice) above a heading.
	count := map[int]int{}
	for _, s := range secs {
		count[eff(s)]++
	}
	var major []int
	for lvl, n := range count {
		if n >= 2 {
			major = append(major, lvl)
		}
	}
	sort.Ints(major)

	// End lines and parents via a stack of open sections.
	var stack []int
	for i := range secs {
		s := &secs[i]
		e := eff(*s)
		for _, lvl := range major {
			if lvl < e {
				s.Rank++
			}
		}
		for len(stack) > 0 && eff(secs[stack[len(stack)-1]]) >= e {
			top := stack[len(stack)-1]
			secs[top].End = s.Start
			stack = stack[:len(stack)-1]
		}
		if len(stack) > 0 {
			s.Parent = stack[len(stack)-1]
		}
		// A one-off level (title page, author line) must not enclose chapters.
		if count[e] < 2 && i+1 < len(secs) {
			s.End = secs[i+1].Start
			continue
		}
		stack = append(stack, i)
	}
	for _, i := range stack {
		secs[i].End = len(lines)
	}
	for i := range secs {
		n := 0
		for _, l := range lines[secs[i].Start:secs[i].End] {
			n += len(l) + 1
		}
		secs[i].Chars = n
	}
	return secs
}

// Match returns the book with ID ref, or all books whose title/id/category contain every word of ref.
func (l *Library) Match(ref string) []*Book {
	ref = strings.TrimSpace(ref)
	key := strings.ToLower(strings.TrimSuffix(filepath.Base(ref), ".md"))
	if b, ok := l.byID[key]; ok {
		return []*Book{b}
	}
	terms := Terms(ref)
	if len(terms) == 0 {
		return nil
	}
	var hits []*Book
	for _, b := range l.Books {
		hay := map[string]bool{}
		tokenize(b.Title+" "+b.ID+" "+b.Category, func(t string) { hay[t] = true })
		ok := true
		for _, t := range terms {
			if !hay[t] {
				ok = false
				break
			}
		}
		if ok {
			hits = append(hits, b)
		}
	}
	// fall back to an acronym such as "tlpi" or "clrs"
	if a := strings.ToLower(ref); len(hits) == 0 && len(a) >= 3 && len(a) <= 6 && !strings.Contains(a, " ") {
		for _, b := range l.Books {
			if strings.HasPrefix(acronym(b.Title), a) {
				hits = append(hits, b)
			}
		}
	}
	return hits
}

// Book resolves a reference to exactly one book (see Match).
func (l *Library) Book(ref string) (*Book, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, fmt.Errorf("book is required (use list_books or find_books to get a book id)")
	}
	hits := l.Match(ref)
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		if sug := l.suggest(ref, 5); len(sug) > 0 {
			return nil, fmt.Errorf("no book matches %q. Closest: %s", ref, strings.Join(sug, ", "))
		}
		return nil, fmt.Errorf("no book matches %q; use list_books or find_books", ref)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%q matches %d books, pass one of these ids:\n", ref, len(hits))
	for i, b := range hits {
		if i == 15 {
			fmt.Fprintf(&sb, "  … and %d more\n", len(hits)-15)
			break
		}
		fmt.Fprintf(&sb, "  %s  (%s)\n", b.ID, b.Title)
	}
	return nil, fmt.Errorf("%s", sb.String())
}

// Section resolves "s12", "12" or a title (exact match first, then substring).
func (b *Book) Section(ref string) (*Section, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("section is required")
	}
	num := strings.TrimPrefix(strings.ToLower(ref), "s")
	var id int
	if _, err := fmt.Sscanf(num, "%d", &id); err == nil && fmt.Sprint(id) == num {
		if id < 0 || id >= len(b.Sections) {
			return nil, fmt.Errorf("section %d out of range (book has %d sections, ids 0..%d)", id, len(b.Sections), len(b.Sections)-1)
		}
		return &b.Sections[id], nil
	}
	low := strings.ToLower(ref)
	var sub *Section
	for i := range b.Sections {
		t := strings.ToLower(b.Sections[i].Title)
		if t == low {
			return &b.Sections[i], nil
		}
		if sub == nil && strings.Contains(t, low) {
			sub = &b.Sections[i]
		}
	}
	if sub != nil {
		return sub, nil
	}
	return nil, fmt.Errorf("no section titled %q in %s; use get_book_toc to list sections", ref, b.ID)
}

// SectionAt returns the innermost section containing line, or nil.
func (b *Book) SectionAt(line int) *Section {
	i := sort.Search(len(b.Sections), func(i int) bool { return b.Sections[i].Start > line }) - 1
	for i >= 0 {
		if line < b.Sections[i].End {
			return &b.Sections[i]
		}
		i = b.Sections[i].Parent
	}
	return nil
}

// Breadcrumb returns "Ancestor › Parent › Section" (at most depth items).
func (b *Book) Breadcrumb(s *Section, depth int) string {
	var parts []string
	for cur := s; cur != nil && len(parts) < depth; {
		parts = append([]string{cur.Title}, parts...)
		if cur.Parent < 0 {
			break
		}
		cur = &b.Sections[cur.Parent]
	}
	return strings.Join(parts, " › ")
}

// suggest returns ids of the books sharing the most words with ref.
func (l *Library) suggest(ref string, n int) []string {
	terms := Terms(ref)
	type cand struct {
		id    string
		score int
	}
	var cs []cand
	for _, b := range l.Books {
		hay := map[string]bool{}
		tokenize(b.Title+" "+b.ID, func(t string) { hay[t] = true })
		k := 0
		for _, t := range terms {
			if hay[t] {
				k++
			}
		}
		if k > 0 {
			cs = append(cs, cand{b.ID, k})
		}
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].score > cs[j].score })
	var out []string
	for i := 0; i < len(cs) && i < n; i++ {
		out = append(out, cs[i].id)
	}
	return out
}

// acronym returns the lowercase initials of a title.
func acronym(title string) string {
	var b strings.Builder
	for _, w := range strings.Fields(title) {
		r := w[0]
		if r >= 'A' && r <= 'Z' {
			b.WriteByte(r + 32)
		} else if r >= 'a' && r <= 'z' {
			b.WriteByte(r)
		}
	}
	return b.String()
}
