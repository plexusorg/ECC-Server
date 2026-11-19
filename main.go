// ECC-Server makes Module-ECC's chat-safety decisions: it gives every chat line a verdict before delivery, keeps
// each player's session with its held lines, asks a judge on Amazon Bedrock for a ruling at the session's checkpoints
// within a spend budget, keeps the record, and tells Module-ECC whom to ban or warn and which held lines to deliver,
// over HTTP with a shared token.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"

	"github.com/BurntSushi/toml"
)

// version is set at build time from the VERSION file (-ldflags -X main.version=...).
var version = "dev"

const defaultConfig = "/etc/ecc-server/config.toml"

// The data folder: the bundle store, state, actions, the judge's spend, the review queue and the records.
const dataDir = "/var/lib/ecc-server"

type Config struct {
	Listen string       `toml:"listen"`
	Model  ModelConfig  `toml:"model"`
	Budget BudgetConfig `toml:"budget"`

	// Data is the data folder, dataDir.
	Data string `toml:"-"`
}

type ModelConfig struct {
	// Threshold names the bundle's hold threshold to serve; empty is the bundle's default.
	Threshold string `toml:"threshold"`
}

// BudgetConfig is what the judge may spend, in US dollars, per UTC day and per UTC month.
type BudgetConfig struct {
	DailyUSD   float64 `toml:"daily_usd"`
	MonthlyUSD float64 `toml:"monthly_usd"`
}

// loadConfig reads the config file; a missing file gives the defaults.
func loadConfig(path string) (Config, error) {
	cfg := Config{Listen: "127.0.0.1:8790", Budget: BudgetConfig{DailyUSD: 25, MonthlyUSD: 250}, Data: dataDir}
	bad := func(format string, args ...any) (Config, error) {
		return cfg, fmt.Errorf("config %s: "+format, append([]any{path}, args...)...)
	}
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return cfg, fmt.Errorf("config: %w", err)
	}
	if err == nil {
		md, err := toml.Decode(string(raw), &cfg)
		if err != nil {
			return bad("%w", err)
		}
		if keys := md.Undecoded(); len(keys) > 0 {
			return bad("unknown keys %v", keys)
		}
	}
	if _, _, err := net.SplitHostPort(cfg.Listen); err != nil {
		return bad("listen: %w", err)
	}
	if b := cfg.Budget; b.DailyUSD < 0 || b.MonthlyUSD < 0 {
		return bad("budget.daily_usd and budget.monthly_usd must not be negative")
	}
	return cfg, nil
}

const usage = `ecc-server <command> [-config FILE] [FOLDER]

  run              serve the current bundle on the configured address
  use FOLDER       install a bundle folder (bundle.json, manifest.json and the files they name) and switch to it
  rollback         switch back to the previous bundle
  update           have the running host check W&B for a new production bundle now (run with sudo)
  status           (run with sudo) print the running host's bundle, health, W&B updates, judge calls, spend and sessions
  token            print the token Module-ECC needs (run with sudo)
  wandb login      store the W&B API key, encrypted for the service (run with sudo)
  wandb logout     remove the stored W&B API key (run with sudo)
  bedrock login    check the Amazon Bedrock API key and store it, encrypted for the service (run with sudo)
  bedrock logout   remove the stored Bedrock API key (run with sudo)
  replay ...       feed recorded lines through the decision core (ecc-server replay -h)
  score ...        score a file of lines with a bundle's models, as the reference scorer does (ecc-server score -h)
  version          print the ecc-server version

The config file is ` + defaultConfig + ` unless -config names another; without one the defaults apply.
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
	if cmd == "replay" || cmd == "score" {
		run := replay
		if cmd == "score" {
			run = scoreLines
		}
		if err := run(os.Args[2:]); err != nil {
			fail(err)
		}
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
