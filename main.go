// ECC-Server hosts Module-ECC's chat-safety classifier: it serves a classifier bundle through a llama-server child
// process and scores chat messages for Module-ECC over loopback HTTP.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/BurntSushi/toml"
)

// version is set at build time from the VERSION file (-ldflags -X main.version=...).
var version = "dev"

const defaultConfig = "/etc/ecc-server/config.toml"

type Config struct {
	Listen  string `toml:"listen"`
	Data    string `toml:"data"`
	Threads int    `toml:"threads"`
	// A local llama-server binary to run instead of the bundle's llama.cpp release (development, or a host
	// without a release build).
	LlamaServer string `toml:"llama_server"`
}

func loadConfig(path string) (Config, error) {
	cfg := Config{Listen: "127.0.0.1:8790", Data: "/var/lib/ecc-server", Threads: 6}
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return cfg, fmt.Errorf("config %s: %w", path, err)
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		return cfg, fmt.Errorf("config %s: unknown keys %v", path, keys)
	}
	// The interface has no authentication: Module-ECC reaches it on the same machine only.
	host, _, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return cfg, fmt.Errorf("config %s: listen: %w", path, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return cfg, fmt.Errorf("config %s: listen must be a loopback address, not %q", path, cfg.Listen)
	}
	if cfg.Threads < 1 {
		return cfg, fmt.Errorf("config %s: threads must be at least 1", path)
	}
	if cfg.Data == "" {
		return cfg, fmt.Errorf("config %s: data is empty", path)
	}
	return cfg, nil
}

const usage = `ecc-server <command> [-config FILE] [FOLDER]

  run              serve the current bundle on the configured loopback address
  use FOLDER       install a bundle folder (model.gguf, bundle.json, manifest.json) and switch to it
  rollback         switch back to the previous bundle
  status           print the bundle the running host serves and its health
  version          print the ecc-server version

The config file defaults to ` + defaultConfig + `.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	if cmd == "version" && len(os.Args) == 2 {
		fmt.Println(version)
		return
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	configPath := fs.String("config", defaultConfig, "config file")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	_ = fs.Parse(os.Args[2:])
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fail(err)
	}
	if (cmd == "use" || cmd == "rollback") && os.Geteuid() == 0 {
		fail(fmt.Errorf("run %s as the service user, which owns the data folder: sudo -u ecc-server ecc-server %s ...", cmd, cmd))
	}
	switch {
	case cmd == "run" && fs.NArg() == 0:
		err = run(cfg)
	case cmd == "use" && fs.NArg() == 1:
		err = useBundle(cfg, fs.Arg(0))
	case cmd == "rollback" && fs.NArg() == 0:
		err = rollback(cfg)
	case cmd == "status" && fs.NArg() == 0:
		err = status(cfg)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	slog.Error(err.Error())
	os.Exit(1)
}
