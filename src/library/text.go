package library

import (
	"math"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// stopwords are dropped from both the index and queries.
var stopwords = func() map[string]struct{} {
	words := `a about above after again against all am an and any are as at be because been before being
below between both but by can could did do does doing down during each few for from further had has have
having he her here hers herself him himself his how i if in into is it its itself just me more most my
myself no nor not now of off on once only or other our ours ourselves out over own same she should so
some such than that the their theirs them themselves then there these they this those through to too
under until up very was we were what when where which while who whom why will with would you your yours
yourself yourselves also may might must shall us use used using one two get got let like via etc eg ie`
	m := map[string]struct{}{}
	for _, w := range strings.Fields(words) {
		m[w] = struct{}{}
	}
	return m
}()

// stem strips common suffixes so plural and verb forms match.
func stem(w string) string {
	n := len(w)
	switch {
	case n > 4 && strings.HasSuffix(w, "ies"):
		return w[:n-3] + "y"
	case n > 5 && strings.HasSuffix(w, "ing"):
		return w[:n-3]
	case n > 4 && strings.HasSuffix(w, "ed") && !strings.HasSuffix(w, "eed"):
		return w[:n-2]
	case n > 4 && (strings.HasSuffix(w, "sses") || strings.HasSuffix(w, "xes") || strings.HasSuffix(w, "ches") || strings.HasSuffix(w, "shes")):
		return w[:n-2]
	case n > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && !strings.HasSuffix(w, "us") && !strings.HasSuffix(w, "is"):
		return w[:n-1]
	}
	return w
}

// tokenize calls fn for each normalized term of s.
func tokenize(s string, fn func(term string)) {
	var buf []byte
	flush := func() {
		if len(buf) == 0 {
			return
		}
		t := strings.Trim(string(buf), "_")
		buf = buf[:0]
		if len(t) < 2 || len(t) > 40 {
			return
		}
		if _, ok := stopwords[t]; ok {
			return
		}
		fn(stem(t))
	}
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
				buf = append(buf, c)
			case c >= 'A' && c <= 'Z':
				buf = append(buf, c+32)
			case c == '+' && len(buf) > 0 && (buf[len(buf)-1] == 'c' || buf[len(buf)-1] == '+'):
				buf = append(buf, c) // c++
			case c == '#' && len(buf) == 1 && (buf[0] == 'c' || buf[0] == 'f'):
				buf = append(buf, c) // c#, f#
			default:
				flush()
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			buf = utf8.AppendRune(buf, unicode.ToLower(r))
		} else {
			flush()
		}
		i += size
	}
	flush()
}

// Terms returns the normalized, de-duplicated query terms of s.
func Terms(s string) []string {
	seen := map[string]bool{}
	var out []string
	tokenize(s, func(t string) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	})
	return out
}

var (
	reHTMLTag     = regexp.MustCompile(`</?(u|b|i|em|strong|sup|sub|br|mark|span|code|small|kbd|s)\s*/?>`)
	reSpaces      = regexp.MustCompile(`\s+`)
	reHeading     = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#*\s*$`)
	reOnlyNums    = regexp.MustCompile(`^[\divxlcdmIVXLCDM.\-–\s]+$`)
	reCaption     = regexp.MustCompile(`(?i)^(example|listing|figure|fig\.|table|exhibit|code|output|snippet)\s*\d`)
	reChapter     = regexp.MustCompile(`(?i)^(chapter|appendix|lecture|unit|module|lesson)\s+([0-9]+|[ivxlc]+|[a-z]\b|one|two|three|four|five|six|seven|eight|nine|ten)\b`)
	reShellCmd    = regexp.MustCompile(`^[a-z][a-z0-9_.+-]*\s+(-{1,2}[a-zA-Z]|/|\./|~|\*)`)
	reBareChapter = regexp.MustCompile(`(?i)^(chapter|part|appendix)\s+([0-9]+|[ivxlc]+|[a-z])\.?$`)
	reSubOne      = regexp.MustCompile(`^(\d{1,2})\.1(\s|$)`)
	rePart        = regexp.MustCompile(`(?i)^(part|book|volume)\s+([0-9]+|[ivxlc]+|one|two|three|four|five|six|seven|eight|nine|ten)\b`)
	noiseTitles   = map[string]bool{
		"note": true, "notes": true, "tip": true, "tips": true, "warning": true, "caution": true,
		"important": true, "hint": true, "info": true, "sidebar": true, "danger": true, "aside": true,
		"tip:": true, "note:": true, "warning:": true, "important:": true, "try it": true,
	}
)

var shellCommands = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range strings.Fields(`chown chmod chgrp ls cd mkdir rmdir cat echo sudo su mount umount kill killall ps cp mv rm ln
touch find grep egrep export make gcc cc g++ clang ldconfig stty sh bash zsh env exit git go cargo rustc npm npx pip pip3
python python3 node docker kubectl helm apt apt-get yum dnf pacman tar curl wget ssh scp ip ifconfig netstat ss top strace
ltrace gdb nm objdump readelf ld ar file useradd passwd source alias unset ulimit sysctl modprobe insmod lsmod dmesg journalctl
systemctl service crontab tail head less more sort uniq wc awk sed xargs tee time nohup nice renice chroot mknod mkfifo dd df du`) {
		m[c] = true
	}
	return m
}()

