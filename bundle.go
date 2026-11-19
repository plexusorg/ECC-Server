package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"ecc-server/core"
	"ecc-server/model"
)

// Bundle is bundle.json, written by the pipeline's `uv run bundle`, with the judge's texts it names.
type Bundle struct {
	Version string `json:"version"`
	// The fast model's graphs and tokenizers, by name.
	Graphs map[string]struct {
		XML string `json:"xml"`
		Bin string `json:"bin"`
	} `json:"graphs"`
	Tokenizers map[string]struct {
		File string `json:"file"`
	} `json:"tokenizers"`

	Judge struct {
		// question.md (the prompt's fixed strings, in a JSON block) and examples.json (the owner's cases).
		Question string `json:"question"`
		Examples string `json:"examples"`
		// Checkpoints: after these held-line counts of a session, then every Every-th; when a held line has waited
		// WaitSeconds; a session ends SessionEndSeconds after the player's leave; a session that ends with ReviewHeld
		// held lines goes on the review queue.
		Checkpoints struct {
			Schedule          []int `json:"schedule"`
			Every             int   `json:"every"`
			WaitSeconds       int   `json:"wait_seconds"`
			SessionEndSeconds int   `json:"session_end_seconds"`
			ReviewHeld        int   `json:"review_held"`
		} `json:"checkpoints"`
		// Render: how a session is shown to the judge (core.Render).
		Render struct {
			Before     int               `json:"before"`
			After      int               `json:"after"`
			MaxChars   int               `json:"max_chars"`
			CutBefore  int               `json:"cut_before"`
			CutAfter   int               `json:"cut_after"`
			Categories map[string]string `json:"categories"`
		} `json:"render"`
	} `json:"judge"`
	// Links: the canonical link that replaces each link on the plugin's allowlist in the text the fast model sees,
	// the one the models were trained with, and the link pattern (core.LinkPattern).
	Links struct {
		Canonical string `json:"canonical"`
		Pattern   string `json:"pattern"`
	} `json:"links"`
	// Channels is the channels a line may have.
	Channels []string `json:"channels"`
	// Text: the numbers of the text preparation, which core/text.go implements.
	Text struct {
		Limits map[string]int `json:"limits"`
		URL    struct {
			Over, Keep int
			Mark       string
		} `json:"url"`
		FoldSeconds int    `json:"fold_seconds"`
		NameLimit   int    `json:"discord_name_limit"`
		CutMark     string `json:"cut_mark"`
		IPMark      string `json:"ip_mark"`
		Whitespace  []rune `json:"whitespace"`
	} `json:"text"`

	dir   string
	texts core.Texts
	// spec is the scoring part of bundle.json, as the fast model reads it; specErr why it cannot be read. A bundle
	// without one serves only with the test rig's scorer.
	spec    *model.Spec
	specErr error
}

type Manifest struct {
	Version string `json:"version"`
	Files   map[string]struct {
		SHA256 string `json:"sha256"`
		Bytes  int64  `json:"bytes"`
	} `json:"files"`
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// files is every file bundle.json names.
func (b *Bundle) files() []string {
	out := []string{b.Judge.Question, b.Judge.Examples}
	for _, g := range b.Graphs {
		out = append(out, g.XML, g.Bin)
	}
	for _, t := range b.Tokenizers {
		out = append(out, t.File)
	}
	return out
}

func loadBundle(dir string) (*Bundle, error) {
	var b Bundle
	raw, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "bundle.json"), err)
	}
	bad := func(format string, args ...any) (*Bundle, error) {
		return nil, fmt.Errorf("bundle %s: "+format, append([]any{b.Version}, args...)...)
	}
	if err := model.CheckRules(raw); err != nil {
		return bad("%w", err)
	}
	if err := b.checkText(); err != nil {
		return bad("%w", err)
	}
	for _, f := range b.files() {
		if f == "" || !filepath.IsLocal(f) {
			return bad("%q is not a file in the bundle", f)
		}
	}
	cp, r := b.Judge.Checkpoints, b.Judge.Render
	switch {
	case len(cp.Schedule) == 0 || cp.Every < 0 || cp.WaitSeconds < 0 || cp.SessionEndSeconds < 1 || cp.ReviewHeld < 0:
		return bad("judge.checkpoints needs schedule, every, wait_seconds, session_end_seconds and review_held")
	case r.Before < 0 || r.After < 0 || r.MaxChars < 1 || r.CutBefore < 0 || r.CutAfter < 0:
		return bad("judge.render needs before, after, max_chars, cut_before and cut_after")
	case strings.TrimSpace(b.Links.Canonical) == "":
		return bad("links.canonical is empty")
	}

	for _, n := range cp.Schedule {
		if n < 1 {
			return bad("judge.checkpoints.schedule counts must be at least 1")
		}
	}
	texts, err := readJudgeTexts(filepath.Join(dir, b.Judge.Question), filepath.Join(dir, b.Judge.Examples))
	if err != nil {
		return bad("%w", err)
	}
	b.dir, b.texts = dir, texts
	b.spec, b.specErr = model.ReadSpec(dir)
	return &b, nil
}

