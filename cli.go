package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func useBundle(cfg Config, folder string) error {
	st, err := readState(cfg)
	if err != nil {
		return err
	}
	version, added, err := installBundle(cfg, folder)
	if err != nil {
		return err
	}
	if version == st.Current {
		fmt.Printf("%s is already the current bundle\n", version)
		return nil
	}
	if err := switchTo(cfg, version); err != nil {
		if added {
			os.RemoveAll(bundleDir(cfg, version))
		}
		return err
	}
	fmt.Printf("now serving %s\n", version)
	return nil
}

func rollback(cfg Config) error {
	st, err := readState(cfg)
	if err != nil {
		return err
	}
	if st.Previous == "" {
		return errors.New("there is no previous bundle")
	}
	if err := switchTo(cfg, st.Previous); err != nil {
		return err
	}
	fmt.Printf("now serving %s (rolled back from %s)\n", st.Previous, st.Current)
	if st.Digests[st.Current] != "" {
		fmt.Printf("%s came from W&B; the host installs it again only when W&B's production changes or you run ecc-server update\n", st.Current)
	}
	return nil
}

// switchTo has a running host switch to an installed bundle, which it activates once the bundle's llama-server is
// healthy; with no host running it activates the bundle for the next start.
func switchTo(cfg Config, version string) error {
	body, _ := json.Marshal(map[string]string{"version": version})
	resp, err := http.Post("http://"+cfg.Listen+"/switch", "application/json", bytes.NewReader(body))
	var dial *net.OpError
	if errors.As(err, &dial) && dial.Op == "dial" {
		fmt.Println("no host is running; the bundle is served from its next start")
		return activate(cfg, version, true)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("the host did not switch to %s: %s", version, strings.TrimSpace(string(raw)))
	}
	return nil
}

// wandbLogin checks a W&B API key with W&B and stores it encrypted with systemd-creds (the host key, and the TPM2
// when there is one), where only the service manager can decrypt it for the unit's LoadCredentialEncrypted=. The
// key never touches the disk in plaintext.
func wandbLogin() error {
	if runtime.GOOS != "linux" {
		return errors.New("wandb login stores the key with systemd-creds, on Linux")
	}
	if os.Geteuid() != 0 {
		return errors.New("run wandb login as root: sudo ecc-server wandb login")
	}
	key, err := readSecret("W&B API key (https://wandb.ai/authorize): ")
	if err != nil {
		return err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("no key was entered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	user, err := newWandb(key, "", "").viewer(ctx)
	if err != nil {
		return fmt.Errorf("checking the key with W&B: %w", err)
	}
	cmd := exec.Command("systemd-creds", "encrypt", "--name="+wandbCredential, "-", "-")
	cmd.Stdin = strings.NewReader(key)
	cmd.Stderr = os.Stderr
	encrypted, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("systemd-creds encrypt: %w", err)
	}
	if err := os.MkdirAll(wandbCredStore, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(wandbCredFile+".tmp", encrypted, 0o600); err != nil {
		return err
	}
	if err := os.Rename(wandbCredFile+".tmp", wandbCredFile); err != nil {
		return err
	}
	fmt.Printf("stored the W&B key of %s, encrypted, in %s\n", user, wandbCredFile)
	return restartService()
}

func wandbLogout() error {
	if os.Geteuid() != 0 {
		return errors.New("run wandb logout as root: sudo ecc-server wandb logout")
	}
	err := os.Remove(wandbCredFile)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Println("no W&B key is stored")
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Printf("removed %s\n", wandbCredFile)
	return restartService()
}

// restartService has a running service pick up the stored key, or its removal.
func restartService() error {
	cmd := exec.Command("systemctl", "try-restart", "ecc-server.service")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl try-restart ecc-server.service: %w", err)
	}
	fmt.Println("restarted ecc-server.service if it was running; a stopped service reads the key when it starts")
	return nil
}

// status prints the bundle a running host serves, its health and its W&B updates.
func status(cfg Config) error {
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + cfg.Listen + "/health")
	var dial *net.OpError
	if errors.As(err, &dial) && dial.Op == "dial" {
		return fmt.Errorf("no host is listening on %s", cfg.Listen)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var h health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return fmt.Errorf("unexpected answer from %s: %w", cfg.Listen, err)
	}
	if h.Version != "" {
		fmt.Printf("bundle: %s\n", h.Version)
	}
	fmt.Printf("health: %s\n", h.Status)
	if w := h.Wandb; w == nil {
		fmt.Println("W&B: no key set (sudo ecc-server wandb login)")
	} else {
		fmt.Printf("W&B: %s\n", w.Artifact)
		if w.LastCheck.IsZero() {
			fmt.Println("last check: running")
		} else {
			fmt.Printf("last check: %s (%s ago): %s\n", w.LastCheck.Format(time.DateTime),
				time.Since(w.LastCheck).Round(time.Second), w.Result)
		}
		if w.Production != "" {
			fmt.Printf("production: %s, digest %s\n", w.Production, w.Digest)
		}
		if d := w.Download; d != nil {
			fmt.Printf("downloading: %s, %.1f of %.1f MB\n", d.File, float64(d.Done)/1e6, float64(d.Total)/1e6)
		}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the host is not ready")
	}
	return nil
}

// update has the running host check W&B now and install production if it is not served, waiting for the result.
func update(cfg Config) error {
	fmt.Println("checking W&B; a new production bundle is downloaded and started before this returns")
	resp, err := http.Post("http://"+cfg.Listen+"/update", "application/json", nil)
	var dial *net.OpError
	if errors.As(err, &dial) && dial.Op == "dial" {
		return fmt.Errorf("no host is listening on %s", cfg.Listen)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Result string `json:"result"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("unexpected answer from %s: %w", cfg.Listen, err)
	}
	if resp.StatusCode != http.StatusOK {
		return errors.New(out.Error)
	}
	fmt.Println(out.Result)
	return nil
}
