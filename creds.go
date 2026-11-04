package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// API keys reach the service as systemd credentials: `ecc-server wandb login` and `ecc-server bedrock login` encrypt
// them into the credential store, and the unit's LoadCredentialEncrypted= lines have systemd decrypt them into the
// service's credential directory at each start.
const credStore = "/etc/credstore.encrypted"

func credFile(name string) string { return credStore + "/" + name }

// credential reads a key from the service's credentials; "" when it is not set.
func credential(name string) (string, error) {
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if dir == "" {
		return "", nil
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(data)), err
}

// readKey reads an API key for `<cmd> login` from a hidden prompt, or from standard input when piped.
func readKey(cmd, prompt string) (string, error) {
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("%s login stores the key with systemd-creds, on Linux", cmd)
	}
	if os.Geteuid() != 0 {
		return "", fmt.Errorf("run %s login as root: sudo ecc-server %s login", cmd, cmd)
	}
	key, err := readSecret(prompt)
	if err != nil {
		return "", err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("no key was entered")
	}
	return key, nil
}

// storeCredential encrypts a key with systemd-creds (the host key, and the TPM2 when there is one) into the
// credential store, where only the service manager can decrypt it. The key never touches the disk in plaintext.
func storeCredential(name, key string) error {
	// No PCR policy: bound to PCR 7 (systemd-creds' default), the credential would stop decrypting after a firmware
	// or Secure Boot change, and systemd would then refuse to start the service.
	cmd := exec.Command("systemd-creds", "encrypt", "--name="+name, "--tpm2-pcrs=", "-", "-")
	cmd.Stdin = strings.NewReader(key)
	cmd.Stderr = os.Stderr
	encrypted, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("systemd-creds encrypt: %w", err)
	}
	if err := os.MkdirAll(credStore, 0o700); err != nil {
		return err
	}
	path := credFile(name)
	if err := os.WriteFile(path+".tmp", encrypted, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// removeCredential deletes a stored key and restarts the service so it stops using it.
func removeCredential(cmd, name, what string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("run %s logout as root: sudo ecc-server %s logout", cmd, cmd)
	}
	path := credFile(name)
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Printf("no %s key is stored\n", what)
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Printf("removed %s\n", path)
	return restartService()
}

// restartService has a running service pick up a stored key, or its removal.
func restartService() error {
	cmd := exec.Command("systemctl", "try-restart", "ecc-server.service")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl try-restart ecc-server.service: %w", err)
	}
	fmt.Println("restarted ecc-server.service if it was running; a stopped service reads the key when it starts")
	return nil
}