// checkText checks that the bundle's channels and the numbers of its text preparation are the ones core/text.go
// implements (the rules hash covers their words).
func (b *Bundle) checkText() error {
	t := b.Text
	var ws []rune
	for _, r := range core.Whitespace {
		for c := r[0]; c <= r[1]; c++ {
			ws = append(ws, c)
		}
	}
	switch {
	case len(b.Channels) == 0:
		return errors.New("channels is empty")
	case len(t.Limits) != 3 || t.Limits["book"] != core.LimitBook || t.Limits["nick"] != core.LimitNick ||
		t.Limits["default"] != core.LimitText:
		return fmt.Errorf("text.limits %v are not the ones this server implements", t.Limits)
	case t.URL.Over != core.URLOver || t.URL.Keep != core.URLKeep || t.URL.Mark != core.URLCut || t.CutMark != core.TextCut ||
		t.IPMark != core.IPMark:
		return errors.New("text.url, cut_mark or ip_mark are not the ones this server implements")
	case !slices.Equal(t.Whitespace, ws):
		return errors.New("text.whitespace is not the set this server implements")
	case t.FoldSeconds != int(core.RepeatWindow/time.Second) || t.NameLimit != core.LimitDiscord:
		return errors.New("text.fold_seconds or discord_name_limit is not the one this server implements")
	case b.Links.Pattern != core.LinkPattern:
		return errors.New("links.pattern is not the one this server implements")
	}
	return nil
}

// readJudgeTexts reads the judge's question and answer format from question.md's JSON block and the owner's cases
// from examples.json, and checks that the prompt's fixed strings are the ones the core lays the prompt out with.
func readJudgeTexts(questionPath, examplesPath string) (core.Texts, error) {
	var t core.Texts
	raw, err := os.ReadFile(questionPath)
	if err != nil {
		return t, err
	}
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	_, block, ok := strings.Cut(string(raw)+"\n", "\n```json\n")
	if ok {
		block, _, ok = strings.Cut(block, "\n```\n")
	}
	if !ok {
		return t, fmt.Errorf("%s has no ```json block", questionPath)
	}
	var fixed struct {
		Question  string `json:"QUESTION"`
		Added     string `json:"ADDED"`
		SepCases  string `json:"SEP_CASES"`
		BlockSep  string `json:"BLOCK_SEP"`
		SepNew    string `json:"SEP_NEW"`
		SepFormat string `json:"SEP_FMT"`
		Format    string `json:"FORMAT"`
	}
	if err := json.Unmarshal([]byte(block), &fixed); err != nil {
		return t, fmt.Errorf("%s: %w", questionPath, err)
	}
	if fixed.Question == "" || fixed.Format == "" {
		return t, fmt.Errorf("%s: QUESTION or FORMAT is empty", questionPath)
	}
	if fixed.SepCases != core.SepCases || fixed.BlockSep != core.BlockSep || fixed.SepNew != core.SepNew ||
		fixed.SepFormat != core.SepFormat {
		return t, fmt.Errorf("%s: the prompt's separators are not the ones this server lays the prompt out with", questionPath)
	}
	t.Question, t.Format = fixed.Question, fixed.Format
	if fixed.Added != "" {
		t.Question += "\n\n" + fixed.Added
	}
	var ex struct {
		Header   string         `json:"header"`
		Examples []core.Example `json:"examples"`
	}
	if err := readJSON(examplesPath, &ex); err != nil {
		return t, err
	}
	if ex.Header != core.ExampleHeader {
		return t, fmt.Errorf("%s: header %q is not the one this server writes, %q", examplesPath, ex.Header, core.ExampleHeader)
	}
	if len(ex.Examples) == 0 {
		return t, fmt.Errorf("%s lists no cases", examplesPath)
	}
	for i, e := range ex.Examples {
		if e.Ruling == "" || e.Text == "" {
			return t, fmt.Errorf("%s: case %d has no ruling or text", examplesPath, i+1)
		}
	}
	t.Examples = ex.Examples
	return t, nil
}

