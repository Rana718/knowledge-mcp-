package library

import (
	"encoding/binary"
	"math"
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
}

// Index is a BM25 inverted index over book chunks. Posting lists are stored
// back to back in one byte slice as (varint chunk delta, tf byte) pairs, so
// the whole index holds no pointers except the term dictionary.
type Index struct {
	books  []*Book
	chunks []chunk
	norm   []float32 // per chunk: k1 * (1 - b + b*len/avgLen)
	dict   map[string]uint32
	df     []uint32
	off    []uint32 // postings of term t are data[off[t]:off[t+1]]
	data   []byte
	titles [][]string // per book: title and category terms
}

// bookIndex is the per-book result of indexing, merged into Index later.
type bookIndex struct {
	chunks  []chunk
	lengths []uint32
	terms   []string // local term id -> term
	termOff []uint32 // postings of local term t: post[termOff[t]:termOff[t+1]]
	post    []localPosting
}

type localPosting struct {
	chunk uint32 // chunk id inside the book
	tf    uint8
}

func buildIndex(books []*Book, workers int) *Index {
	parts := make([]bookIndex, len(books))
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
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

	x := &Index{books: books, dict: map[string]uint32{}}
	nChunks, nLocalTerms := 0, 0
	for _, p := range parts {
		nChunks += len(p.chunks)
		nLocalTerms += len(p.terms)
	}

	// Global term ids, and per term: number of postings and encoded size.
	x.chunks = make([]chunk, 0, nChunks)
	lengths := make([]uint32, 0, nChunks)
	gids := make([][]uint32, len(parts))
	var size []uint32
	last := []int64{}
	var tmp [binary.MaxVarintLen32]byte
	for bi, p := range parts {
		base := uint32(len(x.chunks))
		x.chunks = append(x.chunks, p.chunks...)
		lengths = append(lengths, p.lengths...)
		gids[bi] = make([]uint32, len(p.terms))
		for lt, t := range p.terms {
			g, ok := x.dict[t]
			if !ok {
				g = uint32(len(x.df))
				x.dict[t] = g
				x.df = append(x.df, 0)
				size = append(size, 0)
				last = append(last, -1)
			}
			gids[bi][lt] = g
			for _, q := range p.post[p.termOff[lt]:p.termOff[lt+1]] {
				c := int64(base + q.chunk)
				size[g] += uint32(binary.PutUvarint(tmp[:], uint64(c-last[g]-1))) + 1
				last[g] = c
				x.df[g]++
			}
		}
	}

	// Lay out and encode all posting lists in one allocation.
	x.off = make([]uint32, len(x.df)+1)
	for g, n := range size {
		x.off[g+1] = x.off[g] + n
	}
	x.data = make([]byte, x.off[len(x.df)])
	pos := make([]uint32, len(x.df))
	copy(pos, x.off)
	for g := range last {
		last[g] = -1
	}
	base := uint32(0)
	for bi, p := range parts {
		for lt := range p.terms {
			g := gids[bi][lt]
			for _, q := range p.post[p.termOff[lt]:p.termOff[lt+1]] {
				c := int64(base + q.chunk)
				pos[g] += uint32(binary.PutUvarint(x.data[pos[g]:], uint64(c-last[g]-1)))
				x.data[pos[g]] = q.tf
				pos[g]++
				last[g] = c
			}
		}
		base += uint32(len(p.chunks))
		parts[bi] = bookIndex{} // release as we go
	}

	var total float64
	for _, n := range lengths {
		total += float64(n)
	}
	avg := max(1, total/float64(max(1, len(lengths))))
	x.norm = make([]float32, len(lengths))
	for i, n := range lengths {
		x.norm[i] = float32(bm25K1 * (1 - bm25B + bm25B*float64(n)/avg))
	}
	x.titles = make([][]string, len(books))
	for i, b := range books {
		x.titles[i] = Terms(b.Title + " " + b.Category)
	}
	return x
}

