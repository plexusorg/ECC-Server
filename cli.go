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
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
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

const unitPath = "/etc/systemd/system/ecc-server.service"

const unitTemplate = `[Unit]
Description=ECC-Server, the chat-safety classifier host for Module-ECC
After=network-online.target
Wants=network-online.target

[Service]
User=ecc-server
Group=ecc-server
ExecStart=%s run -config %s
Restart=always
RestartSec=5
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=%s

[Install]
WantedBy=multi-user.target
`

// install writes the systemd unit, creating the ecc-server system user and the data folder it owns.
func install(configPath string, cfg Config) error {
	if runtime.GOOS != "linux" {
		return errors.New("install writes a systemd unit and runs on Linux only")
	}
	if os.Geteuid() != 0 {
		return errors.New("install must run as root")
	}
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	if configPath, err = filepath.Abs(configPath); err != nil {
		return err
	}
	data, err := filepath.Abs(cfg.Data)
	if err != nil {
		return err
	}
	if _, err := user.Lookup("ecc-server"); err != nil {
		cmd := exec.Command("useradd", "--system", "--home-dir", data, "--no-create-home", "--shell", "/usr/sbin/nologin", "ecc-server")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("useradd: %v: %s", err, out)
		}
	}
	if err := os.MkdirAll(data, 0o750); err != nil {
		return err
	}
	if out, err := exec.Command("chown", "ecc-server:ecc-server", data).CombinedOutput(); err != nil {
		return fmt.Errorf("chown %s: %v: %s", data, err, out)
	}
	unit := fmt.Sprintf(unitTemplate, bin, configPath, data)
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %v: %s", err, out)
	}
	fmt.Printf("wrote %s\nnext: sudo -u ecc-server %s use -config %s <bundle folder>\n      systemctl enable --now ecc-server\n", unitPath, bin, configPath)
	return nil
}
