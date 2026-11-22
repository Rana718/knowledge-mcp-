package library

import (
	"math"
	"runtime"
	"sort"
	"strings"
	"sync"
)

const (
	chunkSoftBytes = 1800 // cut a chunk at the next blank line after this size
	chunkHardBytes = 3600 // always cut at this size
	bm25K1         = 1.2
	bm25B          = 0.75
)

type chunk struct {
	book       int32
	start, end int32 // line range [start, end)
	length     uint32
}

type posting struct {
	chunk uint32
	tf    uint16
}

// Index is an in-memory BM25 inverted index over book chunks.
type Index struct {
	books    []*Book
	chunks   []chunk
	postings map[string][]posting
	avgLen   float64
}

type bookIndex struct {
	chunks   []chunk
	postings map[string][]posting
}

func buildIndex(books []*Book) *Index {
	parts := make([]bookIndex, len(books))
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for i, b := range books {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			parts[i] = indexBook(int32(i), b)
		}()
	}
	wg.Wait()

	idx := &Index{books: books, postings: make(map[string][]posting, 1<<20)}
	var total float64
	for _, p := range parts {
		base := uint32(len(idx.chunks))
		for _, c := range p.chunks {
			total += float64(c.length)
		}
		idx.chunks = append(idx.chunks, p.chunks...)
		for term, ps := range p.postings {
			dst := idx.postings[term]
			for _, q := range ps {
				dst = append(dst, posting{chunk: q.chunk + base, tf: q.tf})
			}
			idx.postings[term] = dst
		}
	}
	if len(idx.chunks) > 0 {
		idx.avgLen = total / float64(len(idx.chunks))
	}
	return idx
}

func indexBook(bi int32, b *Book) bookIndex {
	out := bookIndex{postings: map[string][]posting{}}
	if b.Status == StatusNoText {
		return out
	}
	isStart := make(map[int]bool, len(b.Sections))
	for _, s := range b.Sections {
		isStart[s.Start] = true
	}
	tf := map[string]int{}
	emit := func(start, end int) {
		if end <= start {
			return
		}
		clear(tf)
		n := 0
		add := func(t string) { tf[t]++; n++ }
		// include section and parent titles so passages match their topic
		if s := b.SectionAt(start); s != nil {
			tokenize(s.Title, add)
			if s.Parent >= 0 {
				tokenize(b.Sections[s.Parent].Title, add)
			}
		}
		for _, l := range b.Lines[start:end] {
			if isTOCLine(l) {
				continue // printed table-of-contents lines only add noise
			}
			tokenize(l, add)
		}
		if n == 0 {
			return
		}
		id := uint32(len(out.chunks))
		out.chunks = append(out.chunks, chunk{book: bi, start: int32(start), end: int32(end), length: uint32(n)})
		for t, c := range tf {
			if c > math.MaxUint16 {
				c = math.MaxUint16
			}
			out.postings[t] = append(out.postings[t], posting{chunk: id, tf: uint16(c)})
		}
	}

	start, size := b.BodyStart, 0
	for i := b.BodyStart; i < len(b.Lines); i++ {
		if isStart[i] && i > start {
			emit(start, i)
			start, size = i, 0
		}
		size += len(b.Lines[i]) + 1
		if size >= chunkHardBytes || (size >= chunkSoftBytes && strings.TrimSpace(b.Lines[i]) == "") {
			emit(start, i+1)
			start, size = i+1, 0
		}
	}
	emit(start, len(b.Lines))
	return out
}

// isTOCLine reports dot-leader lines like "Pin and Unpin . . . . . 241".
func isTOCLine(l string) bool {
	return strings.Contains(l, ". . . .") || strings.Contains(l, ".....") || strings.Contains(l, "…………")
}

// Hit is one scored chunk.
type Hit struct {
	Book       *Book
	Start, End int // line range
	Score      float64
	Matched    int // number of distinct query terms present
}

// scoreAll returns BM25 scores, weighted by query-term coverage, per chunk.
func (x *Index) scoreAll(terms []string, allow func(*Book) bool) (map[uint32]float64, map[uint32]int) {
	scores := map[uint32]float64{}
	matched := map[uint32]int{}
	N := float64(len(x.chunks))
	allowed := make([]bool, len(x.books))
	for i, b := range x.books {
		allowed[i] = allow == nil || allow(b)
	}
	for _, t := range terms {
		ps := x.postings[t]
		if len(ps) == 0 {
			continue
		}
		df := float64(len(ps))
		idf := math.Log(1 + (N-df+0.5)/(df+0.5))
		for _, p := range ps {
			c := x.chunks[p.chunk]
			if !allowed[c.book] {
				continue
			}
			f := float64(p.tf)
			norm := bm25K1 * (1 - bm25B + bm25B*float64(c.length)/x.avgLen)
			scores[p.chunk] += idf * f * (bm25K1 + 1) / (f + norm)
			matched[p.chunk]++
		}
	}
	n := float64(len(terms))
	for id, s := range scores {
		scores[id] = s * (0.25 + 0.75*float64(matched[id])/n)
	}
	return scores, matched
}

