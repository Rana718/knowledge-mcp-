package library

import (
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
)

// Finder returns the [start, end) of the first match in s, or nil.
type Finder func(s string) []int

// RegexFinder matches a regular expression (compile with (?m) for per-line ^
// and $). When the regexp contains a required literal (e.g. "sync." in
// `sync\.(Wait|Mutex)`), the literal is located first with LiteralFinder and
// the regexp only runs on those lines, which is much faster than scanning
// the whole text with the regexp engine.
func RegexFinder(re *regexp.Regexp) Finder {
	lit, fold := requiredLiteral(re.String())
	pre, ok := LiteralFinder(lit, fold)
	if len(lit) < 3 || !ok {
		return re.FindStringIndex
	}
	return func(s string) []int {
		for pos := 0; pos < len(s); {
			loc := pre(s[pos:])
			if loc == nil {
				return nil
			}
			at := pos + loc[0]
			ls := strings.LastIndexByte(s[:at], '\n') + 1
			le := strings.IndexByte(s[at:], '\n')
			if le < 0 {
				le = len(s)
			} else {
				le += at
			}
			if m := re.FindStringIndex(s[ls:le]); m != nil {
				return []int{ls + m[0], ls + m[1]}
			}
			pos = le + 1
		}
		return nil
	}
}

// requiredLiteral returns the longest literal every match must contain, if
// the pattern is a plain concatenation (alternations make it optional).
func requiredLiteral(expr string) (lit string, fold bool) {
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return "", false
	}
	re = re.Simplify()
	for re.Op == syntax.OpCapture && len(re.Sub) == 1 {
		re = re.Sub[0]
	}
	parts := []*syntax.Regexp{re}
	if re.Op == syntax.OpConcat {
		parts = re.Sub
	}
	for _, p := range parts {
		if p.Op == syntax.OpLiteral && len(p.Rune) > len(lit) {
			lit, fold = string(p.Rune), p.Flags&syntax.FoldCase != 0
		}
	}
	return lit, fold
}

// LiteralFinder matches plain text. Case-insensitive search jumps between
// occurrences of the pattern's rarest byte with IndexByte (vectorized) and
// compares the window with EqualFold, which is far faster than a (?i) regexp.
// Only ASCII patterns are supported; ok is false otherwise.
func LiteralFinder(pat string, fold bool) (f Finder, ok bool) {
	for i := 0; i < len(pat); i++ {
		if pat[i] >= 0x80 {
			return nil, false
		}
	}
	n := len(pat)
	if n == 0 {
		return nil, false
	}
	if !fold {
		return func(s string) []int {
			if i := strings.Index(s, pat); i >= 0 {
				return []int{i, i + n}
			}
			return nil
		}, true
	}
	k := rarestByte(pat)
	lo, up := lower(pat[k]), upper(pat[k])
	return func(s string) []int {
		nl, nu := -2, -2 // next occurrence of each case variant; -1 = none left
		for from := k; ; {
			if nl != -1 && nl < from {
				nl = indexFrom(s, lo, from)
			}
			if up != lo && nu != -1 && nu < from {
				nu = indexFrom(s, up, from)
			}
			i := nl
			if up != lo && nu >= 0 && (i < 0 || nu < i) {
				i = nu
			}
			if i < 0 {
				return nil
			}
			if st := i - k; st+n <= len(s) && strings.EqualFold(s[st:st+n], pat) {
				return []int{st, st + n}
			}
			from = i + 1
		}
	}, true
}

func indexFrom(s string, c byte, from int) int {
	if from >= len(s) {
		return -1
	}
	if j := strings.IndexByte(s[from:], c); j >= 0 {
		return from + j
	}
	return -1
}

// byteFreq lists printable bytes (lower-cased) from most to least frequent,
// measured over the book library.
const byteFreq = " etaiosnrlchdupmf.gbyw*,_v-k1x`)(2#>0<|:/345q6=879;j\"z{'}[]+!?~$\\&%@^"

// rarestByte picks the pattern byte that occurs least often in the books.
func rarestByte(p string) int {
	best, bestRank := 0, -1
	for i := 0; i < len(p); i++ {
		r := strings.IndexByte(byteFreq, lower(p[i]))
		if r < 0 {
			r = len(byteFreq) // not in the table: rare
		}
		if r > bestRank {
			best, bestRank = i, r
		}
	}
	return best
}

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

func upper(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - 32
	}
	return c
}

// GrepMatch is one matching line.
type GrepMatch struct {
	Line int    // 0-based line number
	Text string // the line (a section title for heading matches)
	At   int    // byte offset of the match in Text
}

// Grep returns up to limit matching lines of b in line order. Headings also
// match on their cleaned title, so "^Chapter 5" finds "## **Chapter 5**".
func (b *Book) Grep(find Finder, limit int) []GrepMatch {
	var out []GrepMatch
	seen := map[int]bool{}
	for _, s := range b.Sections {
		if loc := find(s.Title); loc != nil {
			out = append(out, GrepMatch{Line: s.Start, Text: s.Title, At: loc[0]})
			seen[s.Start] = true
		}
	}
	headings := len(out)
	base := b.Lines.Offset(b.BodyStart)
	body := b.Lines.Span(b.BodyStart, b.Lines.Len())
	for pos := 0; pos < len(body) && len(out)-headings < limit; {
		loc := find(body[pos:])
		if loc == nil {
			break
		}
		line := b.Lines.LineAt(base + pos + loc[0])
		if !seen[line] {
			seen[line] = true
			out = append(out, GrepMatch{Line: line, Text: b.Lines.At(line), At: base + pos + loc[0] - b.Lines.Offset(line)})
		}
		pos = max(pos+1, b.Lines.Offset(line+1)-base) // continue on the next line
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
