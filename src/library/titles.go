package library

import (
	"math"
	"sort"
	"strings"
)

// titleIndex lets agents search chapter and section titles across all books.
type titleIndex struct {
	terms [][][]string // [book][section] -> distinct title terms
	idf   map[string]float64
}

func buildTitleIndex(books []*Book) *titleIndex {
	ti := &titleIndex{terms: make([][][]string, len(books)), idf: map[string]float64{}}
	df := map[string]int{}
	total := 0
	for bi, b := range books {
		ti.terms[bi] = make([][]string, len(b.Sections))
		for si, s := range b.Sections {
			t := Terms(s.Title)
			ti.terms[bi][si] = t
			for _, w := range t {
				df[w]++
			}
			total++
		}
	}
	for w, n := range df {
		ti.idf[w] = math.Log(1 + float64(total)/float64(n))
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
	q := Terms(query)
	if len(q) == 0 {
		return nil
	}
	var want float64
	for _, w := range q {
		want += l.titles.idf[w]
	}
	if want == 0 {
		return nil
	}
	phrase := strings.ToLower(strings.Join(strings.Fields(query), " "))
	var hits []SectionHit
	for bi, b := range l.Books {
		if b.Status != StatusOK || (allow != nil && !allow(b)) {
			continue
		}
		for si, terms := range l.titles.terms[bi] {
			var got float64
			for _, w := range q {
				for _, t := range terms {
					if t == w {
						got += l.titles.idf[w]
						break
					}
				}
			}
			cover := got / want
			if cover < 0.5 {
				continue
			}
			s := &b.Sections[si]
			score := 10 * cover
			if len(q) > 1 && strings.Contains(strings.ToLower(s.Title), phrase) {
				score += 3
			}
			// prefer chapters and substantial sections, in proportion to the match
			score += cover * (math.Max(0, 1.5-0.75*float64(s.Rank)) + math.Min(1, float64(s.Chars)/8000))
			hits = append(hits, SectionHit{Book: b, Section: s, Score: score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}