// The judge's caps: a prompt longer than maxPromptChars is not sent, and no more than maxCallsPerDay calls are made in
// a UTC day.
const (
	maxPromptChars = 500000
	maxCallsPerDay = 2000
)

// coreConfig is the decision core's configuration: the bundle's settings, with the judge's caps.
func coreConfig(cfg Config, b *Bundle) core.Config {
	cp, r := b.Judge.Checkpoints, b.Judge.Render
	sec := func(n int) time.Duration { return time.Duration(n) * time.Second }
	return core.Config{Schedule: cp.Schedule, Every: cp.Every, Wait: sec(cp.WaitSeconds),
		SessionEnd: sec(cp.SessionEndSeconds), ReviewHeld: cp.ReviewHeld,
		Render: core.Render{Before: r.Before, After: r.After, MaxChars: r.MaxChars, CutBefore: r.CutBefore,
			CutAfter: r.CutAfter, Categories: r.Categories},
		CanonicalLink: b.Links.Canonical, MaxPromptChars: maxPromptChars, MaxCallsPerDay: maxCallsPerDay,
		Channels: b.Channels, RetryAfter: orDefault(cfg.Test.JudgeRetryAfter, judgeRetryAfter)}
}

// ---------------------------------------------------------------- the bundle store
//
// <data>/bundles/<version>/ holds each installed bundle; <data>/state.json names the current and the previous one.
// Only those two are kept. An install is assembled in <data>/bundles/<version>.tmp/ and renamed into place once
// every file matches the manifest.

type State struct {
	Current  string `json:"current"`
	Previous string `json:"previous"`
	// The W&B artifact digest each installed bundle came from.
	Digests map[string]string `json:"digests,omitempty"`
	// A W&B production digest the updater does not install: the bundle an operator switched away from by going back
	// to the previous one. It is cleared when W&B's production digest changes, or by `ecc-server update`.
	Skip string `json:"skip,omitempty"`
}

func bundleDir(cfg Config, version string) string {
	return filepath.Join(cfg.Data, "bundles", version)
}

func readState(cfg Config) (State, error) {
	var st State
	err := readJSON(filepath.Join(cfg.Data, "state.json"), &st)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	return st, err
}

