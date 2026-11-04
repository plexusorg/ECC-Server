// parity replays Module-ECC's evaluation conversations through a running ECC-Server, in conversation order, and
// compares each message's margin with the margin `uv run eval` recorded for it.
//
//	go run ./parity -ecc <Module-ECC checkout> -bundle <bundle folder> -out results.jsonl
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"
)

type message struct {
	speaker, text string
	key           string // the eval row key; "" when the eval has no row for it
}

type conversation struct {
	set, id  string
	messages []message
}

type result struct {
	Set        string   `json:"set"`
	Conv       string   `json:"conv"`
	Key        string   `json:"key,omitempty"`
	Speaker    string   `json:"speaker"`
	Text       string   `json:"text"`
	Margin     float64  `json:"margin"`
	EvalMargin *float64 `json:"eval_margin,omitempty"`
	PBan       float64  `json:"p_ban"`
	Ms         float64  `json:"ms"`
	Earlier    int      `json:"earlier"`
	Tokens     int      `json:"tokens"`
	Evaluated  int      `json:"evaluated"`
	Version    string   `json:"version"`
	Error      string   `json:"error,omitempty"`
}

func main() {
	ecc := flag.String("ecc", `C:\Users\telesphoreo\PycharmProjects\Module-ECC`, "Module-ECC checkout")
	bundle := flag.String("bundle", "", "bundle folder (for its Platt parameters)")
	host := flag.String("host", "http://127.0.0.1:8790", "ECC-Server address")
	model := flag.String("model", "classifier", "eval model folder under data/eval")
	tfConvs := flag.Int("tf", 200, "Total Freedom conversations to replay")
	seed := flag.Uint64("seed", 1, "seed of the Total Freedom sample")
	sets := flag.String("sets", "cases,redteam,heldout", "case sets to replay")
	workers := flag.Int("workers", 4, "conversations replayed at once")
	out := flag.String("out", "parity.jsonl", "per-message results")
	flag.Parse()

	var b struct {
		Platt struct{ A, B float64 } `json:"platt"`
	}
	raw, err := os.ReadFile(filepath.Join(*bundle, "bundle.json"))
	check(err)
	check(json.Unmarshal(raw, &b))

	evalDir := filepath.Join(*ecc, "data", "eval", *model)
	var convs []conversation
	evalMargins := map[string]float64{}
	for _, set := range strings.Split(*sets, ",") {
		if set == "" {
			continue
		}
		files := caseFiles(*ecc, set)
		c := loadCases(set, files)
		readEval(filepath.Join(evalDir, set+".jsonl"), func(k string, m float64) { evalMargins[set+"|"+k] = m })
		convs = append(convs, c...)
	}
	if *tfConvs > 0 {
		convs = append(convs, loadTF(filepath.Join(evalDir, "tf.jsonl"), *tfConvs, *seed, evalMargins)...)
	}
	total := 0
	for _, c := range convs {
		total += len(c.messages)
	}
	fmt.Printf("replaying %d conversations, %d messages, %d workers\n", len(convs), total, *workers)

	results := make([][]result, len(convs))
	jobs := make(chan int)
	var wg sync.WaitGroup
	var done sync.Mutex
	n := 0
	start := time.Now()
	client := &http.Client{Timeout: 5 * time.Minute}
	for range *workers {
		wg.Go(func() {
			for i := range jobs {
				results[i] = replay(client, *host, convs[i], evalMargins)
				done.Lock()
				n += len(convs[i].messages)
				if (n*20)/total != ((n-len(convs[i].messages))*20)/total {
					fmt.Printf("  %d / %d messages, %.0fs\n", n, total, time.Since(start).Seconds())
				}
				done.Unlock()
			}
		})
	}
	// longest conversations first, so the replay ends with the short ones
	order := make([]int, len(convs))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return len(convs[order[a]].messages) > len(convs[order[b]].messages) })
	for _, i := range order {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	wall := time.Since(start)

	f, err := os.Create(*out)
	check(err)
	w := bufio.NewWriter(f)
	var all []result
	for _, rs := range results {
		for _, r := range rs {
			line, _ := json.Marshal(r)
			w.Write(append(line, '\n'))
			all = append(all, r)
		}
	}
	check(w.Flush())
	check(f.Close())
	fmt.Printf("wall %.0fs, %.1f messages/s\n\n", wall.Seconds(), float64(len(all))/wall.Seconds())
	for _, set := range append(strings.Split(*sets, ","), "tf", "all") {
		var rs []result
		for _, r := range all {
			if r.Set == set || set == "all" {
				rs = append(rs, r)
			}
		}
		if len(rs) > 0 {
			summarize(set, rs, b.Platt.A, b.Platt.B)
		}
	}
	latency(all, results)
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------- inputs

func caseFiles(ecc, set string) []string {
	var cfg struct {
		Eval struct {
			Sets map[string][]string `toml:"sets"`
		} `toml:"eval"`
	}
	_, err := toml.DecodeFile(filepath.Join(ecc, "ecc.toml"), &cfg)
	check(err)
	var files []string
	for _, f := range cfg.Eval.Sets[set] {
		f = strings.ReplaceAll(f, "{data}", filepath.Join(ecc, "data"))
		if !filepath.IsAbs(f) {
			f = filepath.Join(ecc, f)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		check(fmt.Errorf("ecc.toml has no [eval.sets] %s", set))
	}
	return files
}

// pySplitLines is Python's str.splitlines after read_text's universal newlines.
func pySplitLines(s string) []string {
	return strings.FieldsFunc(strings.ReplaceAll(s, "\r\n", "\n"), func(r rune) bool {
		switch r {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			return true
		}
		return false
	})
}

// pyStrip is Python's str.strip(): unicode.IsSpace plus the separators 0x1c-0x1f Python also counts as space.
func pyStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) })
}

