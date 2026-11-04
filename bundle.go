package main

import (
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
)

// Bundle is bundle.json, written by the pipeline's `uv run bundle`.
type Bundle struct {
	Version string `json:"version"`
	GGUF    string `json:"gguf"`
	Platt   struct {
		A float64 `json:"a"`
		B float64 `json:"b"`
	} `json:"platt"`
	Prompt struct {
		Header       string   `json:"header"`
		WindowMin    int      `json:"window_min"`
		WindowMax    int      `json:"window_max"`
		WindowTokens int      `json:"window_tokens"`
		Answers      []string `json:"answers"`
		TopLogprobs  int      `json:"top_logprobs"`
	} `json:"prompt"`
	// Served to Module-ECC as written; the host does not interpret it.
	Decision json.RawMessage `json:"decision"`
	// The slow check's system prompt, which /converse sends to Bedrock, so the policy text stays out of Module-ECC's
	// public code.
	SlowCheck struct {
		System string `json:"system"`
	} `json:"slow_check"`
	LlamaCpp struct {
		Release string `json:"release"`
		Asset   string `json:"asset"`
		URL     string `json:"url"`
		SHA256  string `json:"sha256"`
	} `json:"llama_cpp"`
	LlamaServerArgs []string `json:"llama_server_args"`

	dir string
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

func loadBundle(dir string) (*Bundle, error) {
	var b Bundle
	if err := readJSON(filepath.Join(dir, "bundle.json"), &b); err != nil {
		return nil, err
	}
	p := b.Prompt
	switch {
	case len(p.Answers) != 2 || p.TopLogprobs < 2:
		return nil, fmt.Errorf("bundle %s: prompt needs two answers and top_logprobs of at least 2", b.Version)
	case p.WindowMin < 0 || p.WindowMax <= p.WindowMin || p.WindowTokens < 1:
		return nil, fmt.Errorf("bundle %s: invalid window bounds", b.Version)
	case b.GGUF == "" || filepath.Base(b.GGUF) != b.GGUF:
		return nil, fmt.Errorf("bundle %s: gguf must be a file name in the bundle", b.Version)
	case len(b.Decision) == 0:
		return nil, fmt.Errorf("bundle %s has no decision parameters", b.Version)
	case b.SlowCheck.System == "":
		return nil, fmt.Errorf("bundle %s: slow_check.system is missing or empty", b.Version)
	}
	b.dir = dir
	return &b, nil
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
	if _, ok := m.Files[b.GGUF]; !ok {
		return "", false, fmt.Errorf("%s: the manifest does not cover %s", src, b.GGUF)
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