func writeState(cfg Config, st State) error {
	data, _ := json.MarshalIndent(st, "", " ")
	path := filepath.Join(cfg.Data, "state.json")
	if err := os.WriteFile(path+".tmp", append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// activate makes version current, the current one previous, and deletes every other installed bundle. When an
// operator switches back to the previous bundle, the W&B digest of the bundle left becomes the skipped one.
func activate(cfg Config, version string, byOperator bool) error {
	st, err := readState(cfg)
	if err != nil {
		return err
	}
	if st.Current != version {
		if byOperator && version == st.Previous && st.Digests[st.Current] != "" {
			st.Skip = st.Digests[st.Current]
		}
		st.Current, st.Previous = version, st.Current
	}
	for v := range st.Digests {
		if v != st.Current && v != st.Previous {
			delete(st.Digests, v)
		}
	}
	if err := writeState(cfg, st); err != nil {
		return err
	}
	// The switch is made once state.json names the bundle, so a bundle that cannot be deleted is only reported; the
	// next switch deletes it.
	entries, err := os.ReadDir(filepath.Join(cfg.Data, "bundles"))
	if err != nil {
		slog.Warn("could not list the installed bundles to delete old ones", "err", err)
		return nil
	}
	for _, e := range entries {
		// Keep install staging; each installer owns and removes its own directory.
		if e.Name() != st.Current && e.Name() != st.Previous && !strings.HasSuffix(e.Name(), ".tmp") {
			if err := os.RemoveAll(filepath.Join(cfg.Data, "bundles", e.Name())); err != nil {
				slog.Warn("could not delete an old bundle", "bundle", e.Name(), "err", err)
			}
		}
	}
	return nil
}

// removeStaleInstalls reclaims only staging whose owner has exited. The packaged CLI and daemon share a PID
// namespace. Unknown names and owners whose state cannot be checked stay in place.
func removeStaleInstalls(cfg Config) error {
	stale, err := filepath.Glob(filepath.Join(cfg.Data, "bundles", ".install-*.tmp"))
	for _, dir := range stale {
		owner, _, ok := strings.Cut(strings.TrimPrefix(filepath.Base(dir), ".install-"), "-")
		pid, parseErr := strconv.Atoi(owner)
		if !ok || parseErr != nil || pid < 1 || !installOwnerExited(pid) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	return err
}

// loadInstalledBundle verifies the stored bytes before any judge text or model is activated.
func loadInstalledBundle(cfg Config, version string) (*Bundle, error) {
	if version == "" || !filepath.IsLocal(version) || filepath.Base(version) != version || version == "." {
		return nil, fmt.Errorf("bundle version %q is not a plain name", version)
	}
	dir := bundleDir(cfg, version)
	var manifest Manifest
	if err := readJSON(filepath.Join(dir, "manifest.json"), &manifest); err != nil {
		return nil, err
	}
	if manifest.Version != version {
		return nil, fmt.Errorf("stored manifest version %q does not match requested %q", manifest.Version, version)
	}
	if _, ok := manifest.Files["bundle.json"]; !ok {
		return nil, fmt.Errorf("bundle %s: the manifest does not cover bundle.json", version)
	}
	if err := verifyBundle(dir, manifest); err != nil {
		return nil, err
	}
	b, err := loadBundle(dir)
	if err != nil {
		return nil, err
	}
	if b.Version != version {
		return nil, fmt.Errorf("stored bundle version %q does not match requested %q", b.Version, version)
	}
	for _, name := range b.files() {
		if _, ok := manifest.Files[name]; !ok {
			return nil, fmt.Errorf("bundle %s: the manifest does not cover %s", version, name)
		}
	}
	return b, nil
}

// installBundle copies a bundle folder into the store and checks every file against its manifest.
// It returns the version and whether the folder was newly installed.
func installBundle(cfg Config, src string) (string, bool, error) {
	var m Manifest
	if err := readJSON(filepath.Join(src, "manifest.json"), &m); err != nil {
		return "", false, err
	}
	if m.Version == "" || !filepath.IsLocal(m.Version) || filepath.Base(m.Version) != m.Version || m.Version == "." {
		return "", false, fmt.Errorf("%s: manifest version %q is not a plain name", src, m.Version)
	}
	dst := bundleDir(cfg, m.Version)
	loadFrom, existing := src, false
	if _, err := os.Stat(dst); err == nil {
		// W&B stages only the manifest and bundle.json when this version is installed.
		loadFrom, existing = dst, true
	}
	b, err := loadBundle(loadFrom)
	if err != nil {
		return "", false, err
	}
	if m.Version == "" || m.Version != b.Version || filepath.Base(m.Version) != m.Version {
		return "", false, fmt.Errorf("%s: manifest version %q does not match bundle.json's %q", src, m.Version, b.Version)
	}
	if _, ok := m.Files["bundle.json"]; !ok {
		return "", false, fmt.Errorf("%s: the manifest does not cover bundle.json", src)
	}
	for _, f := range b.files() {
		if _, ok := m.Files[f]; !ok {
			return "", false, fmt.Errorf("%s: the manifest does not cover %s", src, f)
		}
	}
	if existing {
		return m.Version, false, verifyBundle(dst, m)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", false, err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dst), fmt.Sprintf(".install-%d-*.tmp", os.Getpid()))
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(tmp)
	names := []string{"manifest.json"}
	for name := range m.Files {
		if err := checkBundleName(name); err != nil {
			return "", false, fmt.Errorf("%s: %w", src, err)
		}
		names = append(names, name)
	}
	for _, name := range names {
		dst := filepath.Join(tmp, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", false, err
		}
		if err := copyBundleFile(filepath.Join(src, filepath.FromSlash(name)), dst); err != nil {
			return "", false, err
		}
	}
	if err := verifyBundle(tmp, m); err != nil {
		return "", false, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		if _, existsErr := os.Stat(dst); existsErr == nil {
			// Another installer won. Accept only the same verified payload.
			return m.Version, false, verifyBundle(dst, m)
		}
		return "", false, err
	}
	return m.Version, true, nil
}

// checkBundleName accepts a manifest's file name: a path inside the bundle folder, "/" between folders.
func checkBundleName(name string) error {
	if name == "" || name == "manifest.json" || strings.Contains(name, "\\") || !filepath.IsLocal(filepath.FromSlash(name)) {
		return fmt.Errorf("manifest file %q is not a file name inside the bundle", name)
	}
	return nil
}

func verifyBundle(dir string, m Manifest) error {
	for name, want := range m.Files {
		if err := checkBundleName(name); err != nil {
			return err
		}
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != want.SHA256 || n != want.Bytes {
			return fmt.Errorf("%s: SHA-256 %s and %d bytes, the manifest says %s and %d bytes", name, got, n, want.SHA256, want.Bytes)
		}
	}
	return nil
}

func copyBundleFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
