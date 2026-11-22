package library

import (
	"math"
	"sort"
	"strings"
)

// titleIndex is an inverted index over chapter and section titles: for each
// title word, the sections whose title contains it (no pointers).
type titleIndex struct {
	dict  map[string]uint32
	idf   []float64
	posts [][]secRef // per term id
}

type secRef struct{ book, sec uint32 }

func buildTitleIndex(books []*Book) *titleIndex {
	ti := &titleIndex{dict: map[string]uint32{}}
	total := 0
	for bi, b := range books {
		for si, s := range b.Sections {
			total++
			for _, t := range Terms(s.Title) {
				g, ok := ti.dict[t]
				if !ok {
					g = uint32(len(ti.posts))
					ti.dict[t] = g
					ti.posts = append(ti.posts, nil)
				}
				ti.posts[g] = append(ti.posts[g], secRef{uint32(bi), uint32(si)})
			}
		}
	}
	ti.idf = make([]float64, len(ti.posts))
	for g, p := range ti.posts {
		ti.idf[g] = math.Log(1 + float64(total)/float64(len(p)))
		ti.posts[g] = p[:len(p):len(p)]
	}
	return ti
}

// SectionHit is a section whose title matches a query.
type SectionHit struct {
	Book    *Book
	Section *Section
	Score   float64
}

// FindSections ranks section titles by query match (rare words count more),
// preferring whole chapters and substantial sections.
func (l *Library) FindSections(query string, allow func(*Book) bool, limit int) []SectionHit {
	ti := l.titles
	terms := Terms(query)
	var want float64
	got := map[secRef]float64{}
	for _, t := range terms {
		g, ok := ti.dict[t]
		if !ok {
			want += math.Log(1 + float64(len(ti.idf))) // unknown word still counts
			continue
		}
		want += ti.idf[g]
		for _, r := range ti.posts[g] {
			got[r] += ti.idf[g]
		}
	}
	if len(got) == 0 {
		return nil
	}
	phrase := strings.ToLower(strings.Join(strings.Fields(query), " "))
	var hits []SectionHit
	for r, g := range got {
		cover := g / want
		if cover < 0.5 {
			continue
		}
		b := l.Books[r.book]
		if b.Status != StatusOK || (allow != nil && !allow(b)) {
			continue
		}
		s := &b.Sections[r.sec]
		score := 10 * cover
		if len(terms) > 1 && strings.Contains(strings.ToLower(s.Title), phrase) {
			score += 3
		}
		// prefer chapters and substantial sections, in proportion to the match
		score += cover * (math.Max(0, 1.5-0.75*float64(s.Rank)) + math.Min(1, float64(s.Chars)/8000))
		hits = append(hits, SectionHit{Book: b, Section: s, Score: score})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if hits[i].Book != hits[j].Book {
			return hits[i].Book.ID < hits[j].Book.ID
		}
		return hits[i].Section.ID < hits[j].Section.ID
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}
