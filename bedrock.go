package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const bedrockCredential = "ecc-server.bedrock-api-key"

// A login check asks for the least output the model accepts (Kimi rejects fewer than 16).
const loginCheckMaxTokens = 16

// bedrock calls Amazon Bedrock's Converse API with the Bedrock API key, a bearer token. Each call is one attempt.
type bedrock struct {
	key      string
	region   string
	model    string
	endpoint string // https://bedrock-runtime.<region>.amazonaws.com, or the rig's fake
	client   *http.Client

	mu          sync.Mutex
	st          bedrockStatus
	authFailing bool // the previous call was refused for the key; an outage is logged once
}

type bedrockStatus struct {
	KeySet   bool      `json:"key_set"`
	Region   string    `json:"region"`
	Model    string    `json:"model"`
	Tier     string    `json:"service_tier"`
	LastCall time.Time `json:"last_call,omitzero"`
	Result   string    `json:"result,omitempty"` // "ok", or the failure
}

func newBedrock(key string, cfg Config) *bedrock {
	endpoint := cfg.Test.BedrockEndpoint
	if endpoint == "" {
		endpoint = "https://bedrock-runtime." + cfg.Judge.Region + ".amazonaws.com"
	}
	return &bedrock{
		key:      key,
		region:   cfg.Judge.Region,
		model:    cfg.Judge.Model,
		endpoint: strings.TrimSuffix(endpoint, "/"),
		client:   &http.Client{},
		st:       bedrockStatus{KeySet: key != "", Region: cfg.Judge.Region, Model: cfg.Judge.Model, Tier: cfg.Judge.ServiceTier},
	}
}

func (b *bedrock) status() *bedrockStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.st
	return &st
}

type textBlock struct {
	Text string `json:"text"`
}

type converseMessage struct {
	Role    string      `json:"role"`
	Content []textBlock `json:"content"`
}

type converseRequest struct {
	System          []textBlock       `json:"system,omitempty"`
	Messages        []converseMessage `json:"messages"`
	InferenceConfig struct {
		MaxTokens int `json:"maxTokens"`
	} `json:"inferenceConfig"`
	ServiceTier *struct {
		Type string `json:"type"`
	} `json:"serviceTier,omitempty"`
}

type converseResponse struct {
	Output struct {
		Message struct {
			Content []struct {
				Text string `json:"text"` // empty for a block that is not text, such as reasoning
			} `json:"content"`
		} `json:"message"`
	} `json:"output"`
	StopReason string `json:"stopReason"`
	Usage      struct {
		InputTokens           int `json:"inputTokens"`
		OutputTokens          int `json:"outputTokens"`
		CacheReadInputTokens  int `json:"cacheReadInputTokens"`
		CacheWriteInputTokens int `json:"cacheWriteInputTokens"`
	} `json:"usage"`
}

// converseAnswer is a 200 from Bedrock. A refusal or a token cap is still an answer; the judge checks Text.
type converseAnswer struct {
	StopReason string
	Text       string // every text block of the output message, joined
	Usage      tokenUsage
}

// bedrockError is an answer from Bedrock other than 200, or a 200 whose body is not a Converse response.
type bedrockError struct {
	Status  int
	Type    string // the x-amzn-ErrorType, such as ThrottlingException
	Message string
}

func (e *bedrockError) Error() string {
	if e.Type != "" {
		return fmt.Sprintf("Bedrock answered %d %s: %s", e.Status, e.Type, e.Message)
	}
	return fmt.Sprintf("Bedrock answered %d: %s", e.Status, e.Message)
}

// auth reports a key Bedrock does not accept, or one without access to the model.
func (e *bedrockError) auth() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}

// converse sends one user message to the model, with system as the system prompt when it is not empty, on the
// given service tier ("" for the account's default).
func (b *bedrock) converse(ctx context.Context, system, user string, maxTokens int, tier string) (*converseAnswer, error) {
	var body converseRequest
	if system != "" {
		body.System = []textBlock{{Text: system}}
	}
	body.Messages = []converseMessage{{Role: "user", Content: []textBlock{{Text: user}}}}
	body.InferenceConfig.MaxTokens = maxTokens
	if tier != "" {
		body.ServiceTier = &struct {
			Type string `json:"type"`
		}{tier}
	}
	raw, _ := json.Marshal(body)
	endpoint := b.endpoint + "/model/" + url.PathEscape(b.model) + "/converse"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+b.key)
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		e := &bedrockError{Status: resp.StatusCode, Type: resp.Header.Get("X-Amzn-Errortype")}
		e.Type, _, _ = strings.Cut(e.Type, ":")
		var msg struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &msg) == nil && msg.Message != "" {
			e.Message = msg.Message
		} else {
			e.Message = truncate(strings.TrimSpace(string(data)), 200)
		}
		return nil, e
	}
	var out converseResponse
	if err := json.Unmarshal(data, &out); err != nil || out.StopReason == "" {
		return nil, &bedrockError{Status: resp.StatusCode, Message: "the answer is not a Converse response"}
	}
	var text strings.Builder
	for _, c := range out.Output.Message.Content {
		text.WriteString(c.Text)
	}
	u := out.Usage
	return &converseAnswer{StopReason: out.StopReason, Text: text.String(),
		Usage: tokenUsage{Input: u.InputTokens, Output: u.OutputTokens, CacheRead: u.CacheReadInputTokens,
			CacheWrite: u.CacheWriteInputTokens}}, nil
}

// record keeps a judge call's outcome for status and logs it: sizes, tokens and cost only, since the prompt and the
// answer hold the owner's cases and player chat. A refused key is logged once per outage, with the fix.
func (b *bedrock) record(err error, promptBytes int, res *converseAnswer, cost float64, took time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.st.LastCall = time.Now()
	took = took.Round(time.Millisecond)
	usd := fmt.Sprintf("%.4f", cost)
	if err == nil {
		b.st.Result = "ok"
		if b.authFailing {
			slog.Info("Bedrock accepts the key again")
			b.authFailing = false
		}
		slog.Info("judge call", "stop_reason", res.StopReason, "prompt_bytes", promptBytes, "text_bytes", len(res.Text),
			"input_tokens", res.Usage.Input, "cache_read_tokens", res.Usage.CacheRead,
			"cache_write_tokens", res.Usage.CacheWrite, "output_tokens", res.Usage.Output, "usd", usd,
			"took", took.String())
		return
	}
	b.st.Result = err.Error()
	var be *bedrockError
	if errors.As(err, &be) && be.auth() {
		if !b.authFailing {
			slog.Error("Bedrock refused the key for the judge; set a valid key with access to the model with sudo ecc-server bedrock login",
				"status", be.Status, "type", be.Type, "model", b.model)
			b.authFailing = true
		}
		return
	}
	slog.Warn("judge call failed", "err", err.Error(), "prompt_bytes", promptBytes, "usd", usd, "took", took.String())
}
