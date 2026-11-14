// ECC-Server makes Module-ECC's chat-safety decisions: it gives every chat line a verdict before delivery, keeps
// each player's sitting with its held lines, asks a judge on Amazon Bedrock for a ruling at the sitting's checkpoints
// within a spend budget, keeps the record, and tells Module-ECC whom to ban or warn and which held lines to deliver,
// over HTTP with a shared token.
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
	Listen string `toml:"listen"`
	Data   string `toml:"data"`
	// This game server's name, written in every record line.
	Server string `toml:"server"`
	// How many days the daily records are kept.
	RecordRetentionDays int `toml:"record_retention_days"`
	// The W&B artifact whose `production` alias names the bundle to serve, and how often to check it.
	WandbEntity   string        `toml:"wandb_entity"`
	WandbProject  string        `toml:"wandb_project"`
	WandbArtifact string        `toml:"wandb_artifact"`
	CheckInterval time.Duration `toml:"check_interval"`

	Model  ModelConfig  `toml:"model"`
	Judge  JudgeConfig  `toml:"judge"`
	Budget BudgetConfig `toml:"budget"`
	Chat   ChatConfig   `toml:"chat"`
	Test   TestConfig   `toml:"test"`
}

// ModelConfig is how the bundle's models run on this host.
type ModelConfig struct {
	// Threads is each model's CPU threads.
	Threads int `toml:"threads"`
	// CallTimeout cuts off a model call that takes longer; its line is held.
	CallTimeout time.Duration `toml:"call_timeout"`
	// MaxWait: a line still waiting for its verdict this long after it arrived is held, unscored if its models have
	// not run.
	MaxWait time.Duration `toml:"max_wait"`
	// Threshold names the bundle's hold threshold to use; empty is the bundle's default.
	Threshold string `toml:"threshold"`
}

// JudgeConfig is the Bedrock model the judge calls.
type JudgeConfig struct {
	Region string `toml:"region"`
	Model  string `toml:"model"`
	// ServiceTier is Bedrock's service tier: flex, default or priority.
	ServiceTier     string        `toml:"service_tier"`
	MaxOutputTokens int           `toml:"max_output_tokens"`
	Timeout         time.Duration `toml:"timeout"`
}

// BudgetConfig caps what the judge's calls cost.
type BudgetConfig struct {
	DailyUSD   float64 `toml:"daily_usd"`
	MonthlyUSD float64 `toml:"monthly_usd"`
	// Standard-tier prices in dollars per million tokens; the server halves them on the flex tier.
	InputPerMillion      float64 `toml:"input_per_million"`
	OutputPerMillion     float64 `toml:"output_per_million"`
	CacheReadPerMillion  float64 `toml:"cache_read_per_million"`
	CacheWritePerMillion float64 `toml:"cache_write_per_million"`
	// A prompt longer than this many characters is not sent; no more than MaxCallsPerDay calls are made in a UTC day.
	MaxPromptChars int `toml:"max_prompt_chars"`
	MaxCallsPerDay int `toml:"max_calls_per_day"`
}

// ChatConfig is the server's part of the decision settings; the bundle carries the rest, and the plugin sends which
// players are trusted and the allowlist of links.
type ChatConfig struct {
	// Set, these replace the bundle's checkpoint schedule, its every-n-th held line and its waiting time.
	Schedule []int         `toml:"schedule"`
	Every    int           `toml:"every"`
	Wait     time.Duration `toml:"wait"`

	setSchedule, setEvery, setWait bool
}

// TestConfig is for the end-to-end rig only.
type TestConfig struct {
	// HoldWordsFile names a file of words, one a line: the rig's scorer holds a line that contains one of them.
	HoldWordsFile string `toml:"hold_words_file"`
	// BedrockEndpoint replaces the Bedrock runtime endpoint (a local fake of the Converse API).
	BedrockEndpoint string `toml:"bedrock_endpoint"`
}

