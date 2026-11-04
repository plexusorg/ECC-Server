package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Every HTTP endpoint needs the shared token, `Authorization: Bearer <token>`: the endpoints decide who is banned, and
// the listen address can be reachable from other machines. The host creates <data>/token on its first start (mode
// 0600, owned by the service user); `sudo ecc-server token` prints it for the Module-ECC config, and the commands that
// talk to the running host read it. The token is never logged.

func tokenPath(cfg Config) string { return filepath.Join(cfg.Data, "token") }

// loadOrCreateToken returns the host's token, creating the file when it does not exist.
func loadOrCreateToken(cfg Config) (string, error) {
	path := tokenPath(cfg)
	token, err := readToken(cfg)
	if err == nil {
		return token, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(cfg.Data, 0o750); err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token = hex.EncodeToString(raw)
	_ = os.Remove(path + ".tmp")
	f, err := os.OpenFile(path+".tmp", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return "", err
	}
	return token, nil
}

// readToken reads the token file; an error wrapping os.ErrNotExist means the host has never started.
func readToken(cfg Config) (string, error) {
	data, err := os.ReadFile(tokenPath(cfg))
	if err != nil {
		return "", fmt.Errorf("cannot read the token file %s (run with sudo, or as the ecc-server user): %w", tokenPath(cfg), err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("the token file %s is empty; delete it and restart the service to create a new one", tokenPath(cfg))
	}
	return token, nil
}

func printToken(cfg Config) error {
	token, err := readToken(cfg)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s does not exist yet; the service creates it on its first start", tokenPath(cfg))
	}
	if err != nil {
		return err
	}
	fmt.Println(token)
	return nil
}

// requireToken answers 401, with no detail, to a request that lacks the token. The digests have equal length, so the
// comparison takes the same time for any input.
func requireToken(token string, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		have := sha256.Sum256([]byte(got))
		if !ok || subtle.ConstantTimeCompare(have[:], want[:]) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostAddr is the address the commands connect to: the configured listen address, or loopback on its port when that
// is a wildcard.
func hostAddr(cfg Config) string {
	host, port, _ := net.SplitHostPort(cfg.Listen)
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return cfg.Listen
}

// hostRequest sends a request with the token to the running host.
func hostRequest(cfg Config, token, method, path string, body []byte, client *http.Client) (*http.Response, error) {
	req, err := http.NewRequest(method, "http://"+hostAddr(cfg)+path, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return client.Do(req)
}

var errRejected = errors.New("the host rejected the token; if the token file changed, restart the service")
