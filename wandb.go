package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The W&B API key reaches the service as a systemd credential (LoadCredentialEncrypted= in the unit), which
// `ecc-server wandb login` encrypts into the credential store.
const (
	wandbCredential = "ecc-server.wandb-api-key"
	wandbCredStore  = "/etc/credstore.encrypted"
	wandbCredFile   = wandbCredStore + "/" + wandbCredential
	wandbGraphQL    = "https://api.wandb.ai/graphql"
)

// wandbKey reads the W&B API key from the service's credentials; "" when none is set.
func wandbKey() (string, error) {
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if dir == "" {
		return "", nil
	}
	data, err := os.ReadFile(filepath.Join(dir, wandbCredential))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(data)), err
}

type wandb struct {
	key    string
	api    *http.Client
	files  *http.Client
	entity string
	proj   string
}

func newWandb(key, entity, project string) *wandb {
	return &wandb{
		key:    key,
		api:    &http.Client{Timeout: time.Minute},
		files:  &http.Client{},
		entity: entity,
		proj:   project,
	}
}

// query runs a GraphQL query. W&B answers GraphQL errors with HTTP 200 and a top-level errors array.
func (w *wandb) query(ctx context.Context, query string, vars map[string]any, out any) error {
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wandbGraphQL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("api:"+w.key)))
	resp, err := w.api.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("W&B API: %s: %s", resp.Status, truncate(strings.TrimSpace(string(raw)), 200))
	}
	var r struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("W&B API: %w", err)
	}
	if len(r.Errors) > 0 {
		msgs := make([]string, len(r.Errors))
		for i, e := range r.Errors {
			msgs[i] = e.Message
		}
		return fmt.Errorf("W&B API: %s", strings.Join(msgs, "; "))
	}
	return json.Unmarshal(r.Data, out)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// viewer returns the user name the key belongs to.
func (w *wandb) viewer(ctx context.Context) (string, error) {
	var out struct {
		Viewer *struct {
			Username string `json:"username"`
		} `json:"viewer"`
	}
	if err := w.query(ctx, `query { viewer { username } }`, nil, &out); err != nil {
		return "", err
	}
	if out.Viewer == nil {
		return "", errors.New("W&B does not recognize the key")
	}
	return out.Viewer.Username, nil
}

type wbArtifact struct {
	Digest       string `json:"digest"`
	VersionIndex int    `json:"versionIndex"`
	Aliases      []struct {
		Alias string `json:"alias"`
	} `json:"aliases"`
	Files struct {
		PageInfo struct {
			HasNextPage bool `json:"hasNextPage"`
		} `json:"pageInfo"`
		Edges []struct {
			Node wbFile `json:"node"`
		} `json:"edges"`
	} `json:"files"`
}

type wbFile struct {
	Name      string `json:"name"`
	Digest    string `json:"digest"` // base64 of the file's MD5
	SizeBytes int64  `json:"sizeBytes"`
	DirectURL string `json:"directUrl"` // pre-signed; expires, so it is fetched fresh for each download
}

// label is the artifact version as W&B lists it: its version index and its aliases.
func (a *wbArtifact) label() string {
	names := make([]string, len(a.Aliases))
	for i, al := range a.Aliases {
		names[i] = al.Alias
	}
	return fmt.Sprintf("v%d (%s)", a.VersionIndex, strings.Join(names, ", "))
}

func (a *wbArtifact) file(name string) (wbFile, bool) {
	for _, e := range a.Files.Edges {
		if e.Node.Name == name {
			return e.Node, true
		}
	}
	return wbFile{}, false
}

const artifactQuery = `query($e: String!, $p: String!, $n: String!) {
  project(entityName: $e, name: $p) {
    artifact(name: $n) {
      digest versionIndex aliases { alias }
      files(first: 100) { pageInfo { hasNextPage } edges { node { name digest sizeBytes directUrl } } }
    }
  }
}`

