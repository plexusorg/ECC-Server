package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
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
		return activate(cfg, version)
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

// status prints the bundle a running host serves and its health.
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
	var h struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return fmt.Errorf("unexpected answer from %s: %w", cfg.Listen, err)
	}
	if h.Version != "" {
		fmt.Printf("bundle: %s\n", h.Version)
	}
	fmt.Printf("health: %s\n", h.Status)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the host is not ready")
	}
	return nil
}