// Search returns the best chunks for query, best first.
func (x *Index) Search(query string, allow func(*Book) bool, limit int) []Hit {
	terms := Terms(query)
	if len(terms) == 0 {
		return nil
	}
	scores, matched := x.scoreAll(terms, allow)
	ids := make([]uint32, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return scores[ids[i]] > scores[ids[j]] })

	// Exact phrase bonus on the leading candidates.
	phrase := strings.ToLower(strings.Join(strings.Fields(query), " "))
	if len(terms) > 1 && len(phrase) > 3 {
		top := ids[:min(len(ids), 400)]
		for _, id := range top {
			c := x.chunks[id]
			b := x.books[c.book]
			text := strings.ToLower(strings.Join(b.Lines[c.start:c.end], " "))
			if strings.Contains(text, phrase) {
				scores[id] *= 1.6
			}
		}
		sort.SliceStable(top, func(i, j int) bool { return scores[top[i]] > scores[top[j]] })
	}

	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	hits := make([]Hit, len(ids))
	for i, id := range ids {
		c := x.chunks[id]
		hits[i] = Hit{Book: x.books[c.book], Start: int(c.start), End: int(c.end), Score: scores[id], Matched: matched[id]}
	}
	return hits
}

// BookScore ranks a book's relevance to a query.
type BookScore struct {
	Book     *Book
	Score    float64
	Chunks   int   // matching chunks
	TopHits  []Hit // best chunks inside this book
	TitleHit bool
	Matched  []string // query words found in this book's best passages
}

// RankBooks scores books by their best passages plus a title/category bonus.
func (x *Index) RankBooks(query string, allow func(*Book) bool, limit, hitsPerBook int) []BookScore {
	terms := Terms(query)
	if len(terms) == 0 {
		return nil
	}
	scores, matched := x.scoreAll(terms, allow)
	per := map[int32][]uint32{}
	best := 0.0
	for id, s := range scores {
		c := x.chunks[id]
		per[c.book] = append(per[c.book], id)
		best = max(best, s)
	}
	var out []BookScore
	for bi, ids := range per {
		sort.Slice(ids, func(i, j int) bool { return scores[ids[i]] > scores[ids[j]] })
		b := x.books[bi]
		weights := []float64{1, 0.5, 0.35, 0.25, 0.2, 0.15, 0.1, 0.1}
		var s float64
		for i, id := range ids {
			if i >= len(weights) {
				break
			}
			s += weights[i] * scores[id]
		}
		bs := BookScore{Book: b, Chunks: len(ids)}
		hay := map[string]bool{}
		tokenize(b.Title+" "+b.Category, func(t string) { hay[t] = true })
		hit := 0
		for _, t := range terms {
			if hay[t] {
				hit++
			}
		}
		if hit > 0 {
			s += best * 0.8 * float64(hit) / float64(len(terms))
			bs.TitleHit = true
		}
		bs.Score = s
		bs.Matched = matchedWords(query, x.books[bi], ids[:min(len(ids), 8)], x)
		for i, id := range ids {
			if i >= hitsPerBook {
				break
			}
			c := x.chunks[id]
			bs.TopHits = append(bs.TopHits, Hit{Book: b, Start: int(c.start), End: int(c.end), Score: scores[id], Matched: matched[id]})
		}
		out = append(out, bs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Snippet returns the most query-relevant line(s) of a hit, cleaned.
func Snippet(b *Book, start, end int, query string, maxLen int) (string, int) {
	terms := map[string]bool{}
	for _, t := range Terms(query) {
		terms[t] = true
	}
	bestLine, bestScore := start, -1
	for i := start; i < end && i < len(b.Lines); i++ {
		l := b.Lines[i]
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		seen := map[string]bool{}
		tokenize(l, func(t string) {
			if terms[t] {
				seen[t] = true
			}
		})
		if len(seen) > bestScore {
			bestLine, bestScore = i, len(seen)
		}
	}
	var sb strings.Builder
	for i := bestLine; i < end && i < len(b.Lines) && sb.Len() < maxLen; i++ {
		l := cleanInline(b.Lines[i])
		if l == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString(" ")
		}
		sb.WriteString(l)
	}
	return truncate(sb.String(), maxLen), bestLine
}

// Search is the library-level full-text search.
func (l *Library) Search(query string, allow func(*Book) bool, limit int) []Hit {
	return l.index.Search(query, allow, limit)
}

// RankBooks is the library-level book recommendation.
func (l *Library) RankBooks(query string, allow func(*Book) bool, limit, hitsPerBook int) []BookScore {
	return l.index.RankBooks(query, allow, limit, hitsPerBook)
}

// Stats reports index size.
func (l *Library) Stats() (chunks, terms int) {
	return len(l.index.chunks), len(l.index.postings)
}

// matchedWords returns the query words found in the given chunks.
func matchedWords(query string, b *Book, ids []uint32, x *Index) []string {
	present := map[string]bool{}
	for _, id := range ids {
		c := x.chunks[id]
		if s := b.SectionAt(int(c.start)); s != nil {
			tokenize(s.Title, func(t string) { present[t] = true })
		}
		for _, l := range b.Lines[c.start:c.end] {
			tokenize(l, func(t string) { present[t] = true })
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, w := range strings.Fields(query) {
		w = strings.Trim(w, ".,;:!?()[]\"'")
		ok := false
		tokenize(w, func(t string) { ok = ok || present[t] })
		lw := strings.ToLower(w)
		if ok && !seen[lw] {
			seen[lw] = true
			out = append(out, w)
		}
	}
	return out
}