// cleanInline strips markdown emphasis and inline HTML.
func cleanInline(s string) string {
	s = reHTMLTag.ReplaceAllString(s, " ")
	s = strings.NewReplacer("**", "", "__", "", "`", "", "<!--", "", "-->", "").Replace(s)
	s = strings.Trim(s, " *_\t")
	s = reSpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// isNoiseHeading reports headings that are not structure: callouts, captions,
// page numbers, code and shell lines.
func isNoiseHeading(title, raw string, level int) bool {
	if strings.HasPrefix(title, "include <") || strings.HasPrefix(title, "define ") || strings.HasPrefix(title, "!/") {
		return true
	}
	if title != "" && title[0] >= 'a' && title[0] <= 'z' {
		// "# cmd" is a root shell prompt; other lowercase headings must be bold
		if level == 1 || (!strings.Contains(raw, "**") && !strings.Contains(raw, "__")) {
			return true
		}
	}
	if title == "" || len(title) > 160 {
		return true
	}
	if noiseTitles[strings.ToLower(title)] {
		return true
	}
	if reOnlyNums.MatchString(title) || reCaption.MatchString(title) {
		return true
	}
	if strings.ContainsAny(title, "=;{}$") || strings.Contains(title, " # ") || len(title) > 120 {
		return true // code, shell sessions ("# mkdir d # mount ..."), or a whole paragraph
	}
	if reShellCmd.MatchString(title) {
		return true
	}
	if sp := strings.IndexByte(title, ' '); sp > 0 && shellCommands[title[:sp]] {
		return true // "# chown root prog" root-prompt lines
	}
	letters := 0
	for _, r := range title {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	return letters < 2
}

// chapterKind: 2 = "Part N", 1 = "Chapter N" / "Appendix A", 0 = other.
func chapterKind(title string) int {
	switch {
	case rePart.MatchString(title):
		return 2
	case reChapter.MatchString(title):
		return 1
	}
	return 0
}

// truncate shortens s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// isSpacedLetters reports letter-spaced titles like "P R O C E S S E S".
func isSpacedLetters(title string) bool {
	f := strings.Fields(title)
	if len(f) < 4 {
		return false
	}
	single, runes := 0, 0
	for _, t := range f {
		n := utf8.RuneCountInString(t)
		runes += n
		if n == 1 {
			single++
		}
	}
	if single*10 >= len(f)*8 {
		return true
	}
	return strings.ToUpper(title) == title && single*10 >= len(f)*4 && runes*10 <= len(f)*25
}

// vocab holds a book's word frequencies for re-segmenting spaced titles.
type vocab struct {
	freq  map[string]int
	total float64
}

func buildVocab(lines []string) *vocab {
	v := &vocab{freq: map[string]int{}}
	var buf []byte
	flush := func() {
		if len(buf) > 0 && len(buf) <= 20 {
			v.freq[string(buf)]++
			v.total++
		}
		buf = buf[:0]
	}
	for _, l := range lines {
		for i := 0; i < len(l); i++ {
			c := l[i]
			switch {
			case c >= 'a' && c <= 'z':
				buf = append(buf, c)
			case c >= 'A' && c <= 'Z':
				buf = append(buf, c+32)
			default:
				flush()
			}
		}
		flush()
	}
	return v
}

// segment splits a run of letters into the most probable words (Viterbi).
func (v *vocab) segment(run string) []string {
	n := len(run)
	best := make([]float64, n+1)
	from := make([]int, n+1)
	for i := 1; i <= n; i++ {
		best[i] = math.Inf(1)
		for j := max(0, i-20); j < i; j++ {
			w := run[j:i]
			var cost float64
			if f := v.freq[w]; f > 0 && (len(w) > 1 || w == "a" || w == "i") {
				// penalize long tokens such as run-together page headers
				cost = -math.Log(float64(f)/v.total) + 0.6*float64(max(0, len(w)-8))
			} else {
				cost = 25 + 5*float64(len(w))
			}
			if c := best[j] + cost; c < best[i] {
				best[i], from[i] = c, j
			}
		}
	}
	var words []string
	for i := n; i > 0; i = from[i] {
		words = append([]string{run[from[i]:i]}, words...)
	}
	return words
}

// despace turns "F I L E I / O" into "FILE I/O".
func (v *vocab) despace(title string) string {
	joined := strings.Join(strings.Fields(title), "")
	var out strings.Builder
	var run []byte
	flushRun := func() {
		if len(run) == 0 {
			return
		}
		words := v.segment(strings.ToLower(string(run)))
		if out.Len() > 0 {
			if last := out.String()[out.Len()-1]; last != '/' && last != '-' && last != '(' && last != ' ' {
				out.WriteByte(' ')
			}
		}
		out.WriteString(strings.ToUpper(strings.Join(words, " ")))
		run = run[:0]
	}
	for i := 0; i < len(joined); i++ {
		c := joined[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			run = append(run, c)
			continue
		}
		flushRun()
		switch c {
		case ':', ',', ';':
			out.WriteByte(c)
			out.WriteByte(' ')
		case '&':
			out.WriteString(" & ")
		default:
			out.WriteByte(c)
		}
	}
	flushRun()
	return strings.Join(strings.Fields(out.String()), " ")
}

// CleanLine strips heading markers and inline markdown from a line.
func CleanLine(l string) string {
	return cleanInline(strings.TrimLeft(strings.TrimSpace(l), "#> "))
}
