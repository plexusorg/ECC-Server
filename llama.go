package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// llama is one llama-server child process serving one bundle's GGUF on a loopback port.
type llama struct {
	url    string
	cmd    *exec.Cmd
	exited chan struct{}
	client *http.Client
}

const llamaReadyTimeout = 10 * time.Minute

func startLlama(cfg Config, b *Bundle) (*llama, error) {
	bin := cfg.LlamaServer
	if bin == "" {
		var err error
		if bin, err = llamaRelease(cfg, b); err != nil {
			return nil, err
		}
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	args := append([]string{"-m", filepath.Join(b.dir, b.GGUF), "--host", "127.0.0.1", "--port", strconv.Itoa(port),
		"-t", strconv.Itoa(cfg.Threads), "--no-webui"}, b.LlamaServerArgs...)
	cmd := exec.Command(bin, args...)
	cmd.Dir = filepath.Dir(bin)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	cmd.SysProcAttr = childAttr()
	slog.Info("starting llama-server", "bundle", b.Version, "bin", bin, "args", strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	l := &llama{url: "http://127.0.0.1:" + strconv.Itoa(port), cmd: cmd, exited: make(chan struct{}),
		client: &http.Client{Timeout: 2 * time.Minute}}
	go func() {
		_ = cmd.Wait()
		close(l.exited)
	}()
	deadline := time.Now().Add(llamaReadyTimeout)
	for !l.healthy() {
		select {
		case <-l.exited:
			return nil, fmt.Errorf("llama-server exited during startup: %v", cmd.ProcessState)
		case <-time.After(500 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			l.stop()
			return nil, fmt.Errorf("llama-server was not healthy within %v", llamaReadyTimeout)
		}
	}
	return l, nil
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func (l *llama) healthy() bool {
	resp, err := l.client.Get(l.url + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (l *llama) stop() {
	stopChild(l.cmd, l.exited)
}

func (l *llama) post(path string, body, out any) error {
	data, _ := json.Marshal(body)
	resp, err := l.client.Post(l.url+path, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("llama-server %s: %s: %s", path, resp.Status, raw)
	}
	return json.Unmarshal(raw, out)
}

// tokenize returns the token ids of text: no special tokens added, special-token text parsed, as the pipeline's
// Hugging Face tokenizer counts (encode with add_special_tokens=False).
func (l *llama) tokenize(text string) ([]int, error) {
	var out struct {
		Tokens []int `json:"tokens"`
	}
	err := l.post("/tokenize", map[string]any{"content": text}, &out)
	return out.Tokens, err
}

type completion struct {
	Probs []struct {
		TopLogprobs []struct {
			ID      int     `json:"id"`
			Logprob float64 `json:"logprob"`
		} `json:"top_logprobs"`
	} `json:"completion_probabilities"`
	Timings struct {
		PromptN int `json:"prompt_n"`
	} `json:"timings"`
}

// complete runs the prompt to its next token and returns the top n logprobs there. cache_prompt reuses the slot's
// cached prefix, so a conversation's next prompt decodes only its new line.
func (l *llama) complete(prompt string, n int) (*completion, error) {
	var out completion
	err := l.post("/completion", map[string]any{"prompt": prompt, "n_predict": 1, "temperature": 0, "n_probs": n,
		"cache_prompt": true}, &out)
	if err == nil && len(out.Probs) == 0 {
		err = errors.New("llama-server returned no completion probabilities")
	}
	return &out, err
}

// ---------------------------------------------------------------- the llama.cpp release

// llamaRelease returns the llama-server of the bundle's llama.cpp release, downloading and unpacking the release
// asset into <data>/llama/<release> after checking its SHA-256.
func llamaRelease(cfg Config, b *Bundle) (string, error) {
	r := b.LlamaCpp
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("bundle %s names the Linux llama.cpp asset %s; set llama_server in the config on %s", b.Version, r.Asset, runtime.GOOS)
	}
	if r.Release == "" || filepath.Base(r.Release) != r.Release || r.URL == "" || r.SHA256 == "" {
		return "", fmt.Errorf("bundle %s: llama_cpp needs release, url and sha256", b.Version)
	}
	dir := filepath.Join(cfg.Data, "llama", r.Release)
	if bin, err := findFile(dir, "llama-server"); err == nil {
		return bin, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	archive := dir + ".tar.gz"
	slog.Info("downloading llama.cpp", "release", r.Release, "url", r.URL)
	if err := download(r.URL, archive, r.SHA256); err != nil {
		return "", err
	}
	defer os.Remove(archive)
	tmp := dir + ".tmp"
	os.RemoveAll(tmp)
	if err := untar(archive, tmp); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	return findFile(dir, "llama-server")
}

func download(url, path, sha string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != strings.ToLower(sha) {
		err = fmt.Errorf("download %s: SHA-256 %x, the bundle says %s", url, h.Sum(nil), sha)
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

// untar unpacks a .tar.gz into dir, keeping file modes and the relative symlinks llama.cpp's shared libraries use.
func untar(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(h.Name))
		if !filepath.IsLocal(name) {
			return fmt.Errorf("%s: unsafe path %q", archive, h.Name)
		}
		path := filepath.Join(dir, name)
		switch h.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(path, 0o755)
		case tar.TypeReg:
			err = writeFile(path, tr, fs.FileMode(h.Mode)&0o777)
		case tar.TypeSymlink:
			if !filepath.IsLocal(filepath.Join(filepath.Dir(name), h.Linkname)) {
				return fmt.Errorf("%s: unsafe link %q -> %q", archive, h.Name, h.Linkname)
			}
			if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
				err = os.Symlink(h.Linkname, path)
			}
		}
		if err != nil {
			return err
		}
	}
}

func writeFile(path string, r io.Reader, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func findFile(dir, name string) (string, error) {
	found := ""
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() && d.Name() == name {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	if err == nil && found == "" {
		err = fmt.Errorf("%s has no %s", dir, name)
	}
	return found, err
}
