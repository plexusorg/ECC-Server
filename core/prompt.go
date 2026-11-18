package core

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// entry is one line of the stream with what the core decided about it.
type entry struct {
	pos  int
	line Line
	// session is the session whose player said the line ("" for a context line).
	session    string
	held       bool
	categories []string
	// verdict is the line's verdict; released marks a held line a ruling delivered.
	verdict  string
	released bool
	// unscored marks a line held whose models have not run yet (ErrLater): not held for the judge until Rescored.
	unscored bool
	// refs is the driver's id of the line and of each of its held repeats, with the order each arrived in.
	refs []lineRef
}

type lineRef struct {
	ref string
	n   int
}

// stream is every line since the game server started, in arrival order, numbered from 0. Lines no open session can
// show any more are dropped; of a private conversation the last Window.Before lines before the drop point are kept,
// since a private line's window counts that conversation's lines only.
type stream struct {
	name  string
	next  int
	base  int
	lines []*entry       // positions base..next-1
	kept  map[int]*entry // private lines older than base that a window can still show
	talks map[string][]int
}

func newStream(name string) *stream {
	return &stream{name: name, kept: map[int]*entry{}, talks: map[string][]int{}}
}

func (s *stream) add(e *entry) {
	e.pos = s.next
	s.next++
	s.lines = append(s.lines, e)
	if e.line.Private {
		s.talks[e.line.Conversation] = append(s.talks[e.line.Conversation], e.pos)
	}
}

func (s *stream) get(p int) *entry {
	if p >= s.base {
		return s.lines[p-s.base]
	}
	e := s.kept[p]
	if e == nil {
		panic(fmt.Sprintf("stream %s: line %d was dropped while a session could show it", s.name, p))
	}
	return e
}

// prune drops the lines before position from, keeping each private conversation's last `tail` lines before it.
func (s *stream) prune(from, tail int) {
	if from <= s.base {
		return
	}
	if from > s.next {
		from = s.next
	}
	for conv, ps := range s.talks {
		i := sort.SearchInts(ps, from)
		keepFrom := max(0, i-tail)
		for _, p := range ps[:keepFrom] {
			delete(s.kept, p)
		}
		for _, p := range ps[keepFrom:i] {
			if p >= s.base {
				s.kept[p] = s.lines[p-s.base]
			}
		}
		if keepFrom == len(ps) {
			delete(s.talks, conv)
		} else {
			s.talks[conv] = ps[keepFrom:]
		}
	}
	s.lines = s.lines[from-s.base:]
	s.base = from
}

// Render holds how a checkpoint's session is shown to the judge.
type Render struct {
	// Lines before and after each of the player's marked lines that the judge sees.
	Before int `toml:"before"`
	After  int `toml:"after"`
	// A session longer than MaxChars characters is cut: the held lines' windows only, then the held lines' with
	// CutBefore and CutAfter, then as many held lines spread evenly as fit.
	MaxChars  int `toml:"max_chars"`
	CutBefore int `toml:"cut_before"`
	CutAfter  int `toml:"cut_after"`
	// Categories names each category as the judge reads it.
	Categories map[string]string `toml:"categories"`
}

var errTooLong = errors.New("one held line does not fit in the case's size")

// view is a session as of one line of the stream: what a checkpoint at that line shows.
type view struct {
	st      *stream
	session string
	end     int   // the last position shown
	mine    []int // the player's lines up to end
	held    []int // of them, the held ones
}

