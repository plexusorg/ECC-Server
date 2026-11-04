package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// updater serves the bundle that W&B's production alias names. Each check resolves the alias; for a digest the host
// has not installed, it downloads the bundle into <data>/staging/<digest>/, installs it into the store (which checks
// every file against the bundle's manifest) and switches to it the way `use` does. Downloads resume across checks
// and restarts, and nothing in staging is a bundle until installBundle has verified it.
type updater struct {
	s   *server
	wb  *wandb
	run sync.Mutex // one check at a time

	// Guarded by run.
	failing bool   // the previous check failed to reach W&B or fetch from it; an outage is logged once
	failed  string // a production digest whose bundle failed a check; not fetched again until production changes

	mu sync.Mutex
	st wandbStatus
}

type wandbStatus struct {
	Artifact   string          `json:"artifact"`
	LastCheck  time.Time       `json:"last_check,omitzero"`
	Result     string          `json:"result,omitempty"`
	Production string          `json:"production,omitempty"`
	Digest     string          `json:"production_digest,omitempty"`
	Download   *downloadStatus `json:"download,omitempty"`
}

type downloadStatus struct {
	File  string `json:"file"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
}

// errBad marks a production bundle that failed a check: it is discarded and not fetched again until W&B's
// production digest changes or an operator runs `ecc-server update`.
type errBad struct{ error }

func (e errBad) Unwrap() error { return e.error }

func bad(format string, args ...any) error { return errBad{fmt.Errorf(format, args...)} }

func newUpdater(s *server, key string) *updater {
	u := &updater{s: s, wb: newWandb(key, s.cfg.WandbEntity, s.cfg.WandbProject)}
	u.st.Artifact = u.name()
	return u
}

func (u *updater) name() string {
	return fmt.Sprintf("%s/%s/%s:production", u.s.cfg.WandbEntity, u.s.cfg.WandbProject, u.s.cfg.WandbArtifact)
}

func (u *updater) loop(ctx context.Context) {
	for {
		_, _ = u.check(ctx, false)
		select {
		case <-ctx.Done():
			return
		case <-time.After(u.s.cfg.CheckInterval):
		}
	}
}

func (u *updater) status() *wandbStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.st
	if st.Download != nil {
		d := *st.Download
		st.Download = &d
	}
	return &st
}

// check brings the served bundle to W&B's production. force (`ecc-server update`) also installs a digest that a
// rollback skipped or that failed before.
func (u *updater) check(ctx context.Context, force bool) (string, error) {
	u.run.Lock()
	defer u.run.Unlock()
	if force {
		u.failed = ""
	}
	res, err := u.update(ctx, force)
	var b errBad
	unreachable := err != nil && !errors.As(err, &b)
	switch {
	case ctx.Err() != nil:
	case unreachable && !u.failing:
		slog.Warn("W&B check failed; serving the bundle on disk and retrying every check_interval", "err", err)
		u.failing = true
	case !unreachable && u.failing:
		slog.Info("W&B is reachable again")
		u.failing = false
	}
	u.mu.Lock()
	u.st.LastCheck = time.Now()
	u.st.Download = nil
	u.st.Result = res
	if err != nil {
		u.st.Result = "error: " + err.Error()
	}
	u.mu.Unlock()
	return res, err
}

func (u *updater) update(ctx context.Context, force bool) (string, error) {
	a, err := u.wb.artifact(ctx, u.s.cfg.WandbArtifact+":production")
	if err == nil && a == nil {
		err = fmt.Errorf("W&B has no %s", u.name())
	}
	u.mu.Lock()
	u.st.Production, u.st.Digest = "", ""
	if a != nil {
		u.st.Production, u.st.Digest = a.label(), a.Digest
	}
	u.mu.Unlock()
	if err != nil {
		return "", err
	}

	st, err := u.clearSkip(a.Digest, force)
	if err != nil {
		return "", err
	}
	switch {
	case st.Current != "" && st.Digests[st.Current] == a.Digest:
		return "serving production " + st.Current, nil
	case st.Skip == a.Digest:
		return fmt.Sprintf("serving %s: rolled back from production %s, which is skipped until W&B's production changes or ecc-server update", st.Current, a.label()), nil
	case u.failed == a.Digest:
		return "", bad("production %s failed a check and was discarded; ecc-server update tries it again", a.label())
	}

	version, err := u.fetch(ctx, a)
	var b errBad
	if errors.As(err, &b) {
		u.failed = a.Digest
		slog.Error("discarded the production bundle; the current one keeps serving", "production", a.label(), "reason", err)
	}
	if err != nil {
		return "", err
	}
	slog.Info("serving production from W&B", "bundle", version, "production", a.label())
	return "serving production " + version, nil
}

// clearSkip drops the rollback skip once production names another digest, or on `ecc-server update`.
func (u *updater) clearSkip(digest string, force bool) (State, error) {
	u.s.switchMu.Lock()
	defer u.s.switchMu.Unlock()
	st, err := readState(u.s.cfg)
	if err != nil || st.Skip == "" || (st.Skip == digest && !force) {
		return st, err
	}
	st.Skip = ""
	return st, writeState(u.s.cfg, st)
}

// fetch downloads production's bundle into staging, installs it and serves it. A bundle version already in the
// store (the previous one, or one `use` installed) is checked against production's manifest instead of downloaded
// again.
func (u *updater) fetch(ctx context.Context, a *wbArtifact) (string, error) {
	cfg := u.s.cfg
	staging := filepath.Join(cfg.Data, "staging")
	dir := filepath.Join(staging, a.Digest)
	// Only production's download is kept; one for an earlier production version is abandoned.
	entries, err := os.ReadDir(staging)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	for _, e := range entries {
		if e.Name() != a.Digest {
			if err := os.RemoveAll(filepath.Join(staging, e.Name())); err != nil {
				return "", err
			}
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for _, name := range []string{"manifest.json", "bundle.json"} {
		if err := u.download(ctx, a.Digest, name, dir); err != nil {
			return "", err
		}
	}
	var m Manifest
	if err := readJSON(filepath.Join(dir, "manifest.json"), &m); err != nil {
		os.RemoveAll(dir)
		return "", bad("%w", err)
	}
	if m.Version == "" || filepath.Base(m.Version) != m.Version {
		os.RemoveAll(dir)
		return "", bad("manifest version %q is not a plain name", m.Version)
	}
	if _, err := os.Stat(bundleDir(cfg, m.Version)); errors.Is(err, os.ErrNotExist) {
		for name := range m.Files {
			if filepath.Base(name) != name || name == "manifest.json" {
				os.RemoveAll(dir)
				return "", bad("manifest file %q is not a plain bundle file name", name)
			}
			if err := u.download(ctx, a.Digest, name, dir); err != nil {
				return "", err
			}
		}
	}

	u.s.switchMu.Lock()
	defer u.s.switchMu.Unlock()
	defer os.RemoveAll(dir)
	version, added, err := installBundle(cfg, dir)
	if err != nil {
		return "", bad("%w", err)
	}
	if err := u.s.serve(version, false); err != nil {
		if added {
			os.RemoveAll(bundleDir(cfg, version))
		}
		return "", bad("%w", err)
	}
	st, err := readState(cfg)
	if err != nil {
		return "", err
	}
	if st.Digests == nil {
		st.Digests = map[string]string{}
	}
	st.Digests[version] = a.Digest
	return version, writeState(cfg, st)
}

// download fetches one file of production into dir, with a download URL resolved just before it (they expire).
// A file whose content does not match W&B's digest is a bad bundle; any other failure leaves the partial file for
// the next check to resume.
func (u *updater) download(ctx context.Context, digest, name, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
		return nil
	}
	a, err := u.wb.artifact(ctx, u.s.cfg.WandbArtifact+":production")
	if err != nil {
		return err
	}
	if a == nil || a.Digest != digest {
		return errors.New("W&B's production changed during the download")
	}
	f, ok := a.file(name)
	if !ok {
		os.RemoveAll(dir)
		return bad("production has no %s", name)
	}
	err = u.wb.download(ctx, f, dir, func(done int64) {
		u.mu.Lock()
		u.st.Download = &downloadStatus{File: name, Done: done, Total: f.SizeBytes}
		u.mu.Unlock()
	})
	if errors.Is(err, errCorrupt) {
		os.RemoveAll(dir)
		return errBad{err}
	}
	return err
}
