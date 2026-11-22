package main

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcp_books/src/library"
)

// state holds the library once background indexing finishes.
type state struct {
	once  sync.Once
	ready chan struct{}
	lib   *library.Library
	err   error
}

func newState() *state { return &state{ready: make(chan struct{})} }

func (s *state) set(lib *library.Library, err error) {
	s.once.Do(func() { s.lib, s.err = lib, err; close(s.ready) })
}

func (s *state) get(ctx context.Context) (*library.Library, error) {
	select {
	case <-s.ready:
		return s.lib, s.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ---- inputs ----

type listBooksIn struct {
	Category string `json:"category,omitempty" jsonschema:"category path or folder name, e.g. 'Rust' or '02_Databases_and_Storage'. Omit (with query) to get the category overview"`
	Query    string `json:"query,omitempty" jsonschema:"words that must appear in the book title"`
	All      bool   `json:"all,omitempty" jsonschema:"also list books without usable text"`
}

type findBooksIn struct {
	Topic    string   `json:"topic,omitempty" jsonschema:"one topic with specific terms, e.g. 'raft leader election'"`
	Topics   []string `json:"topics,omitempty" jsonschema:"several topics, e.g. the parts of a project: ['file locking flock', 'topological sort']"`
	Category string   `json:"category,omitempty" jsonschema:"optional category filter"`
	Limit    int      `json:"limit,omitempty" jsonschema:"books per topic (default 6 for one topic, 3 for several)"`
}

type findSectionsIn struct {
	Query    string `json:"query" jsonschema:"words to look for in chapter and section titles, e.g. 'file locking' or 'consistent hashing'"`
	Book     string `json:"book,omitempty" jsonschema:"limit to books: comma-separated ids or title fragments"`
	Category string `json:"category,omitempty" jsonschema:"limit to a category"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max results (default 15, max 50)"`
}

type tocIn struct {
	Book   string `json:"book" jsonschema:"book id or a unique part of its title"`
	Depth  int    `json:"depth,omitempty" jsonschema:"heading depth, 1 = chapters only (default: automatic)"`
	Filter string `json:"filter,omitempty" jsonschema:"only sections whose title contains this text, at any depth"`
	Offset int    `json:"offset,omitempty" jsonschema:"skip this many entries"`
}

type readSectionIn struct {
	Book     string `json:"book" jsonschema:"book id or a unique part of its title"`
	Section  string `json:"section" jsonschema:"section id like 's42', or a section title"`
	Offset   int    `json:"offset,omitempty" jsonschema:"continue a truncated read from this offset"`
	MaxChars int    `json:"max_chars,omitempty" jsonschema:"page size in characters (default 12000, max 40000)"`
}

type readLinesIn struct {
	Book      string `json:"book" jsonschema:"book id or a unique part of its title"`
	StartLine int    `json:"start_line" jsonschema:"1-based first line"`
	Count     int    `json:"count,omitempty" jsonschema:"number of lines (default 80, max 400)"`
}

type searchIn struct {
	Query    string `json:"query" jsonschema:"question or keywords"`
	Book     string `json:"book,omitempty" jsonschema:"limit to books: comma-separated ids or title fragments ('rust' = all Rust books)"`
	Category string `json:"category,omitempty" jsonschema:"limit to a category"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max results (default 8, max 30)"`
}

type grepIn struct {
	Pattern       string `json:"pattern" jsonschema:"text to find (literal unless regex=true)"`
	Book          string `json:"book,omitempty" jsonschema:"limit to books: comma-separated ids or title fragments"`
	Category      string `json:"category,omitempty" jsonschema:"limit to a category"`
	Regex         bool   `json:"regex,omitempty" jsonschema:"pattern is a regular expression"`
	CaseSensitive bool   `json:"case_sensitive,omitempty"`
	Limit         int    `json:"limit,omitempty" jsonschema:"max lines (default 30, max 200)"`
}

// ---- registration ----

type handler[In any] func(ctx context.Context, lib *library.Library, in In) (string, error)

func add[In any](s *mcp.Server, st *state, name, title, desc string, h handler[In]) {
	no := false
	tool := &mcp.Tool{Name: name, Description: desc,
		Annotations: &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &no}}
	mcp.AddTool(s, tool, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (res *mcp.CallToolResult, _ any, err error) {
		defer func() {
			if p := recover(); p != nil {
				log.Printf("%s: panic: %v", name, p)
				res, err = nil, fmt.Errorf("internal error in %s", name)
			}
		}()
		lib, err := st.get(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("library not available: %w", err)
		}
		text, err := h(ctx, lib, in)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	})
}

func registerTools(s *mcp.Server, st *state) {
	add(s, st, "find_books", "Find books",
		"Find the best books and sections for a topic. Pass `topic` for one subject, or `topics` (a list) to cover several parts of a project at once; the result then also ranks books by how many topics they cover. Each hit shows the matched words and a section id for read_section.", findBooks)
	add(s, st, "find_chapters", "Find chapters",
		"Search chapter and section titles across all books: 'which books have a chapter on X?'. Returns book, section id, chapter path and size, chapters first. Faster and more precise than full-text search when the topic is a likely chapter title.", findChapters)
	add(s, st, "get_book_toc", "Table of contents",
		"Chapter and section index of a book with section ids (sN) and sizes, plus the book's category, pages and status. Use filter to find sections by title.", bookTOC)
	add(s, st, "read_section", "Read section",
		"Read a chapter or section by id ('s42') or title. Long sections come in pages; the result says how to continue and lists the sub-sections.", readSection)
	add(s, st, "search_books", "Search passages",
		"Ranked passage search for a specific question, over all books or only some. One result per section, with section id and snippet.", searchBooks)
	add(s, st, "grep_books", "Grep",
		"Exact text or regex search line by line. Best for API names, flags, error messages and code identifiers.", grepBooks)
	add(s, st, "read_lines", "Read lines",
		"Read lines of a book by line number, e.g. around a grep hit.", readLines)
	add(s, st, "list_books", "List books",
		"Without arguments: the category overview. With category or query: the matching books and their ids.", listBooks)

	s.AddResource(&mcp.Resource{
		URI: "books://catalog", Name: "catalog", Title: "Book catalog", MIMEType: "text/markdown",
		Description: "All books grouped by category",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		lib, err := st.get(ctx)
		if err != nil {
			return nil, err
		}
		text, _ := listBooks(ctx, lib, listBooksIn{Category: "/", All: true})
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "text/markdown", Text: text}}}, nil
	})
}