var serverName = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// The region becomes part of the Bedrock endpoint's host name.
var regionName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func loadConfig(path string) (Config, error) {
	cfg := Config{Listen: "127.0.0.1:8790", Data: "/var/lib/ecc-server", RecordRetentionDays: 180,
		WandbEntity: "telesphoreo-telesphoreo", WandbProject: "ecc", WandbArtifact: "classifier-bundle",
		CheckInterval: 10 * time.Minute,
		Model:         ModelConfig{Threads: 6, CallTimeout: 800 * time.Millisecond, MaxWait: 900 * time.Millisecond},
		Judge: JudgeConfig{Region: "us-east-1", Model: "us.moonshotai.kimi-k3.1", ServiceTier: "flex",
			MaxOutputTokens: 8000, Timeout: 10 * time.Minute},
		Budget: BudgetConfig{DailyUSD: 10, MonthlyUSD: 150, InputPerMillion: 4.40, OutputPerMillion: 22.00,
			CacheReadPerMillion: 0.22, CacheWritePerMillion: 5.50, MaxPromptChars: 500000, MaxCallsPerDay: 2000}}
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return cfg, fmt.Errorf("config %s: %w", path, err)
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		return cfg, fmt.Errorf("config %s: unknown keys %v", path, keys)
	}
	cfg.Chat.setSchedule = md.IsDefined("chat", "schedule")
	cfg.Chat.setEvery = md.IsDefined("chat", "every")
	cfg.Chat.setWait = md.IsDefined("chat", "wait")
	bad := func(format string, args ...any) (Config, error) {
		return cfg, fmt.Errorf("config %s: "+format, append([]any{path}, args...)...)
	}
	if _, _, err := net.SplitHostPort(cfg.Listen); err != nil {
		return bad("listen: %w", err)
	}
	if cfg.Data == "" {
		return bad("data is empty")
	}
	if cfg.WandbEntity == "" || cfg.WandbProject == "" || cfg.WandbArtifact == "" {
		return bad("wandb_entity, wandb_project and wandb_artifact must not be empty")
	}
	if cfg.CheckInterval < time.Minute {
		return bad("check_interval must be at least 1m")
	}
	if !serverName.MatchString(cfg.Server) {
		return bad("server %q must be 1 to 32 lowercase letters, digits, - or _", cfg.Server)
	}
	if cfg.RecordRetentionDays < 3 {
		return bad("record_retention_days must be at least 3")
	}
	if m := cfg.Model; m.Threads < 1 || m.CallTimeout < 10*time.Millisecond || m.MaxWait < 10*time.Millisecond {
		return bad("model.threads must be at least 1, model.call_timeout and model.max_wait at least 10ms")
	}
	j := cfg.Judge
	switch {
	case !regionName.MatchString(j.Region):
		return bad("judge.region %q is not an AWS region name", j.Region)
	case j.Model == "":
		return bad("judge.model is empty")
	case j.ServiceTier != "flex" && j.ServiceTier != "default" && j.ServiceTier != "priority":
		return bad("judge.service_tier must be flex, default or priority")
	case j.MaxOutputTokens < 16:
		return bad("judge.max_output_tokens must be at least 16")
	case j.Timeout < 10*time.Second:
		return bad("judge.timeout must be at least 10s")
	}
	b := cfg.Budget
	for _, p := range []float64{b.DailyUSD, b.MonthlyUSD, b.InputPerMillion, b.OutputPerMillion, b.CacheReadPerMillion,
		b.CacheWritePerMillion} {
		if p < 0 {
			return bad("budget amounts and prices must not be negative")
		}
	}
	if b.MaxPromptChars < 1 || b.MaxCallsPerDay < 1 {
		return bad("budget.max_prompt_chars and budget.max_calls_per_day must be at least 1")
	}
	c := cfg.Chat
	for _, n := range c.Schedule {
		if n < 1 {
			return bad("chat.schedule counts must be at least 1")
		}
	}
	if c.Every < 0 || c.Wait < 0 {
		return bad("chat.every and chat.wait must not be negative")
	}

	return cfg, nil
}

const usage = `ecc-server <command> [-config FILE] [FOLDER]

  run              serve the current bundle on the configured address
  use FOLDER       install a bundle folder (bundle.json, manifest.json and the files they name) and switch to it
  rollback         switch back to the previous bundle
  update           have the running host check W&B for a new production bundle now (run with sudo)
  status           (run with sudo) print the running host's bundle, health, W&B updates, judge calls, spend and sittings
  token            print the token Module-ECC needs (run with sudo)
  wandb login      store the W&B API key, encrypted for the service (run with sudo)
  wandb logout     remove the stored W&B API key (run with sudo)
  bedrock login    check the Amazon Bedrock API key and store it, encrypted for the service (run with sudo)
  bedrock logout   remove the stored Bedrock API key (run with sudo)
  replay ...       feed recorded lines through the decision core (ecc-server replay -h)
  score ...        score a file of lines with a bundle's models, as the reference scorer does (ecc-server score -h)
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
