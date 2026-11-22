package library

import (
	"sort"
	"strings"
)

// Lines is a book's text plus the start offset of every line. Lines are
// substrings of the text, so nothing is copied (4 bytes per line instead of
// a 16-byte string header).
type Lines struct {
	text string
	off  []uint32 // off[i] = start of line i; off[Len()] = len(text)+1
}

func newLines(text string) Lines {
	off := make([]uint32, 1, strings.Count(text, "\n")+2)
	for i := 0; ; {
		j := strings.IndexByte(text[i:], '\n')
		if j < 0 {
			break
		}
		i += j + 1
		off = append(off, uint32(i))
	}
	return Lines{text: text, off: append(off, uint32(len(text)+1))}
}

// Len returns the number of lines.
func (l Lines) Len() int { return len(l.off) - 1 }

// At returns line i without its newline.
func (l Lines) At(i int) string { return l.text[l.off[i] : l.off[i+1]-1] }

// Span returns lines [a, b) joined by newlines, without copying.
func (l Lines) Span(a, b int) string {
	if a >= b {
		return ""
	}
	return l.text[l.off[a] : l.off[b]-1]
}

// Bytes returns the size of lines [a, b) including newlines.
func (l Lines) Bytes(a, b int) int { return int(l.off[b] - l.off[a]) }

// LineAt returns the line containing byte offset pos of the text.
func (l Lines) LineAt(pos int) int {
	return sort.Search(len(l.off)-1, func(i int) bool { return int(l.off[i+1]) > pos })
}

// Offset returns the byte offset where line i starts.
func (l Lines) Offset(i int) int { return int(l.off[i]) }