// ---- helpers ----

func categoryMatcher(filter string) func(*library.Book) bool {
	f := strings.Trim(strings.ToLower(strings.TrimSpace(filter)), "/")
	if f == "" {
		return nil
	}
	return func(b *library.Book) bool {
		c := strings.ToLower(b.Category)
		if strings.HasPrefix(c, f) {
			return true
		}
		for _, seg := range strings.Split(c, "/") {
			if seg == f || (len(f) >= 4 && strings.Contains(seg, f)) || strings.TrimLeft(seg, "0123456789_") == f {
				return true
			}
		}
		return false
	}
}

// bookFilter combines a comma-separated book list and a category filter.
// A fragment matching several books selects all of them.
func bookFilter(lib *library.Library, books, category string) (func(*library.Book) bool, error) {
	cat := categoryMatcher(category)
	var set map[*library.Book]bool
	if strings.TrimSpace(books) != "" {
		set = map[*library.Book]bool{}
		for _, ref := range strings.Split(books, ",") {
			if strings.TrimSpace(ref) == "" {
				continue
			}
			matches := lib.Match(ref)
			if len(matches) == 0 {
				_, err := lib.Book(ref) // error with suggestions
				return nil, err
			}
			for _, b := range matches {
				set[b] = true
			}
		}
	}
	if cat == nil && set == nil {
		return nil, nil
	}
	return func(b *library.Book) bool {
		return (set == nil || set[b]) && (cat == nil || cat(b))
	}, nil
}

func status(b *library.Book) string {
	switch b.Status {
	case library.StatusNoText:
		return " [no text: scanned PDF]"
	case library.StatusStub:
		return " [stub: placeholder only]"
	}
	return ""
}

func size(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func clamp(v, def, lo, hi int) int {
	if v <= 0 {
		v = def
	}
	return max(lo, min(v, hi))
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n] + "…"
}

// where formats "s12 Chapter › Section" for a line of a book.
func where(b *library.Book, line int) string {
	if s := b.SectionAt(line); s != nil {
		return fmt.Sprintf("s%d %s", s.ID, b.Breadcrumb(s, 2))
	}
	return fmt.Sprintf("line %d", line+1)
}