func indexBook(bi int32, b *Book) bookIndex {
	var out bookIndex
	if b.Status == StatusNoText {
		return out
	}
	isStart := make(map[int]bool, len(b.Sections))
	for _, s := range b.Sections {
		isStart[s.Start] = true
	}
	// Capacity hints from the book size (measured over this library: about
	// one posting per 18 bytes of text, one distinct term per 250 bytes)
	// avoid repeated slice growth.
	textLen := b.Lines.Bytes(b.BodyStart, b.Lines.Len())
	nTerms := textLen/250 + 64
	// stop words share the dictionary lookup, marked with stopID
	const stopID = ^uint32(0)
	dict := make(map[string]uint32, nTerms+len(stopwords))
	for w := range stopwords {
		dict[stem(w)] = stopID
	}
	out.terms = make([]string, 0, nTerms)
	out.chunks = make([]chunk, 0, textLen/1500+8)
	out.lengths = make([]uint32, 0, textLen/1500+8)
	var (
		count   = make([]uint16, 0, nTerms) // per local term, for the current chunk
		touched = make([]uint32, 0, 512)
		flat    = make([]struct{ term, chunk uint32 }, 0, textLen/18+64)
		tfs     = make([]uint8, 0, textLen/18+64)
		buf     []byte
		n       uint32
	)
	add := func(t string) {
		id, ok := dict[t]
		if id == stopID && ok {
			return
		}
		if !ok {
			id = uint32(len(out.terms))
			t = strings.Clone(t)
			dict[t] = id
			out.terms = append(out.terms, t)
			count = append(count, 0)
		}
		if count[id] == 0 {
			touched = append(touched, id)
		}
		if count[id] < math.MaxUint16 {
			count[id]++
		}
		n++
	}
	emit := func(start, end int) {
		if end <= start || start >= b.IndexAt {
			return
		}
		n, touched = 0, touched[:0]
		// include section and parent titles so passages match their topic
		if s := b.SectionAt(start); s != nil {
			buf = scan(s.Title, buf, false, add)
			if s.Parent >= 0 {
				buf = scan(b.Sections[s.Parent].Title, buf, false, add)
			}
		}
		for i := start; i < end; i++ {
			if l := b.Lines.At(i); !isTOCLine(l) { // printed contents pages only add noise
				buf = scan(l, buf, false, add)
			}
		}
		if n == 0 {
			return
		}
		cid := uint32(len(out.chunks))
		out.chunks = append(out.chunks, chunk{book: bi, start: int32(start), end: int32(end)})
		out.lengths = append(out.lengths, n)
		for _, id := range touched {
			flat = append(flat, struct{ term, chunk uint32 }{id, cid})
			tfs = append(tfs, uint8(min(count[id], math.MaxUint8)))
			count[id] = 0
		}
	}

	start, size := b.BodyStart, 0
	for i := b.BodyStart; i < b.Lines.Len(); i++ {
		if isStart[i] && i > start {
			emit(start, i)
			start, size = i, 0
		}
		l := b.Lines.At(i)
		size += len(l) + 1
		if size >= chunkHardBytes || (size >= chunkSoftBytes && strings.TrimSpace(l) == "") {
			emit(start, i+1)
			start, size = i+1, 0
		}
	}
	emit(start, b.Lines.Len())

	// Group postings by term (counting sort keeps chunk order within a term).
	out.termOff = make([]uint32, len(out.terms)+1)
	for _, f := range flat {
		out.termOff[f.term+1]++
	}
	for t := range out.terms {
		out.termOff[t+1] += out.termOff[t]
	}
	out.post = make([]localPosting, len(flat))
	next := make([]uint32, len(out.terms))
	copy(next, out.termOff)
	for i, f := range flat {
		out.post[next[f.term]] = localPosting{chunk: f.chunk, tf: tfs[i]}
		next[f.term]++
	}
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

// scratch holds per-query score buffers; reused through a sync.Pool so a
// query allocates almost nothing.
type scratch struct {
	score   []float32
	hits    []uint8
	touched []uint32
	allowed []bool
	buf     []byte
}

var scratchPool sync.Pool

func (x *Index) getScratch() *scratch {
	s, _ := scratchPool.Get().(*scratch)
	if s == nil || len(s.score) != len(x.chunks) {
		s = &scratch{score: make([]float32, len(x.chunks)), hits: make([]uint8, len(x.chunks))}
	}
	return s
}

func putScratch(s *scratch) {
	for _, id := range s.touched {
		s.score[id], s.hits[id] = 0, 0
	}
	s.touched = s.touched[:0]
	scratchPool.Put(s)
}

// score fills s with BM25 scores, weighted by query-term coverage.
func (x *Index) score(terms []string, allow func(*Book) bool, s *scratch) {
	s.allowed = s.allowed[:0]
	for _, b := range x.books {
		s.allowed = append(s.allowed, allow == nil || allow(b))
	}
	N := float64(len(x.chunks))
	for _, t := range terms {
		g, ok := x.dict[t]
		if !ok {
			continue
		}
		df := float64(x.df[g])
		idf := float32(math.Log(1 + (N-df+0.5)/(df+0.5)))
		data := x.data[x.off[g]:x.off[g+1]]
		c := int64(-1)
		for len(data) > 0 {
			d, k := binary.Uvarint(data)
			c += int64(d) + 1
			f := float32(data[k])
			data = data[k+1:]
			id := uint32(c)
			if !s.allowed[x.chunks[id].book] {
				continue
			}
			if s.hits[id] == 0 {
				s.touched = append(s.touched, id)
			}
			s.score[id] += idf * f * (bm25K1 + 1) / (f + x.norm[id])
			s.hits[id]++
		}
	}
	n := float32(len(terms))
	for _, id := range s.touched {
		s.score[id] *= 0.25 + 0.75*float32(s.hits[id])/n
	}
}

// topK returns the ids of the k best-scored touched chunks, best first.
func topK(s *scratch, k int) []uint32 {
	if k <= 0 || len(s.touched) <= k {
		ids := append([]uint32(nil), s.touched...)
		sort.Slice(ids, func(i, j int) bool { return s.score[ids[i]] > s.score[ids[j]] })
		return ids
	}
	// min-heap of size k on score
	h := make([]uint32, 0, k)
	less := func(a, b uint32) bool { return s.score[a] < s.score[b] }
	down := func(i int) {
		for {
			l := 2*i + 1
			if l >= len(h) {
				return
			}
			m := l
			if r := l + 1; r < len(h) && less(h[r], h[l]) {
				m = r
			}
			if !less(h[m], h[i]) {
				return
			}
			h[i], h[m] = h[m], h[i]
			i = m
		}
	}
	for _, id := range s.touched {
		if len(h) < k {
			h = append(h, id)
			for i := len(h) - 1; i > 0; {
				p := (i - 1) / 2
				if !less(h[i], h[p]) {
					break
				}
				h[i], h[p] = h[p], h[i]
				i = p
			}
		} else if less(h[0], id) {
			h[0] = id
			down(0)
		}
	}
	sort.Slice(h, func(i, j int) bool { return s.score[h[i]] > s.score[h[j]] })
	return h
}

// Search returns the best chunks for query, best first.
func (x *Index) Search(query string, allow func(*Book) bool, limit int) []Hit {
	terms := Terms(query)
	if len(terms) == 0 {
		return nil
	}
	s := x.getScratch()
	defer putScratch(s)
	x.score(terms, allow, s)

	phrase := strings.Join(strings.Fields(query), " ")
	find, usePhrase := LiteralFinder(phrase, true)
	usePhrase = usePhrase && len(terms) > 1 && len(phrase) > 3
	k := limit
	if usePhrase {
		k = max(limit, 200)
	}
	ids := topK(s, k)
	if usePhrase {
		for _, id := range ids {
			c := x.chunks[id]
			if find(x.books[c.book].Lines.Span(int(c.start), int(c.end))) != nil {
				s.score[id] *= 1.6
			}
		}
		sort.SliceStable(ids, func(i, j int) bool { return s.score[ids[i]] > s.score[ids[j]] })
	}
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	hits := make([]Hit, len(ids))
	for i, id := range ids {
		c := x.chunks[id]
		hits[i] = Hit{Book: x.books[c.book], Start: int(c.start), End: int(c.end), Score: float64(s.score[id]), Matched: int(s.hits[id])}
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

const bookTop = 8

var bookWeights = [bookTop]float32{1, 0.5, 0.35, 0.25, 0.2, 0.15, 0.1, 0.1}

// RankBooks scores books by their best passages plus a title/category bonus.
func (x *Index) RankBooks(query string, allow func(*Book) bool, limit, hitsPerBook int) []BookScore {
	terms := Terms(query)
	if len(terms) == 0 {
		return nil
	}
	s := x.getScratch()
	defer putScratch(s)
	x.score(terms, allow, s)

	// best bookTop chunks per book, kept sorted by insertion
	top := make([][bookTop]uint32, len(x.books))
	cnt := make([]int, len(x.books))
	var best float32
	for _, id := range s.touched {
		bi := x.chunks[id].book
		sc := s.score[id]
		best = max(best, sc)
		n := min(cnt[bi], bookTop)
		cnt[bi]++
		j := n
		for j > 0 && s.score[top[bi][j-1]] < sc {
			if j < bookTop {
				top[bi][j] = top[bi][j-1]
			}
			j--
		}
		if j < bookTop {
			top[bi][j] = id
		}
	}

	var out []BookScore
	for bi, n := range cnt {
		if n == 0 {
			continue
		}
		var sc float32
		for i := 0; i < min(n, bookTop); i++ {
			sc += bookWeights[i] * s.score[top[bi][i]]
		}
		bs := BookScore{Book: x.books[bi], Chunks: n}
		hit := 0
		for _, t := range terms {
			for _, tt := range x.titles[bi] {
				if t == tt {
					hit++
					break
				}
			}
		}
		if hit > 0 {
			sc += best * 0.8 * float32(hit) / float32(len(terms))
			bs.TitleHit = true
		}
		bs.Score = float64(sc)
		out = append(out, bs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	// details only for the books we return
	for i := range out {
		b := out[i].Book
		bi := bookIndexOf(x.books, b)
		ids := top[bi][:min(cnt[bi], bookTop)]
		out[i].Matched = matchedWords(query, terms, b, ids, x, s)
		for j, id := range ids {
			if j >= hitsPerBook {
				break
			}
			c := x.chunks[id]
			out[i].TopHits = append(out[i].TopHits, Hit{Book: b, Start: int(c.start), End: int(c.end), Score: float64(s.score[id]), Matched: int(s.hits[id])})
		}
	}
	return out
}

func bookIndexOf(books []*Book, b *Book) int {
	for i, x := range books {
		if x == b {
			return i
		}
	}
	return -1
}

// matchedWords returns the query words found in the given chunks.
func matchedWords(query string, terms []string, b *Book, ids []uint32, x *Index, s *scratch) []string {
	found := make([]bool, len(terms))
	mark := func(t string) {
		for i, q := range terms {
			if q == t {
				found[i] = true
			}
		}
	}
	for _, id := range ids {
		c := x.chunks[id]
		if sec := b.SectionAt(int(c.start)); sec != nil {
			s.buf = scanTerms(sec.Title, s.buf, mark)
		}
		s.buf = scanTerms(b.Lines.Span(int(c.start), int(c.end)), s.buf, mark)
	}
	present := map[string]bool{}
	for i, t := range terms {
		if found[i] {
			present[t] = true
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, w := range strings.Fields(query) {
		w = strings.Trim(w, ".,;:!?()[]\"'")
		ok := false
		tokenize(w, func(t string) { ok = ok || present[t] })
		if lw := strings.ToLower(w); ok && !seen[lw] {
			seen[lw] = true
			out = append(out, w)
		}
	}
	return out
}

// Snippet returns the most query-relevant line(s) of a hit, cleaned.
func Snippet(b *Book, start, end int, query string, maxLen int) (string, int) {
	terms := Terms(query)
	end = min(end, b.Lines.Len())
	bestLine, bestScore := start, -1
	var buf []byte
	for i := start; i < end; i++ {
		l := b.Lines.At(i)
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		var seen uint64
		buf = scanTerms(l, buf, func(t string) {
			for k, q := range terms {
				if q == t && k < 64 {
					seen |= 1 << k
				}
			}
		})
		if n := popcount(seen); n > bestScore {
			bestLine, bestScore = i, n
		}
	}
	var sb strings.Builder
	for i := bestLine; i < end && sb.Len() < maxLen; i++ {
		l := cleanInline(b.Lines.At(i))
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

func popcount(v uint64) int {
	n := 0
	for ; v != 0; v &= v - 1 {
		n++
	}
	return n
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
	return len(l.index.chunks), len(l.index.df)
}
