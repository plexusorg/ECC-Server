package main

import (
	"context"
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
	if st.Digests[st.Current] != "" {
		fmt.Printf("%s came from W&B; the host installs it again only when W&B's production changes or you run ecc-server update\n", st.Current)
	}
	return nil
}

// switchTo has a running host switch to an installed bundle, which it activates once the bundle loads; with no host
// running it activates the bundle for the next start.
func switchTo(cfg Config, version string) error {
	token, err := readToken(cfg)
	var resp *http.Response
	if err == nil {
		body, _ := json.Marshal(map[string]string{"version": version})
		resp, err = hostRequest(cfg, token, http.MethodPost, "/switch", body, http.DefaultClient)
	}
	// no token file: the host has never started
	if errors.Is(err, os.ErrNotExist) || noHost(err) {
		fmt.Println("no host is running; the bundle is served from its next start")
		return activate(cfg, version, true)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errRejected
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("the host did not switch to %s: %s", version, strings.TrimSpace(string(raw)))
	}
	return nil
}

// noHost reports a request that failed because nothing listens on the host's address.
func noHost(err error) bool {
	var dial *net.OpError
	return errors.As(err, &dial) && dial.Op == "dial"
}

// wandbLogin checks a W&B API key with W&B and stores it encrypted for the service.
func wandbLogin() error {
	key, err := readKey("wandb", "W&B API key (https://wandb.ai/authorize): ")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	user, err := newWandb(key, "", "").viewer(ctx)
	if err != nil {
		return fmt.Errorf("checking the key with W&B: %w", err)
	}
	if err := storeCredential(wandbCredential, key); err != nil {
		return err
	}
	fmt.Printf("stored the W&B key of %s, encrypted, in %s\n", user, credFile(wandbCredential))
	return restartService()
}

// bedrockLogin checks a Bedrock API key with a small Converse call to the configured model, which proves both the
// key and its access to the model, and stores it encrypted for the service.
func bedrockLogin(cfg Config) error {
	key, err := readKey("bedrock", "Amazon Bedrock API key: ")
	if err != nil {
		return err
	}
	b := newBedrock(key, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := b.converse(ctx, "", "Hi", loginCheckMaxTokens, cfg.Judge.ServiceTier); err != nil {
		return fmt.Errorf("checking the key with a call to %s in %s: %w", cfg.Judge.Model, cfg.Judge.Region, err)
	}
	if err := storeCredential(bedrockCredential, key); err != nil {
		return err
	}
	fmt.Printf("the key can call %s in %s; stored it, encrypted, in %s\n", cfg.Judge.Model, cfg.Judge.Region, credFile(bedrockCredential))
	return restartService()
}

// status prints the bundle a running host serves, its health, its W&B updates, its judge calls, the judge's spend
// and its sittings.
func status(cfg Config) error {
	token, err := readToken(cfg)
	if err != nil {
		return err
	}
	resp, err := hostRequest(cfg, token, http.MethodGet, "/status", nil, &http.Client{Timeout: 5 * time.Second})
	if noHost(err) {
		return fmt.Errorf("no host is listening on %s", hostAddr(cfg))
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errRejected
	}
	var h hostStatus
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return fmt.Errorf("unexpected answer from %s: %w", hostAddr(cfg), err)
	}
	if h.Version != "" {
		fmt.Printf("bundle: %s\n", h.Version)
	}
	fmt.Printf("health: %s\n", h.Status)
	fmt.Printf("scorer: %s\n", h.Scorer)
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
	if b := h.Judge; b != nil {
		key := "key set"
		if !b.KeySet {
			key = "no key set (sudo ecc-server bedrock login)"
		}
		fmt.Printf("judge: %s in %s, %s tier, %s\n", b.Model, b.Region, b.Tier, key)
		if b.LastCall.IsZero() {
			fmt.Println("last judge call: none since the host started")
		} else {
			fmt.Printf("last judge call: %s (%s ago): %s\n", b.LastCall.Format(time.DateTime),
				time.Since(b.LastCall).Round(time.Second), b.Result)
		}
	}
	sp := h.Spend
	fmt.Printf("spend: %s $%.4f of $%.2f (%d calls); %s $%.4f of $%.2f", sp.Day, sp.DayUSD, sp.DailyBudget, sp.Calls,
		sp.Month, sp.MonthUSD, sp.MonthlyBudget)
	if sp.ReservedUSD > 0 {
		fmt.Printf("; $%.4f of it reserved for calls in flight", sp.ReservedUSD)
	}
	fmt.Println()
	st := h.Sittings
	fmt.Printf("sittings: %d open, %d held lines unruled, %d judge calls in flight\n", st.Sittings, st.Unruled, st.InFlight)
	fmt.Printf("unacknowledged actions: %d\n", h.Unacknowledged)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the host is not ready")
	}
	return nil
}

// update has the running host check W&B now and install production if it is not served, waiting for the result.
func update(cfg Config) error {
	token, err := readToken(cfg)
	if err != nil {
		return err
	}
	fmt.Println("checking W&B; a new production bundle is downloaded and started before this returns")
	resp, err := hostRequest(cfg, token, http.MethodPost, "/update", nil, http.DefaultClient)
	if noHost(err) {
		return fmt.Errorf("no host is listening on %s", hostAddr(cfg))
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errRejected
	}
	var out struct {
		Result string `json:"result"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("unexpected answer from %s: %w", hostAddr(cfg), err)
	}
	if resp.StatusCode != http.StatusOK {
		return errors.New(out.Error)
	}
	fmt.Println(out.Result)
	return nil
}
