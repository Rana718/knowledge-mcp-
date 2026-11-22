// Package library loads markdown books, parses their structure and indexes them for search.
package library

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
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

	labelNum, labelLevel int8 // bare chapter number printed just above the heading
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
	Lines     Lines
	BodyStart int // first line after the generated metadata header
	IndexAt   int // first line of the back-of-book index (Lines.Len() if none); not searched
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
	workers := max(2, runtime.NumCPU()/2)

	books := make([]*Book, len(paths))
	errs := make([]error, len(paths))
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
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
	lib.index = buildIndex(books, workers)
	lib.titles = buildTitleIndex(books)
	lib.LoadTime = time.Since(t0)
	return lib, nil
}

func loadBook(path, rel string, meta indexEntry) (*Book, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var text string
	if bytes.IndexByte(data, '\r') >= 0 {
		text = strings.ReplaceAll(string(data), "\r\n", "\n")
	} else if len(data) > 0 {
		text = unsafe.String(unsafe.SliceData(data), len(data)) // data is never modified: no second copy
	}
	b := &Book{
		ID:        strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel)),
		RelPath:   rel,
		Category:  filepath.ToSlash(filepath.Dir(rel)),
		Title:     meta.Title,
		Pages:     meta.Pages,
		SourcePDF: meta.PDFPath,
		Lines:     newLines(text),
	}
	if meta.Category != "" {
		b.Category = meta.Category
	}
	// Skip the generated header ("# Title", "> **Source Document**", "---").
	for i := 0; i < b.Lines.Len() && i < 15; i++ {
		l := strings.TrimSpace(b.Lines.At(i))
		if i == 0 && b.Title == "" {
			if _, raw, ok := parseHeading(l); ok {
				b.Title = cleanInline(raw)
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
	body := b.Lines.Span(b.BodyStart, b.Lines.Len())
	b.Chars = len(body) - strings.Count(body, " ") - strings.Count(body, "\n") - strings.Count(body, "\t")
	switch {
	case b.Chars < 1500:
		b.Status = StatusNoText
	case b.Chars < 5000 || strings.Contains(text, "System Design Infographic"):
		b.Status = StatusStub
	default:
		b.Status = StatusOK
	}
	b.Sections = parseSections(b.Lines, b.BodyStart)
	b.IndexAt = backIndex(b)
	return b, nil
}

// parseSections finds the real headings and computes each section's span and rank.
func parseSections(lines Lines, start int) []Section {
	var secs []Section
	inFence := false
	labelLine, labelNum, labelLevel := -10, 0, 0
	for i := start; i < lines.Len(); i++ {
		l := strings.TrimSpace(lines.At(i))
		if strings.HasPrefix(l, "```") || strings.HasPrefix(l, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if m := reBoldNum.FindStringSubmatch(l); m != nil {
			labelLine, labelNum, labelLevel = i, atoi(m[1]), 0
			continue
		}
		if len(l) < 3 || l[0] != '#' {
			continue
		}
		level, raw, ok := parseHeading(l)
		if !ok {
			continue
		}
		title := cleanInline(raw)
		if m := reChapNum.FindStringSubmatch(title); m != nil {
			labelLine, labelNum, labelLevel = i, atoi(m[1]), level
			continue
		}
		if isNoiseHeading(title, raw, level) {
			continue
		}
		s := Section{ID: len(secs), Title: title, Level: level, Start: i, Parent: -1}
		if i-labelLine <= 4 {
			s.labelNum, s.labelLevel = int8(labelNum), int8(labelLevel)
			labelLine = -10
		}
		secs = append(secs, s)
	}

	// Repair letter-spaced titles using the book's own vocabulary.
	var voc *vocab
	for i := range secs {
		if isSpacedLetters(secs[i].Title) {
			if voc == nil {
				voc = buildVocab(lines.Span(start, lines.Len()))
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
	secs = dropPrintedTOC(lines, merged, start)
	numberChapters(lines, secs)

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
		secs[i].End = lines.Len()
	}
	for i := range secs {
		secs[i].Chars = lines.Bytes(secs[i].Start, secs[i].End)
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

// dropPrintedTOC removes headings that belong to a printed table of contents:
// in the first tenth of the book, followed by page-numbered entries rather
// than prose, and repeated later as the real heading. Their lines fold into
// the preceding "Contents" section, so the real chapter is the only match.
func dropPrintedTOC(lines Lines, secs []Section, start int) []Section {
	front := start + (lines.Len()-start)/10
	later := map[string]int{}
	key := make([]string, len(secs))
	for i, s := range secs {
		key[i] = strings.Join(Terms(s.Title), " ")
		later[key[i]]++
	}
	out := secs[:0]
	for i, s := range secs {
		later[key[i]]--
		end := lines.Len()
		if i+1 < len(secs) {
			end = secs[i+1].Start
		}
		if s.Start < front && key[i] != "" && later[key[i]] > 0 && tocLike(lines, s.Start+1, end) {
			continue
		}
		s.ID = len(out)
		out = append(out, s)
	}
	return out
}

// tocLike reports whether lines [a, b) read like contents entries
// (ending in page numbers or dot leaders) rather than prose.
func tocLike(lines Lines, a, b int) bool {
	toc, prose := 0, 0
	for i := a; i < b; i++ {
		l := strings.TrimSpace(lines.At(i))
		if strings.Trim(l, "|-: ") == "" {
			continue
		}
		if isTOCLine(l) || rePageEnd.MatchString(l) {
			toc++
		} else if len(l) > 60 {
			prose++
		}
	}
	return toc >= 2 && toc >= 3*prose
}

// numberChapters finds chapters whose number is printed on its own line
// above the title ("**2**" or "## 4") or at the start of it ("1 Concurrency"),
// keeps the longest run that counts up like chapters (so page and figure
// numbers are ignored), names them "Chapter N: Title" and puts them all at
// the same heading level.
func numberChapters(lines Lines, secs []Section) {
	type cand struct{ sec, num, level int }
	var cs []cand
	for i, s := range secs {
		if chapterKind(s.Title) != 0 || reNumbered.MatchString(s.Title) {
			continue
		}
		if s.labelNum > 0 {
			lvl := s.Level
			if s.labelLevel > 0 {
				lvl = min(lvl, int(s.labelLevel))
			}
			cs = append(cs, cand{i, int(s.labelNum), lvl})
		} else if m := reNumTitle.FindStringSubmatch(s.Title); m != nil {
			cs = append(cs, cand{i, atoi(m[1]), s.Level})
		}
	}
	// longest chain starting at 1 or 2 where each number is 1 or 2 above the last
	best, prev := make([]int, len(cs)), make([]int, len(cs))
	end := -1
	for i, c := range cs {
		best[i], prev[i] = 0, -1
		if c.num <= 2 {
			best[i] = 1
		}
		for j := 0; j < i; j++ {
			if d := c.num - cs[j].num; (d == 1 || d == 2) && best[j] > 0 && best[j]+1 >= best[i] {
				best[i], prev[i] = best[j]+1, j
			}
		}
		if best[i] > 0 && (end < 0 || best[i] > best[end]) {
			end = i
		}
	}
	if end < 0 || best[end] < 3 {
		return
	}
	var chain []cand
	for i := end; i >= 0; i = prev[i] {
		chain = append(chain, cs[i])
	}
	// real chapters are long; short runs are numbered lists or page furniture
	first, last := secs[chain[len(chain)-1].sec], secs[chain[0].sec]
	if lines.Bytes(first.Start, last.Start) < 8000*(len(chain)-1) {
		return
	}
	for i := 1; i < len(chain); i++ {
		if lines.Bytes(secs[chain[i].sec].Start, secs[chain[i-1].sec].Start) < 1500 {
			return
		}
	}
	level := 6
	for _, c := range chain {
		level = min(level, c.level)
	}
	for _, c := range chain {
		s := &secs[c.sec]
		title := s.Title
		if s.labelNum == 0 {
			title = strings.TrimSpace(title[len(strconv.Itoa(c.num)):])
		}
		s.Title = "Chapter " + strconv.Itoa(c.num) + ": " + title
		s.Level = level
	}
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// backIndex finds the alphabetical index at the end of a book: a section
// titled "Index" in the last quarter of its text. Its entries list every
// topic of the book, so searching them only returns noise.
func backIndex(b *Book) int {
	n := b.Lines.Len()
	total := b.Lines.Bytes(b.BodyStart, n)
	for _, s := range b.Sections {
		if b.Lines.Bytes(s.Start, n) <= total/4 && reBackIndex.MatchString(s.Title) {
			return s.Start
		}
	}
	return b.Lines.Len()
}