func (v view) windows(pos []int, before, after int) []int {
	keep := map[int]bool{}
	n := v.end + 1
	for _, p := range pos {
		e := v.st.get(p)
		if e.line.Private {
			talk := v.st.talks[e.line.Conversation]
			talk = talk[:sort.SearchInts(talk, n)]
			i := sort.SearchInts(talk, p)
			for _, q := range talk[max(0, i-before):min(len(talk), i+after+1)] {
				keep[q] = true
			}
			continue
		}
		for q := max(0, p-before); q < min(n, p+after+1); q++ {
			keep[q] = true
		}
	}
	out := make([]int, 0, len(keep))
	for p := range keep {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// player is the name the player showed most often up to the checkpoint, the earliest of equals.
func (v view) player() string {
	count := map[string]int{}
	var order []string
	for _, p := range v.mine {
		name := v.st.get(p).line.Name
		if count[name] == 0 {
			order = append(order, name)
		}
		count[name]++
	}
	best := ""
	for _, name := range order {
		if best == "" || count[name] > count[best] {
			best = name
		}
	}
	return best
}

// shown is the case's text and the ids of the player's lines in it.
type shown struct {
	text string
	mine []string
}

func (r Render) text(v view, keep []int, player string) shown {
	var b strings.Builder
	var ids []string
	b.WriteString("player: ")
	b.WriteString(player)
	b.WriteString(" · log ")
	// the UTC date of the case's first line; each line's time is its UTC time of day
	b.WriteString(v.st.get(keep[0]).line.Time.UTC().Format(time.DateOnly))
	last := -1
	for _, p := range keep {
		e := v.st.get(p)
		b.WriteByte('\n')
		if last >= 0 && p != last+1 {
			b.WriteString("···\n")
		}
		last = p
		mine := e.session != "" && e.session == v.session
		if mine {
			b.WriteString(">> ")
			ids = append(ids, e.line.ID)
		}
		l := e.line
		b.WriteString(l.ID)
		b.WriteByte(' ')
		b.WriteString(l.Time.UTC().Format(time.TimeOnly))
		b.WriteByte(' ')
		b.WriteString(l.Channel)
		if l.Target != "" {
			b.WriteString(" to ")
			b.WriteString(l.Target)
		}
		b.WriteByte(' ')
		b.WriteString(l.Name)
		b.WriteString(": ")
		b.WriteString(l.Text)
		if l.Sent > 1 {
			b.WriteString(" (sent " + strconv.Itoa(l.Sent) + " times)")
		}
		if mine && e.held {
			if e.released {
				b.WriteString(" [previously cleared]")
			} else {
				b.WriteString(" [held]")
			}
		}
		if mine {
			for _, k := range e.categories {
				name, ok := r.Categories[k]
				if !ok {
					name = k
				}
				b.WriteString(" [" + name + "]")
			}
		}
	}
	return shown{b.String(), ids}
}

func (r Render) fits(s shown) bool { return utf8.RuneCountInString(s.text) <= r.MaxChars }

// render is the checkpoint's case: the windows around the player's marked lines, cut down to MaxChars.
func (r Render) render(v view) (shown, error) {
	player := v.player()
	var flagged []int
	for _, p := range v.mine {
		if e := v.st.get(p); e.held || len(e.categories) > 0 {
			flagged = append(flagged, p)
		}
	}
	for _, try := range []struct {
		pos           []int
		before, after int
	}{{flagged, r.Before, r.After}, {v.held, r.Before, r.After}, {v.held, r.CutBefore, r.CutAfter}} {
		if s := r.text(v, v.windows(try.pos, try.before, try.after), player); r.fits(s) {
			return s, nil
		}
	}
	m := len(v.held)
	lo, hi, best := 1, m, shown{}
	for lo <= hi {
		k := (lo + hi) / 2
		var pick []int
		if k > 1 {
			seen := map[int]bool{}
			for i := range k {
				p := v.held[int(math.RoundToEven(float64(i*(m-1))/float64(k-1)))]
				if !seen[p] {
					seen[p] = true
					pick = append(pick, p)
				}
			}
			sort.Ints(pick)
		} else {
			pick = []int{v.held[m-1]}
		}
		if s := r.text(v, v.windows(pick, r.CutBefore, r.CutAfter), player); r.fits(s) {
			best, lo = s, k+1
		} else {
			hi = k - 1
		}
	}
	if best.text == "" {
		return shown{}, errTooLong
	}
	return best, nil
}

// The fixed strings of the judge's prompt: QuestionHead, the question, SepCases, the owner's cases joined by
// BlockSep (each "## C<n>: ruling: <ruling>\n" and its text), SepNew, "## N1\n" and the case, SepFormat, the format.
const (
	QuestionHead  = "# Question\n\n"
	SepCases      = "\n\n# The owner's cases\n\n"
	BlockSep      = "\n\n"
	SepNew        = "\n\n# New cases\n\n"
	SepFormat     = "\n\n# Answer format\n\n"
	ExampleHeader = "## C<n>: ruling: <ruling>"
	NewCase       = "N1"
)

// prompt lays out the judge's prompt around one case. Its first split bytes are the question and the owner's cases,
// the same in every prompt; the rest is SepNew, the case, SepFormat and the format.
func (t Texts) prompt(c string) (prompt string, split int) {
	var b strings.Builder
	b.WriteString(QuestionHead)
	b.WriteString(t.Question)
	b.WriteString(SepCases)
	for i, e := range t.Examples {
		if i > 0 {
			b.WriteString(BlockSep)
		}
		b.WriteString("## C" + strconv.Itoa(i+1) + ": ruling: " + e.Ruling + "\n")
		b.WriteString(e.Text)
	}
	split = b.Len()
	b.WriteString(SepNew + "## " + NewCase + "\n")
	b.WriteString(c)
	b.WriteString(SepFormat)
	b.WriteString(t.Format)
	return b.String(), split
}
