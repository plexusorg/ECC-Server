package model

// The tokenizer: a pure-Go reading of the bundle's tokenizer.json files (Unigram models: e5-small's and
// XLM-R-large's): added-token splitting, the SentencePiece Precompiled normalizer (with the e5 file's space collapse),
// Metaspace, and Unigram Viterbi, as Hugging Face tokenizers does them. Encode returns the ids of
// encode(text, add_special_tokens=False).

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

type added struct {
	content        string
	id             int32
	lstrip, rstrip bool
}

type Tokenizer struct {
	ids      map[string]int32
	scores   []float64
	maxPiece int
	unkID    int32
	unkScore float64
	trie     []uint32 // the charsmap's double array
	norm     []byte   // the charsmap's normalized strings, NUL-separated
	replace  []replaceRule
	added    []added
	wsSplit  bool // WhitespaceSplit before Metaspace (the XLM-R-large file)
}

type replaceRule struct {
	re   *regexp.Regexp
	with string
}

type normJSON struct {
	Type        string          `json:"type"`
	Charsmap    string          `json:"precompiled_charsmap"`
	Normalizers []normJSON      `json:"normalizers"`
	Pattern     json.RawMessage `json:"pattern"`
	Content     string          `json:"content"`
}

type fileJSON struct {
	Added []struct {
		ID      int32  `json:"id"`
		Content string `json:"content"`
		LStrip  bool   `json:"lstrip"`
		RStrip  bool   `json:"rstrip"`
		Single  bool   `json:"single_word"`
		Normal  bool   `json:"normalized"`
	} `json:"added_tokens"`
	Normalizer *normJSON `json:"normalizer"`
	Pre        preJSON   `json:"pre_tokenizer"`
	Model      struct {
		Type     string               `json:"type"`
		UnkID    int32                `json:"unk_id"`
		Vocab    [][2]json.RawMessage `json:"vocab"`
		Fallback bool                 `json:"byte_fallback"`
	} `json:"model"`
}

type preJSON struct {
	Type        string    `json:"type"`
	Replacement string    `json:"replacement"`
	Prefix      *bool     `json:"add_prefix_space"`
	Scheme      string    `json:"prepend_scheme"`
	Split       *bool     `json:"split"`
	Pre         []preJSON `json:"pretokenizers"`
}

func metaspaceOK(p preJSON) bool {
	return p.Type == "Metaspace" && p.Replacement == "▁" && (p.Split == nil || *p.Split) &&
		(p.Scheme == "always" || (p.Scheme == "" && p.Prefix != nil && *p.Prefix))
}

