package main

import (
	"errors"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// The classifier prompt, byte for byte as Module-ECC's training pipeline builds it (src/ecc/chat.py, prompt format
// 1): the bundle's header, then one "speaker: text\n" line per message, the target last. The answer token follows the
// target's newline. A conversation's prompt grows by whole lines, so each prompt is a token prefix of the next and
// llama-server decodes only the new line.

var lineBreaks = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ")

// clean is chat.clean: NFC, then each line break inside a message becomes one space.
func clean(s string) string {
	return lineBreaks.Replace(norm.NFC.String(s))
}

// promptLine is chat.prompt_line.
func promptLine(speaker, text string) string {
	return clean(speaker) + ": " + clean(text) + "\n"
}

type line struct {
	speaker, text string
	cost          int // tokens of promptLine(speaker, text)
}

func classifierPrompt(header string, lines []line, speaker, text string) string {
	var sb strings.Builder
	sb.WriteString(header)
	for _, l := range lines {
		sb.WriteString(promptLine(l.speaker, l.text))
	}
	sb.WriteString(promptLine(speaker, text))
	return sb.String()
}

// fittedPrompt is chat.fitted_prompt: the prompt of the window's lines and the target, dropping the oldest lines
// until it is at most limit tokens; a target too long on its own is cut from the end, to 90% of its code points at a
// time. It returns the prompt and its token count.
func fittedPrompt(header string, lines []line, speaker, text string, limit int, count func(string) (int, error)) (string, int, error) {
	for {
		p := classifierPrompt(header, lines, speaker, text)
		n, err := count(p)
		if err != nil || n <= limit {
			return p, n, err
		}
		if len(lines) > 0 {
			lines = lines[1:]
		} else if text != "" {
			r := []rune(text)
			text = string(r[:int(float64(len(r))*0.9)])
		} else {
			return "", 0, errors.New("the speaker name alone is longer than the window")
		}
	}
}

// window is one conversation's streaming window: the earlier lines the next target sees (chat.window_sizes).
type window struct {
	lines []line
	used  int // header tokens plus the cost of lines
}

// fit makes room for a target of cost tokens, as chat.window_sizes does before each line: when the window already
// holds WindowMax earlier lines, or the target would take it past WindowTokens, it is rebuilt from its last
// WindowMin lines, dropping the oldest of those while the target still would not fit.
func (w *window) fit(b *Bundle, header, cost int) {
	p := b.Prompt
	if len(w.lines) < p.WindowMax && w.used+cost <= p.WindowTokens {
		return
	}
	w.lines = w.lines[max(0, len(w.lines)-p.WindowMin):]
	w.used = header
	for _, l := range w.lines {
		w.used += l.cost
	}
	for len(w.lines) > 0 && w.used+cost > p.WindowTokens {
		w.used -= w.lines[0].cost
		w.lines = w.lines[1:]
	}
}

func (w *window) add(l line) {
	w.lines = append(w.lines, l)
	w.used += l.cost
}