var header = regexp.MustCompile(`^##\s*(\S+)\s*\[(\w+)\]`)

// loadCases is Module-ECC chat.load_cases with evaluate.case_keys: conversations in file order, each message keyed
// by [conv, text, occurrence], occurrences counted per conversation id across the set.
func loadCases(set string, files []string) []conversation {
	var convs []conversation
	seen := map[[2]string]int{}
	for _, path := range files {
		data, err := os.ReadFile(path)
		check(err)
		id := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		cur := -1
		for _, raw := range pySplitLines(string(data)) {
			l := pyStrip(raw)
			if l == "" || (strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "##")) {
				continue
			}
			if strings.HasPrefix(l, "##") {
				m := header.FindStringSubmatch(l)
				id = m[1]
				convs = append(convs, conversation{set: set, id: fmt.Sprintf("%s/%s#%d", set, id, len(convs))})
				cur = len(convs) - 1
				continue
			}
			if cur < 0 {
				convs = append(convs, conversation{set: set, id: fmt.Sprintf("%s/%s#%d", set, id, len(convs))})
				cur = len(convs) - 1
			}
			parts := strings.SplitN(l, "|", 3)
			key, _ := json.Marshal([]any{id, parts[2], seen[[2]string{id, parts[2]}]})
			seen[[2]string{id, parts[2]}]++
			convs[cur].messages = append(convs[cur].messages, message{speaker: parts[1], text: parts[2], key: string(key)})
		}
	}
	return convs
}

func readEval(path string, add func(string, float64)) {
	f, err := os.Open(path)
	check(err)
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	seen := map[string]bool{}
	for sc.Scan() {
		var r struct {
			K      json.RawMessage `json:"k"`
			Margin *float64        `json:"margin"`
		}
		check(json.Unmarshal(sc.Bytes(), &r))
		var ks string
		if json.Unmarshal(r.K, &ks) != nil {
			ks = string(r.K)
		}
		// the key re-encoded the way loadCases encodes it
		var parts []any
		if json.Unmarshal([]byte(ks), &parts) == nil {
			b, _ := json.Marshal(parts)
			ks = string(b)
		}
		if seen[ks] || r.Margin == nil {
			continue
		}
		seen[ks] = true
		add(ks, *r.Margin)
	}
	check(sc.Err())
}