// LoadTokenizer reads a tokenizer.json and refuses any part it does not implement.
func LoadTokenizer(path string) (*Tokenizer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f fileJSON
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	if f.Model.Type != "Unigram" || f.Model.Fallback {
		return nil, fmt.Errorf("%s: model %s (byte_fallback %v) is not supported", path, f.Model.Type, f.Model.Fallback)
	}
	wsSplit := false
	switch {
	case metaspaceOK(f.Pre):
	case f.Pre.Type == "Sequence" && len(f.Pre.Pre) == 2 && f.Pre.Pre[0].Type == "WhitespaceSplit" && metaspaceOK(f.Pre.Pre[1]):
		wsSplit = true
	default:
		return nil, fmt.Errorf("%s: pre_tokenizer %+v is not supported", path, f.Pre)
	}
	t := &Tokenizer{ids: make(map[string]int32, len(f.Model.Vocab)), unkID: f.Model.UnkID, wsSplit: wsSplit}
	minScore := math.Inf(1)
	for i, v := range f.Model.Vocab {
		var piece string
		var score float64
		if err := json.Unmarshal(v[0], &piece); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(v[1], &score); err != nil {
			return nil, err
		}
		t.ids[piece] = int32(i)
		t.scores = append(t.scores, score)
		minScore = math.Min(minScore, score)
		t.maxPiece = max(t.maxPiece, len(piece))
	}
	t.unkScore = minScore - 10.0
	for _, a := range f.Added {
		if a.Single || a.Normal {
			return nil, fmt.Errorf("%s: added token %q with single_word/normalized is not supported", path, a.Content)
		}
		t.added = append(t.added, added{a.Content, a.ID, a.LStrip, a.RStrip})
	}
	if err := t.loadNormalizer(f.Normalizer); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

func (t *Tokenizer) loadNormalizer(n *normJSON) error {
	if n == nil {
		return nil
	}
	switch n.Type {
	case "Sequence":
		for i := range n.Normalizers {
			if err := t.loadNormalizer(&n.Normalizers[i]); err != nil {
				return err
			}
		}
		return nil
	case "Precompiled":
		if t.trie != nil || len(t.replace) > 0 {
			return fmt.Errorf("Precompiled must come first")
		}
		b, err := base64.StdEncoding.DecodeString(n.Charsmap)
		if err != nil {
			return err
		}
		size := binary.LittleEndian.Uint32(b)
		for i := uint32(0); i < size; i += 4 {
			t.trie = append(t.trie, binary.LittleEndian.Uint32(b[4+i:]))
		}
		t.norm = b[4+size:]
		return nil
	case "Replace":
		var p struct {
			Regex  *string `json:"Regex"`
			String *string `json:"String"`
		}
		if err := json.Unmarshal(n.Pattern, &p); err != nil {
			return err
		}
		var re *regexp.Regexp
		var err error
		switch {
		case p.Regex != nil:
			re, err = regexp.Compile(*p.Regex)
		case p.String != nil:
			re, err = regexp.Compile(regexp.QuoteMeta(*p.String))
		default:
			err = fmt.Errorf("a Replace normalizer has no pattern")
		}
		if err != nil {
			return err
		}
		t.replace = append(t.replace, replaceRule{re, n.Content})
		return nil
	}
	return fmt.Errorf("normalizer %s is not supported", n.Type)
}

// ---------------------------------------------------------------- the Precompiled normalizer

// prefix returns the normalized string of the shortest key in the charsmap that is a prefix of chunk, as
// spm_precompiled's transform does (it takes the first result of a common prefix search).
func (t *Tokenizer) prefix(chunk string) (string, bool) {
	a := t.trie
	pos := uint32(0)
	u := a[pos]
	pos ^= offset(u)
	for i := 0; i < len(chunk); i++ {
		c := uint32(chunk[i])
		if c == 0 {
			break
		}
		pos ^= c
		u = a[pos]
		if label(u) != c {
			return "", false
		}
		pos ^= offset(u)
		if hasLeaf(u) {
			v := a[pos] & ((1 << 31) - 1)
			end := v
			for int(end) < len(t.norm) && t.norm[end] != 0 {
				end++
			}
			return string(t.norm[v:end]), true
		}
	}
	return "", false
}

func hasLeaf(u uint32) bool  { return (u>>8)&1 == 1 }
func label(u uint32) uint32  { return u & ((1 << 31) | 0xFF) }
func offset(u uint32) uint32 { return (u >> 10) << ((u & (1 << 9)) >> 6) }

func (t *Tokenizer) normalize(s string) string {
	if t.trie != nil {
		var b strings.Builder
		g := s
		for len(g) > 0 {
			var cl string
			cl, g, _, _ = uniseg.FirstGraphemeClusterInString(g, -1)
			if len(cl) < 6 {
				if n, ok := t.prefix(cl); ok {
					b.WriteString(n)
					continue
				}
			}
			for _, r := range cl {
				part := string(r)
				if n, ok := t.prefix(part); ok {
					b.WriteString(n)
				} else {
					b.WriteString(part)
				}
			}
		}
		s = b.String()
	}
	for _, r := range t.replace {
		s = r.re.ReplaceAllLiteralString(s, r.with)
	}
	return s
}

// ---------------------------------------------------------------- encoding

// Encode returns the token ids of text with no special tokens added; special-token text in it becomes that token.
func (t *Tokenizer) Encode(text string) []int32 {
	var out []int32
	start := 0
	for i := 0; i < len(text); {
		best := -1
		for k, a := range t.added {
			if strings.HasPrefix(text[i:], a.content) && (best < 0 || len(a.content) > len(t.added[best].content)) {
				best = k
			}
		}
		if best < 0 {
			_, n := utf8.DecodeRuneInString(text[i:])
			i += n
			continue
		}
		a := t.added[best]
		ms, me := i, i+len(a.content)
		if a.lstrip {
			j := ms
			for j > start {
				r, n := utf8.DecodeLastRuneInString(text[:j])
				if !unicode.IsSpace(r) {
					break
				}
				j -= n
			}
			ms = j
		}
		if a.rstrip {
			for me < len(text) {
				r, n := utf8.DecodeRuneInString(text[me:])
				if !unicode.IsSpace(r) {
					break
				}
				me += n
			}
		}
		if start < ms {
			out = t.encodePiece(text[start:ms], out)
		}
		out = append(out, a.id)
		start, i = me, me
	}
	if start < len(text) {
		out = t.encodePiece(text[start:], out)
	}
	return out
}

func (t *Tokenizer) encodePiece(s string, out []int32) []int32 {
	s = t.normalize(s)
	if !t.wsSplit {
		return t.metaspace(s, out)
	}
	for _, w := range strings.FieldsFunc(s, unicode.IsSpace) {
		out = t.metaspace(w, out)
	}
	return out
}

func (t *Tokenizer) metaspace(s string, out []int32) []int32 {
	if s == "" {
		return out
	}
	s = strings.ReplaceAll(s, " ", "▁")
	if !strings.HasPrefix(s, "▁") {
		s = "▁" + s
	}
	// Split before every ▁ (MergedWithNext).
	for len(s) > 0 {
		j := strings.Index(s[len("▁"):], "▁")
		if !strings.HasPrefix(s, "▁") {
			j = strings.Index(s, "▁")
		} else if j >= 0 {
			j += len("▁")
		}
		if j < 0 {
			j = len(s)
		}
		out = t.viterbi(s[:j], out)
		s = s[j:]
	}
	return out
}

type node struct {
	score float64
	start int
	id    int32
	set   bool
}

// viterbi is Hugging Face's Unigram encode_optimized with fused unknowns.
func (t *Tokenizer) viterbi(s string, out []int32) []int32 {
	n := len(s)
	best := make([]node, n+1)
	best[0].set = true
	for st := 0; st < n; {
		till := best[st].score
		_, mb := utf8.DecodeRuneInString(s[st:])
		single := false
		for e := st + 1; e <= n && e-st <= t.maxPiece; e++ {
			if e < n && !utf8.RuneStart(s[e]) {
				continue
			}
			id, ok := t.ids[s[st:e]]
			if !ok {
				continue
			}
			c := t.scores[id] + till
			if !best[e].set || c > best[e].score {
				best[e] = node{c, st, id, true}
			}
			if e-st == mb {
				single = true
			}
		}
		if !single {
			c := t.unkScore + till
			if !best[st+mb].set || c > best[st+mb].score {
				best[st+mb] = node{c, st, t.unkID, true}
			}
		}
		st += mb
	}
	var rev []int32
	unkEnd := -1 // end of the run of unknowns being fused, or -1
	flush := func(st int) {
		if unkEnd >= 0 {
			id, ok := t.ids[s[st:unkEnd]]
			if !ok {
				id = t.unkID
			}
			rev = append(rev, id)
			unkEnd = -1
		}
	}
	e := n
	for e > 0 {
		nd := best[e]
		if nd.id == t.unkID {
			if unkEnd < 0 {
				unkEnd = e
			}
		} else {
			flush(e)
			rev = append(rev, nd.id)
		}
		e = nd.start
	}
	flush(0)
	for i := len(rev) - 1; i >= 0; i-- {
		out = append(out, rev[i])
	}
	return out
}
