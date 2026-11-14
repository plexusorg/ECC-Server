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
		// Checkpoints: after these held-line counts of a sitting, then every Every-th; when a held line has waited
		// WaitSeconds; a sitting ends SittingEndSeconds after the player's leave; a sitting that ends with ReviewHeld
		// held lines goes on the review queue.
		Checkpoints struct {
			Schedule          []int `json:"schedule"`
			Every             int   `json:"every"`
			WaitSeconds       int   `json:"wait_seconds"`
			SittingEndSeconds int   `json:"sitting_end_seconds"`
			ReviewHeld        int   `json:"review_held"`
		} `json:"checkpoints"`
		// Render: how a sitting is shown to the judge (core.Render).
		Render struct {
			Before    int               `json:"before"`
			After     int               `json:"after"`
			MaxChars  int               `json:"max_chars"`
			CutBefore int               `json:"cut_before"`
			CutAfter  int               `json:"cut_after"`
			Kinds     map[string]string `json:"kinds"`
		} `json:"render"`
	} `json:"judge"`
	// Links: the canonical link that replaces each link on the plugin's allowlist in the text the fast model sees,
	// the one the models were trained with.
	Links struct {
		Canonical string `json:"canonical"`
	} `json:"links"`

	dir   string
	texts core.Texts
	// spec is the scoring part of bundle.json, as the fast model reads it.
	spec *model.Spec
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
	if err := readJSON(filepath.Join(dir, "bundle.json"), &b); err != nil {
		return nil, err
	}
	bad := func(format string, args ...any) (*Bundle, error) {
		return nil, fmt.Errorf("bundle %s: "+format, append([]any{b.Version}, args...)...)
	}
	for _, f := range b.files() {
		if f == "" || !filepath.IsLocal(f) {
			return bad("%q is not a file in the bundle", f)
		}
	}
	cp, r := b.Judge.Checkpoints, b.Judge.Render
	switch {
	case len(cp.Schedule) == 0 || cp.Every < 0 || cp.WaitSeconds < 0 || cp.SittingEndSeconds < 1 || cp.ReviewHeld < 0:
		return bad("judge.checkpoints needs schedule, every, wait_seconds, sitting_end_seconds and review_held")
	case r.Before < 0 || r.After < 0 || r.MaxChars < 1 || r.CutBefore < 0 || r.CutAfter < 0:
		return bad("judge.render needs before, after, max_chars, cut_before and cut_after")
	case strings.TrimSpace(b.Links.Canonical) == "":
		return bad("links.canonical is empty")
	}
	spec, err := model.ReadSpec(dir)
	if err != nil {
		return bad("%w", err)
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
	b.dir, b.texts, b.spec = dir, texts, spec
	return &b, nil
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

// coreConfig is the decision core's configuration: the bundle's settings, with the config's caps and any checkpoint
// settings it sets.
func coreConfig(cfg Config, b *Bundle) core.Config {
	cp, r := b.Judge.Checkpoints, b.Judge.Render
	sec := func(n int) time.Duration { return time.Duration(n) * time.Second }
	c := core.Config{Schedule: cp.Schedule, Every: cp.Every, Wait: sec(cp.WaitSeconds),
		SittingEnd: sec(cp.SittingEndSeconds), ReviewHeld: cp.ReviewHeld,
		Render: core.Render{Before: r.Before, After: r.After, MaxChars: r.MaxChars, CutBefore: r.CutBefore,
			CutAfter: r.CutAfter, Kinds: r.Kinds},
		CanonicalLink: b.Links.Canonical, MaxPromptChars: cfg.Budget.MaxPromptChars, MaxCallsPerDay: cfg.Budget.MaxCallsPerDay, Zone: time.UTC}
	if cfg.Chat.setSchedule {
		c.Schedule = cfg.Chat.Schedule
	}
	if cfg.Chat.setEvery {
		c.Every = cfg.Chat.Every
	}
	if cfg.Chat.setWait {
		c.Wait = cfg.Chat.Wait
	}
	return c
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
		// An install in progress (another process's `use`) keeps its .tmp folder; installBundle replaces a stale one.
		if e.Name() != st.Current && e.Name() != st.Previous && !strings.HasSuffix(e.Name(), ".tmp") {
			if err := os.RemoveAll(filepath.Join(cfg.Data, "bundles", e.Name())); err != nil {
				slog.Warn("could not delete an old bundle", "bundle", e.Name(), "err", err)
			}
		}
	}
	return nil
}

// removeStaleInstalls deletes the .tmp folders of installs a stopped process left unfinished.
func removeStaleInstalls(cfg Config) error {
	stale, err := filepath.Glob(filepath.Join(cfg.Data, "bundles", "*.tmp"))
	for _, dir := range stale {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	return err
}

// installBundle copies (or hardlinks) a bundle folder into the store and checks every file against its manifest.
// It returns the version and whether the folder was newly installed.
func installBundle(cfg Config, src string) (string, bool, error) {
	var m Manifest
	if err := readJSON(filepath.Join(src, "manifest.json"), &m); err != nil {
		return "", false, err
	}
	b, err := loadBundle(src)
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
	dst := bundleDir(cfg, m.Version)
	if _, err := os.Stat(dst); err == nil {
		return m.Version, false, verifyBundle(dst, m)
	}
	tmp := dst + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", false, err
	}
	names := []string{"manifest.json"}
	for name := range m.Files {
		if filepath.Base(name) != name {
			return "", false, fmt.Errorf("%s: manifest file %q is not a plain file name", src, name)
		}
		names = append(names, name)
	}
	for _, name := range names {
		if err := linkOrCopy(filepath.Join(src, name), filepath.Join(tmp, name)); err != nil {
			os.RemoveAll(tmp)
			return "", false, err
		}
	}
	if err := verifyBundle(tmp, m); err != nil {
		os.RemoveAll(tmp)
		return "", false, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", false, err
	}
	return m.Version, true, nil
}

func verifyBundle(dir string, m Manifest) error {
	for name, want := range m.Files {
		f, err := os.Open(filepath.Join(dir, name))
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

func linkOrCopy(src, dst string) error {
	if os.Link(src, dst) == nil {
		return nil
	}
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