// loadTF picks n whole Total Freedom conversations of the eval's tf set at random; each conversation's rows in row
// order are its messages in order.
func loadTF(path string, n int, seed uint64, margins map[string]float64) []conversation {
	f, err := os.Open(path)
	check(err)
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	type row struct {
		K       int      `json:"k"`
		Conv    string   `json:"conv"`
		Speaker string   `json:"speaker"`
		Text    string   `json:"text"`
		Margin  *float64 `json:"margin"`
	}
	byConv := map[string][]row{}
	seen := map[int]bool{}
	for sc.Scan() {
		var r row
		check(json.Unmarshal(sc.Bytes(), &r))
		if seen[r.K] {
			continue
		}
		seen[r.K] = true
		byConv[r.Conv] = append(byConv[r.Conv], r)
	}
	check(sc.Err())
	ids := make([]string, 0, len(byConv))
	for id := range byConv {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	rng := rand.New(rand.NewPCG(seed, seed))
	rng.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	fmt.Printf("tf.jsonl: %d conversations, %d rows\n", len(ids), len(seen))
	var convs []conversation
	for _, id := range ids[:min(n, len(ids))] {
		rows := byConv[id]
		sort.Slice(rows, func(a, b int) bool { return rows[a].K < rows[b].K })
		c := conversation{set: "tf", id: "tf/" + id}
		for _, r := range rows {
			key := fmt.Sprint(r.K)
			if r.Margin != nil {
				margins["tf|"+key] = *r.Margin
			}
			c.messages = append(c.messages, message{speaker: r.Speaker, text: r.Text, key: key})
		}
		convs = append(convs, c)
	}
	return convs
}

// ---------------------------------------------------------------- replay

func replay(client *http.Client, host string, c conversation, evalMargins map[string]float64) []result {
	var rs []result
	for _, m := range c.messages {
		body, _ := json.Marshal(map[string]string{"conversation": c.id, "speaker": m.speaker, "text": m.text})
		t0 := time.Now()
		resp, err := client.Post(host+"/score", "application/json", bytes.NewReader(body))
		r := result{Set: c.set, Conv: c.id, Speaker: m.speaker, Text: m.text}
		if err == nil {
			var buf bytes.Buffer
			buf.ReadFrom(resp.Body)
			resp.Body.Close()
			r.Ms = float64(time.Since(t0).Microseconds()) / 1000
			if resp.StatusCode != http.StatusOK {
				r.Error = fmt.Sprintf("%s: %s", resp.Status, strings.TrimSpace(buf.String()))
			} else {
				var s struct {
					PBan      float64 `json:"p_ban"`
					Margin    float64 `json:"margin"`
					Version   string  `json:"version"`
					Earlier   int     `json:"earlier"`
					Tokens    int     `json:"tokens"`
					Evaluated int     `json:"evaluated"`
				}
				err = json.Unmarshal(buf.Bytes(), &s)
				r.PBan, r.Margin, r.Version, r.Earlier, r.Tokens, r.Evaluated = s.PBan, s.Margin, s.Version, s.Earlier, s.Tokens, s.Evaluated
			}
		}
		if err != nil {
			r.Error = err.Error()
		}
		if v, ok := evalMargins[c.set+"|"+m.key]; ok && m.key != "" {
			r.Key = m.key
			r.EvalMargin = &v
		}
		rs = append(rs, r)
	}
	return rs
}

// ---------------------------------------------------------------- summary

func sigmoid(x float64) float64 { return 1 / (1 + math.Exp(-x)) }

func summarize(set string, rs []result, a, b float64) {
	var xs, ys []float64
	errs, unmatched := 0, 0
	var sumAbs, maxAbs float64
	var maxR result
	flips := map[float64]int{0.5: 0, 0.9: 0}
	above := map[float64]int{0.5: 0, 0.9: 0}
	for _, r := range rs {
		if r.Error != "" {
			errs++
			continue
		}
		if r.EvalMargin == nil {
			unmatched++
			continue
		}
		x, y := r.Margin, *r.EvalMargin
		xs, ys = append(xs, x), append(ys, y)
		d := math.Abs(x - y)
		sumAbs += d
		if d > maxAbs {
			maxAbs, maxR = d, r
		}
		pe := sigmoid(a*y + b)
		for t := range flips {
			if (r.PBan >= t) != (pe >= t) {
				flips[t]++
			}
			if pe >= t {
				above[t]++
			}
		}
	}
	fmt.Printf("%s: %d messages, %d compared, %d without an eval row, %d errors\n", set, len(rs), len(xs), unmatched, errs)
	if len(xs) == 0 {
		return
	}
	fmt.Printf("  Pearson %.6f  mean |diff| %.4f  max |diff| %.4f\n", pearson(xs, ys), sumAbs/float64(len(xs)), maxAbs)
	fmt.Printf("  decisions on different sides: P(ban) 0.5: %d (eval has %d at or above)  0.9: %d (eval has %d)\n",
		flips[0.5], above[0.5], flips[0.9], above[0.9])
	fmt.Printf("  largest diff: %s %q host %.4f eval %.4f\n", maxR.Conv, maxR.Text, maxR.Margin, *maxR.EvalMargin)
}

func pearson(xs, ys []float64) float64 {
	n := float64(len(xs))
	var mx, my float64
	for i := range xs {
		mx += xs[i]
		my += ys[i]
	}
	mx, my = mx/n, my/n
	var sxy, sxx, syy float64
	for i := range xs {
		dx, dy := xs[i]-mx, ys[i]-my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	return sxy / math.Sqrt(sxx*syy)
}

func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.Sort(s)
	return s[min(len(s)-1, int(p*float64(len(s))))]
}

func mean(v []float64) float64 {
	t := 0.0
	for _, x := range v {
		t += x
	}
	return t / float64(max(1, len(v)))
}

// latency splits messages into appends (the window grew by one line, so the previous prompt is a prefix of this one)
// and rebuilds; an append is a cache hit when llama-server decoded only the tokens past the previous prompt.
func latency(all []result, convs [][]result) {
	var ms, app, reb []float64
	hits, appends, evaluated, tokens := 0, 0, 0, 0
	var misses []result
	for _, rs := range convs {
		for i, r := range rs {
			if r.Error != "" {
				continue
			}
			ms = append(ms, r.Ms)
			evaluated += r.Evaluated
			tokens += r.Tokens
			if i > 0 && rs[i-1].Error == "" && r.Earlier == rs[i-1].Earlier+1 && r.Version == rs[i-1].Version {
				appends++
				app = append(app, r.Ms)
				if r.Tokens-r.Evaluated >= rs[i-1].Tokens {
					hits++
				} else {
					misses = append(misses, r)
				}
			} else {
				reb = append(reb, r.Ms)
			}
		}
	}
	fmt.Printf("\nlatency ms: all mean %.1f p50 %.1f p95 %.1f max %.1f (%d)\n", mean(ms), pct(ms, .5), pct(ms, .95), pct(ms, 1), len(ms))
	fmt.Printf("  appends mean %.1f p50 %.1f p95 %.1f (%d); new or rebuilt windows mean %.1f p50 %.1f p95 %.1f (%d)\n",
		mean(app), pct(app, .5), pct(app, .95), len(app), mean(reb), pct(reb, .5), pct(reb, .95), len(reb))
	fmt.Printf("cache: %d of %d appends hit (%.2f%%); decoded %d of %d prompt tokens (%.1f%%)\n", hits, appends,
		100*float64(hits)/float64(max(1, appends)), evaluated, tokens, 100*float64(evaluated)/float64(max(1, tokens)))
	for i, r := range misses {
		if i == 10 {
			break
		}
		fmt.Printf("  miss: %s earlier %d tokens %d evaluated %d\n", r.Conv, r.Earlier, r.Tokens, r.Evaluated)
	}
}
