package main

import (
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	"ecc-server/core"
)

// wordScorer is the end-to-end rig's scorer, set with [test] hold_words_file: it holds a line that contains one of
// the file's words (one a line, any case) and delivers every other line. It stands in for the fast model so the rig
// can drive the server's sessions, checkpoints and actions; it filters nothing a game server needs filtered. A line
// that contains [test] slow_word takes [test] slow_for to score, as a slow model call.
type wordScorer struct {
	words    []string
	slowWord string
	slowFor  time.Duration
}

func newWordScorer(t TestConfig) (*wordScorer, error) {
	data, err := os.ReadFile(t.HoldWordsFile)
	if err != nil {
		return nil, fmt.Errorf("[test] hold_words_file: %w", err)
	}
	w := wordScorer{slowWord: strings.ToLower(t.SlowWord), slowFor: t.SlowFor}
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			w.words = append(w.words, l)
		}
	}
	if len(w.words) == 0 {
		return nil, fmt.Errorf("[test] hold_words_file %s lists no words", t.HoldWordsFile)
	}
	for range 3 {
		slog.Warn("THE TEST RIG'S SCORER IS IN USE: lines are held by the word list in [test] hold_words_file, not by " +
			"a model. This server does not filter chat; never run it for a game server with players.")
	}
	return &w, nil
}

func (w *wordScorer) See(core.Line) {}

func (w *wordScorer) Score(l core.Line) (core.Verdict, error) {
	words := strings.FieldsFunc(strings.ToLower(l.Text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if w.slowWord != "" && slices.Contains(words, w.slowWord) {
		time.Sleep(w.slowFor)
	}
	for _, f := range words {
		if slices.Contains(w.words, f) {
			return core.Verdict{Hold: true}, nil
		}
	}
	return core.Verdict{}, nil
}