func matched(words []string, query string) string {
	if len(words) == 0 {
		return "title only"
	}
	return fmt.Sprintf("%d/%d: %s", len(words), len(strings.Fields(query)), strings.Join(words, ", "))
}

var (
	reHTMLComment = regexp.MustCompile(`<!--.*?-->`)
	reInlineTags  = regexp.MustCompile(`</?(u|mark|span|sup|sub|small)>`)
	reBlankRuns   = regexp.MustCompile(`\n{3,}`)
)

// cleanText removes conversion artifacts that only cost tokens.
func cleanText(s string) string {
	s = reHTMLComment.ReplaceAllString(s, "")
	s = reInlineTags.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "<br>", " ")
	return reBlankRuns.ReplaceAllString(s, "\n\n")
}

// ---- handlers ----

func listBooks(_ context.Context, lib *library.Library, in listBooksIn) (string, error) {
	if strings.TrimSpace(in.Category) == "" && strings.TrimSpace(in.Query) == "" {
		return categoryOverview(lib), nil
	}
	cat := categoryMatcher(in.Category)
	terms := library.Terms(in.Query)
	var sb strings.Builder
	last, n, hidden := "", 0, 0
	for _, b := range lib.Books {
		if cat != nil && !cat(b) {
			continue
		}
		if len(terms) > 0 {
			have := map[string]bool{}
			for _, t := range library.Terms(b.Title + " " + b.ID) {
				have[t] = true
			}
			ok := true
			for _, t := range terms {
				ok = ok && have[t]
			}
			if !ok {
				continue
			}
		}
		if b.Status != library.StatusOK && !in.All {
			hidden++
			continue
		}
		if b.Category != last {
			fmt.Fprintf(&sb, "\n%s\n", b.Category)
			last = b.Category
		}
		fmt.Fprintf(&sb, "- %s  (id: %s)%s\n", b.Title, b.ID, status(b))
		n++
	}
	head := fmt.Sprintf("%d books", n)
	if hidden > 0 {
		head += fmt.Sprintf(" (%d without usable text hidden, all=true shows them)", hidden)
	}
	if n == 0 {
		return head + ". Call list_books with no arguments to see the categories.", nil
	}
	return head + "\n" + sb.String(), nil
}

func categoryOverview(lib *library.Library) string {
	totals := map[string]int{}
	unusable := 0
	for _, b := range lib.Books {
		if b.Status != library.StatusOK {
			unusable++
			continue
		}
		parts := strings.Split(b.Category, "/")
		for i := range parts {
			totals[strings.Join(parts[:i+1], "/")]++
		}
	}
	keys := make([]string, 0, len(totals))
	for k := range totals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d readable books (%d more have no usable text).\n\n", len(lib.Books)-unusable, unusable)
	for _, k := range keys {
		fmt.Fprintf(&sb, "%s%s (%d)\n", strings.Repeat("  ", strings.Count(k, "/")), k[strings.LastIndex(k, "/")+1:], totals[k])
	}
	sb.WriteString("\nlist_books(category=...) lists the books of a category; find_books finds books by topic.")
	return sb.String()
}

func findBooks(_ context.Context, lib *library.Library, in findBooksIn) (string, error) {
	var topics []string
	for _, t := range append([]string{in.Topic}, in.Topics...) {
		if t = strings.TrimSpace(t); t != "" {
			topics = append(topics, t)
		}
	}
	if len(topics) == 0 {
		return "", fmt.Errorf("pass topic (one subject) or topics (a list)")
	}
	allow := categoryMatcher(in.Category)
	if len(topics) == 1 {
		return findOne(lib, topics[0], allow, clamp(in.Limit, 6, 1, 20)), nil
	}
	return findMany(lib, topics[:min(len(topics), 15)], allow, clamp(in.Limit, 3, 1, 8)), nil
}