// artifact resolves name ("artifact:alias"); nil when the project has no such alias.
func (w *wandb) artifact(ctx context.Context, name string) (*wbArtifact, error) {
	var out struct {
		Project *struct {
			Artifact *wbArtifact `json:"artifact"`
		} `json:"project"`
	}
	if err := w.query(ctx, artifactQuery, map[string]any{"e": w.entity, "p": w.proj, "n": name}, &out); err != nil {
		return nil, err
	}
	if out.Project == nil {
		return nil, fmt.Errorf("W&B has no project %s/%s visible to this key", w.entity, w.proj)
	}
	a := out.Project.Artifact
	if a != nil && a.Files.PageInfo.HasNextPage {
		return nil, fmt.Errorf("%s has more than 100 files; a bundle has three", name)
	}
	return a, nil
}

// errCorrupt marks a downloaded file whose content does not match W&B's digest.
var errCorrupt = errors.New("corrupt download")

// download fetches f into dir/<name>. The bytes go to dir/<name>.part first, which a later call resumes with a
// range request; the file takes its name only once its size and MD5 match W&B's. progress gets the bytes written
// so far.
func (w *wandb) download(ctx context.Context, f wbFile, dir string, progress func(done int64)) error {
	final := filepath.Join(dir, f.Name)
	if _, err := os.Stat(final); err == nil {
		return nil
	}
	want, err := base64.StdEncoding.DecodeString(f.Digest)
	if err != nil || len(want) != md5.Size {
		return fmt.Errorf("%s: W&B digest %q is not a base64 MD5", f.Name, f.Digest)
	}
	part := final + ".part"
	out, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	h := md5.New()
	have, err := io.Copy(h, out)
	if err != nil {
		return err
	}
	if have > f.SizeBytes {
		if have, err = restart(out, h); err != nil {
			return err
		}
	}
	progress(have)
	if have < f.SizeBytes {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.DirectURL, nil)
		if err != nil {
			return err
		}
		if have > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
		}
		// The stall timer covers connecting and the response headers as well as the body.
		stall := time.AfterFunc(stallTimeout, cancel)
		defer stall.Stop()
		resp, err := w.files.Do(req)
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL's signature grants access to the file, so it stays out of logs and status
		}
		if err != nil {
			return fmt.Errorf("download %s: %w", f.Name, err)
		}
		defer resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusPartialContent && have > 0 &&
			strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", have)):
		case resp.StatusCode == http.StatusOK:
			if have, err = restart(out, h); err != nil {
				return err
			}
		default:
			return fmt.Errorf("download %s: %s", f.Name, resp.Status)
		}
		dst := &countWriter{w: io.MultiWriter(out, h), n: have, progress: progress}
		if _, err := io.Copy(dst, &stallReader{r: resp.Body, timer: stall}); err != nil {
			return fmt.Errorf("download %s: %w", f.Name, err)
		}
		have = dst.n
	}
	if have != f.SizeBytes {
		return fmt.Errorf("download %s: got %d bytes, W&B lists %d", f.Name, have, f.SizeBytes)
	}
	if got := h.Sum(nil); !bytes.Equal(got, want) {
		out.Close()
		os.Remove(part)
		return fmt.Errorf("%w: %s has MD5 %s, W&B lists %s", errCorrupt, f.Name, base64.StdEncoding.EncodeToString(got), f.Digest)
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(part, final)
}

// restart empties a partial download whose bytes cannot be resumed.
func restart(f *os.File, h hash.Hash) (int64, error) {
	h.Reset()
	if err := f.Truncate(0); err != nil {
		return 0, err
	}
	_, err := f.Seek(0, io.SeekStart)
	return 0, err
}

// A download that receives nothing for this long is abandoned; the next check resumes it.
const stallTimeout = 2 * time.Minute

type stallReader struct {
	r     io.Reader
	timer *time.Timer
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(stallTimeout)
	}
	return n, err
}

type countWriter struct {
	w        io.Writer
	n        int64
	progress func(int64)
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	c.progress(c.n)
	return n, err
}
