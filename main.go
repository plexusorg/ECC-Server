// ECC-Server makes Module-ECC's chat-safety decisions: it scores chat messages with a classifier bundle served by a
// llama-server child process, runs the slow check on Amazon Bedrock, keeps the record, and tells Module-ECC whom to
// mute, unmute and ban, over HTTP with a shared token.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"time"

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
	// The W&B artifact whose `production` alias names the bundle to serve, and how often to check it.
	WandbEntity   string        `toml:"wandb_entity"`
	WandbProject  string        `toml:"wandb_project"`
	WandbArtifact string        `toml:"wandb_artifact"`
	CheckInterval time.Duration `toml:"check_interval"`
	// The Bedrock region and model (a model or inference profile ID) the slow check calls.
	BedrockRegion string `toml:"bedrock_region"`
	BedrockModel  string `toml:"bedrock_model"`
	// This game server's name, written in every record line.
	Server string `toml:"server"`
	// How many of a player's messages can wait to be scored; past this, their oldest waiting message is not scored.
	QueueSize int `toml:"queue_size"`
	// A banned player is kicked at a random time this long after the verdict.
	KickDelayMin time.Duration `toml:"kick_delay_min"`
	KickDelayMax time.Duration `toml:"kick_delay_max"`
	// How long "not sure yet" verdicts may hold a player muted, not counting time while Bedrock fails.
	HeldCap time.Duration `toml:"held_cap"`
	// How many days the daily records are kept.
	RecordRetentionDays int `toml:"record_retention_days"`
}

var serverName = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// The region becomes part of the Bedrock endpoint's host name.
var regionName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func loadConfig(path string) (Config, error) {
	cfg := Config{Listen: "127.0.0.1:8790", Data: "/var/lib/ecc-server", Threads: 6, WandbEntity: "telesphoreo-telesphoreo",
		WandbProject: "ecc", WandbArtifact: "classifier-bundle", CheckInterval: 10 * time.Minute,
		BedrockRegion: "us-east-1", BedrockModel: "us.moonshotai.kimi-k3", QueueSize: 20, KickDelayMin: 2 * time.Minute,
		KickDelayMax: 10 * time.Minute, HeldCap: 10 * time.Minute, RecordRetentionDays: 180}
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return cfg, fmt.Errorf("config %s: %w", path, err)
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		return cfg, fmt.Errorf("config %s: unknown keys %v", path, keys)
	}
	if _, _, err := net.SplitHostPort(cfg.Listen); err != nil {
		return cfg, fmt.Errorf("config %s: listen: %w", path, err)
	}
	if cfg.Threads < 1 {
		return cfg, fmt.Errorf("config %s: threads must be at least 1", path)
	}
	if cfg.Data == "" {
		return cfg, fmt.Errorf("config %s: data is empty", path)
	}
	if cfg.WandbEntity == "" || cfg.WandbProject == "" || cfg.WandbArtifact == "" {
		return cfg, fmt.Errorf("config %s: wandb_entity, wandb_project and wandb_artifact must not be empty", path)
	}
	if cfg.CheckInterval < time.Minute {
		return cfg, fmt.Errorf("config %s: check_interval must be at least 1m", path)
	}
	if !regionName.MatchString(cfg.BedrockRegion) {
		return cfg, fmt.Errorf("config %s: bedrock_region %q is not an AWS region name", path, cfg.BedrockRegion)
	}
	if cfg.BedrockModel == "" {
		return cfg, fmt.Errorf("config %s: bedrock_model is empty", path)
	}
	if !serverName.MatchString(cfg.Server) {
		return cfg, fmt.Errorf("config %s: server %q must be 1 to 32 lowercase letters, digits, - or _", path, cfg.Server)
	}
	if cfg.QueueSize < 1 || cfg.RecordRetentionDays < 1 {
		return cfg, fmt.Errorf("config %s: queue_size and record_retention_days must be at least 1", path)
	}
	if cfg.KickDelayMin < time.Second || cfg.KickDelayMax < cfg.KickDelayMin {
		return cfg, fmt.Errorf("config %s: kick_delay_min must be at least 1s and kick_delay_max at least kick_delay_min", path)
	}
	if cfg.HeldCap < time.Minute {
		return cfg, fmt.Errorf("config %s: held_cap must be at least 1m", path)
	}
	return cfg, nil
}

const usage = `ecc-server <command> [-config FILE] [FOLDER]

  run              serve the current bundle on the configured address
  use FOLDER       install a bundle folder (model.gguf, bundle.json, manifest.json) and switch to it
  rollback         switch back to the previous bundle
  update           have the running host check W&B for a new production bundle now (run with sudo)
  status           (run with sudo) print the running host's bundle, health, W&B updates, Bedrock calls, backlog, pending players and actions
  token            print the token Module-ECC needs (run with sudo)
  wandb login      store the W&B API key, encrypted for the service (run with sudo)
  wandb logout     remove the stored W&B API key (run with sudo)
  bedrock login    check the Amazon Bedrock API key and store it, encrypted for the service (run with sudo)
  bedrock logout   remove the stored Bedrock API key (run with sudo)
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
	case cmd == "update" && fs.NArg() == 0:
		err = update(cfg)
	case cmd == "token" && fs.NArg() == 0:
		err = printToken(cfg)
	case cmd == "wandb" && fs.NArg() == 1 && fs.Arg(0) == "login":
		err = wandbLogin()
	case cmd == "wandb" && fs.NArg() == 1 && fs.Arg(0) == "logout":
		err = removeCredential("wandb", wandbCredential, "W&B")
	case cmd == "bedrock" && fs.NArg() == 1 && fs.Arg(0) == "login":
		err = bedrockLogin(cfg)
	case cmd == "bedrock" && fs.NArg() == 1 && fs.Arg(0) == "logout":
		err = removeCredential("bedrock", bedrockCredential, "Bedrock")
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