func findOne(lib *library.Library, topic string, allow func(*library.Book) bool, limit int) string {
	ranked := lib.RankBooks(topic, allow, limit, 3)
	if len(ranked) == 0 {
		return fmt.Sprintf("No books match %q. Try other or fewer terms.", topic)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Books for %q:\n", topic)
	for i, r := range ranked {
		b := r.Book
		fmt.Fprintf(&sb, "\n%d. %s%s\n   id: %s · matched %s\n", i+1, b.Title, status(b), b.ID, matched(r.Matched, topic))
		seen := map[string]bool{}
		for _, h := range r.TopHits {
			snip, line := library.Snippet(b, h.Start, h.End, topic, 200)
			w := where(b, line)
			if seen[w] {
				continue
			}
			seen[w] = true
			fmt.Fprintf(&sb, "   - %s: “%s”\n", w, snip)
		}
	}
	sb.WriteString("\nRead with read_section(book, \"sN\").")
	return sb.String()
}

func findMany(lib *library.Library, topics []string, allow func(*library.Book) bool, per int) string {
	type cover struct {
		b      *library.Book
		topics []string
		score  float64
	}
	byBook := map[*library.Book]*cover{}
	var detail strings.Builder
	for ti, topic := range topics {
		ranked := lib.RankBooks(topic, allow, per, 1)
		fmt.Fprintf(&detail, "\n%d. %s\n", ti+1, topic)
		if len(ranked) == 0 {
			detail.WriteString("   no matching books\n")
			continue
		}
		top := ranked[0].Score
		for _, r := range ranked {
			c := byBook[r.Book]
			if c == nil {
				c = &cover{b: r.Book}
				byBook[r.Book] = c
			}
			c.topics = append(c.topics, fmt.Sprint(ti+1))
			c.score += r.Score / top
			w := ""
			if len(r.TopHits) > 0 {
				w = " → " + where(r.Book, r.TopHits[0].Start)
			}
			fmt.Fprintf(&detail, "   - %s (id: %s) matched %s%s\n", r.Book.Title, r.Book.ID, matched(r.Matched, topic), w)
		}
	}
	list := make([]*cover, 0, len(byBook))
	for _, c := range byBook {
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool {
		if len(list[i].topics) != len(list[j].topics) {
			return len(list[i].topics) > len(list[j].topics)
		}
		return list[i].score > list[j].score
	})
	var sb strings.Builder
	fmt.Fprintf(&sb, "Books covering the most of %d topics:\n", len(topics))
	for i, c := range list[:min(len(list), 10)] {
		fmt.Fprintf(&sb, "%d. %s (id: %s) — topics %s\n", i+1, c.b.Title, c.b.ID, strings.Join(c.topics, ", "))
	}
	sb.WriteString("\nBest books per topic:")
	sb.WriteString(detail.String())
	sb.WriteString("\nRead with read_section(book, \"sN\") before quoting a book.")
	return sb.String()
}

func findChapters(_ context.Context, lib *library.Library, in findSectionsIn) (string, error) {
	if strings.TrimSpace(in.Query) == "" {
		return "", fmt.Errorf("query is required")
	}
	allow, err := bookFilter(lib, in.Book, in.Category)
	if err != nil {
		return "", err
	}
	hits := lib.FindSections(in.Query, allow, clamp(in.Limit, 15, 1, 50))
	if len(hits) == 0 {
		return fmt.Sprintf("No chapter or section titles match %q. Try find_books or search_books for the text.", in.Query), nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Sections titled like %q:\n\n", in.Query)
	for i, h := range hits {
		fmt.Fprintf(&sb, "%d. %s — s%d %s (%s)\n   id: %s\n", i+1, h.Book.Title, h.Section.ID, h.Book.Breadcrumb(h.Section, 2), size(h.Section.Chars), h.Book.ID)
	}
	sb.WriteString("\nRead with read_section(book, \"sN\").")
	return sb.String(), nil
}

func tocEntries(b *library.Book, depth int, filter string) []*library.Section {
	f := strings.ToLower(filter)
	var out []*library.Section
	for i := range b.Sections {
		s := &b.Sections[i]
		if f != "" {
			if strings.Contains(strings.ToLower(s.Title), f) {
				out = append(out, s)
			}
		} else if s.Rank < depth {
			out = append(out, s)
		}
	}
	return out
}

// autoDepth picks the smallest depth that shows a useful number of entries.
func autoDepth(b *library.Book) int {
	d := 1
	for d < 4 && len(tocEntries(b, d, "")) < 15 {
		d++
	}
	for d > 1 && len(tocEntries(b, d, "")) > 400 {
		d--
	}
	return d
}

func bookTOC(_ context.Context, lib *library.Library, in tocIn) (string, error) {
	b, err := lib.Book(in.Book)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s%s\nid: %s · %s", b.Title, status(b), b.ID, b.Category)
	if b.Pages > 0 {
		fmt.Fprintf(&sb, " · %d pages", b.Pages)
	}
	sb.WriteString("\n")
	if b.Status == library.StatusNoText {
		sb.WriteString("\nThis book has no extracted text, so it cannot be read or searched.")
		return sb.String(), nil
	}
	depth := in.Depth
	if depth <= 0 {
		depth = autoDepth(b)
	}
	entries := tocEntries(b, depth, in.Filter)
	if in.Filter != "" {
		fmt.Fprintf(&sb, "%d sections with %q in the title:\n\n", len(entries), in.Filter)
	} else {
		fmt.Fprintf(&sb, "%d of %d sections (depth %d):\n\n", len(entries), len(b.Sections), depth)
	}
	const pageSize = 250
	offset := max(0, in.Offset)
	end := min(len(entries), offset+pageSize)
	for _, s := range entries[min(offset, end):end] {
		indent := ""
		if in.Filter == "" {
			indent = strings.Repeat("  ", s.Rank)
		}
		fmt.Fprintf(&sb, "%ss%d %s (%s)\n", indent, s.ID, s.Title, size(s.Chars))
	}
	if end < len(entries) {
		fmt.Fprintf(&sb, "\n%d more: offset=%d\n", len(entries)-end, end)
	}
	return sb.String(), nil
}

func readSection(_ context.Context, lib *library.Library, in readSectionIn) (string, error) {
	b, err := lib.Book(in.Book)
	if err != nil {
		return "", err
	}
	s, err := b.Section(in.Section)
	if err != nil {
		return "", err
	}
	text := cleanText(sectionText(b, s))
	maxChars := clamp(in.MaxChars, 12000, 1000, 40000)
	offset := max(0, in.Offset)
	if offset >= len(text) {
		return fmt.Sprintf("offset %d is past the end of s%d (%d chars)", offset, s.ID, len(text)), nil
	}
	page := text[offset:]
	next := -1
	if len(page) > maxChars {
		cut := maxChars
		if nl := strings.LastIndex(page[:cut], "\n"); nl > maxChars/2 {
			cut = nl
		}
		for cut > 0 && (page[cut]&0xC0) == 0x80 {
			cut--
		}
		page, next = page[:cut], offset+cut
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s — s%d %s", b.Title, s.ID, b.Breadcrumb(s, 3))
	if next >= 0 || offset > 0 {
		fmt.Fprintf(&sb, " (chars %d-%d of %d)", offset, offset+len(page), len(text))
	}
	sb.WriteString("\n")
	// On the first page of a long section, list its sub-sections so the
	// agent can jump straight to the relevant one.
	if offset == 0 && next >= 0 {
		var subs []string
		for i := s.ID + 1; i < len(b.Sections) && b.Sections[i].Start < s.End && len(subs) < 40; i++ {
			if c := b.Sections[i]; c.Parent == s.ID {
				subs = append(subs, fmt.Sprintf("s%d %s (%s)", c.ID, c.Title, size(c.Chars)))
			}
		}
		if len(subs) > 0 {
			sb.WriteString("Sub-sections: " + strings.Join(subs, " · ") + "\n")
		}
	}
	sb.WriteString("\n" + page)
	if next >= 0 {
		fmt.Fprintf(&sb, "\n\n[continue: read_section(book=%q, section=\"s%d\", offset=%d)]", b.ID, s.ID, next)
	} else {
		for i := s.ID + 1; i < len(b.Sections); i++ {
			if b.Sections[i].Start >= s.End {
				fmt.Fprintf(&sb, "\n\n[end of section; next: s%d %s]", i, b.Sections[i].Title)
				break
			}
		}
	}
	return sb.String(), nil
}

// sectionText joins a section's lines, writing headings as clean titles
// ("## Chapter 55: FILE LOCKING") with depth relative to the section.
func sectionText(b *library.Book, s *library.Section) string {
	heads := map[int]*library.Section{}
	for i := s.ID; i < len(b.Sections) && b.Sections[i].Start < s.End; i++ {
		heads[b.Sections[i].Start] = &b.Sections[i]
	}
	var sb strings.Builder
	for i := s.Start; i < s.End; i++ {
		if h := heads[i]; h != nil {
			sb.WriteString(strings.Repeat("#", min(6, 1+h.Rank-s.Rank+1)) + " " + h.Title + "\n")
			continue
		}
		sb.WriteString(b.Lines[i])
		sb.WriteByte('\n')
	}
	return sb.String()
}

func readLines(_ context.Context, lib *library.Library, in readLinesIn) (string, error) {
	b, err := lib.Book(in.Book)
	if err != nil {
		return "", err
	}
	start := max(1, in.StartLine)
	if start > len(b.Lines) {
		return "", fmt.Errorf("start_line %d is past the end (%d lines)", start, len(b.Lines))
	}
	end := min(len(b.Lines), start-1+clamp(in.Count, 80, 1, 400))
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s, lines %d-%d (%s)\n", b.Title, start, end, where(b, start-1))
	for i := start - 1; i < end; i++ {
		if l := cleanText(b.Lines[i]); strings.TrimSpace(l) != "" {
			fmt.Fprintf(&sb, "%d: %s\n", i+1, l)
		}
	}
	if end < len(b.Lines) {
		fmt.Fprintf(&sb, "[continue: start_line=%d]", end+1)
	}
	return sb.String(), nil
}

func searchBooks(_ context.Context, lib *library.Library, in searchIn) (string, error) {
	if strings.TrimSpace(in.Query) == "" {
		return "", fmt.Errorf("query is required")
	}
	allow, err := bookFilter(lib, in.Book, in.Category)
	if err != nil {
		return "", err
	}
	limit := clamp(in.Limit, 8, 1, 30)
	var sb strings.Builder
	seen := map[string]bool{}
	n := 0
	for _, h := range lib.Search(in.Query, allow, limit*4) {
		snip, line := library.Snippet(h.Book, h.Start, h.End, in.Query, 300)
		w := where(h.Book, line)
		if key := h.Book.ID + w; seen[key] {
			continue
		} else {
			seen[key] = true
		}
		n++
		fmt.Fprintf(&sb, "\n%d. %s — %s (id: %s, line %d)\n   “%s”\n", n, h.Book.Title, w, h.Book.ID, line+1, snip)
		if n == limit {
			break
		}
	}
	if n == 0 {
		return fmt.Sprintf("No passages match %q. Try other keywords, or grep_books for exact text.", in.Query), nil
	}
	return fmt.Sprintf("%d results for %q:\n%s", n, in.Query, sb.String()), nil
}

func grepBooks(ctx context.Context, lib *library.Library, in grepIn) (string, error) {
	if in.Pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	expr := in.Pattern
	if !in.Regex {
		expr = regexp.QuoteMeta(expr)
	}
	if !in.CaseSensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("invalid regex: %w", err)
	}
	allow, err := bookFilter(lib, in.Book, in.Category)
	if err != nil {
		return "", err
	}
	limit := clamp(in.Limit, 30, 1, 200)
	var sb strings.Builder
	n, books, more := 0, 0, false
	for _, b := range lib.Books {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if allow != nil && !allow(b) {
			continue
		}
		first := true
		for i := b.BodyStart; i < len(b.Lines); i++ {
			l := b.Lines[i]
			loc := re.FindStringIndex(l)
			if loc == nil && strings.ContainsAny(l, "#*_`<") {
				// also match heading text without markdown ("## **Chapter 5**")
				l = library.CleanLine(l)
				loc = re.FindStringIndex(l)
			}
			if loc == nil {
				continue
			}
			if n == limit {
				more = true
				break
			}
			if first {
				fmt.Fprintf(&sb, "\n%s (id: %s)\n", b.Title, b.ID)
				first = false
				books++
			}
			from := max(0, loc[0]-120)
			for from > 0 && (l[from]&0xC0) == 0x80 {
				from--
			}
			line := strings.TrimSpace(cleanText(l[from:]))
			if from > 0 {
				line = "…" + line
			}
			fmt.Fprintf(&sb, "%d [%s]: %s\n", i+1, truncateStr(where(b, i), 70), truncateStr(line, 300))
			n++
		}
		if more {
			break
		}
	}
	if n == 0 {
		return fmt.Sprintf("No lines match %q.", in.Pattern), nil
	}
	head := fmt.Sprintf("%d lines in %d books", n, books)
	if more {
		head += " (limit reached; narrow with book or category)"
	}
	return head + ":\n" + sb.String(), nil
}
