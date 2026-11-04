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

// The slow check's output cap. A login check asks for the least the model accepts (Kimi rejects fewer than 16).
const (
	slowCheckMaxTokens  = 8000
	loginCheckMaxTokens = 16
)

// bedrock calls Amazon Bedrock's Converse API with the Bedrock API key, a bearer token. Each call is one attempt.
type bedrock struct {
	key    string
	region string
	model  string
	client *http.Client

	mu          sync.Mutex
	st          bedrockStatus
	authFailing bool // the previous call was refused for the key; an outage is logged once
}

type bedrockStatus struct {
	KeySet   bool      `json:"key_set"`
	Region   string    `json:"region"`
	Model    string    `json:"model"`
	LastCall time.Time `json:"last_call,omitzero"`
	Result   string    `json:"result,omitempty"` // "ok", or the failure's reason
}

func newBedrock(key, region, model string) *bedrock {
	return &bedrock{
		key:    key,
		region: region,
		model:  model,
		client: &http.Client{Timeout: 300 * time.Second},
		st:     bedrockStatus{KeySet: key != "", Region: region, Model: model},
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

type converseUsage struct {
	InputTokens           int `json:"input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	CacheReadInputTokens  int `json:"cache_read_input_tokens"`
	CacheWriteInputTokens int `json:"cache_write_input_tokens"`
}

// converseAnswer is a 200 from Bedrock. A refusal or a token cap is still an answer; the slow check judges
// StopReason and Text.
type converseAnswer struct {
	Version    string        `json:"version"` // the bundle whose slow-check system prompt was sent
	Model      string        `json:"model"`
	StopReason string        `json:"stop_reason"`
	Text       string        `json:"text"` // every text block of the output message, joined
	Usage      converseUsage `json:"usage"`
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

// converse sends one user message to the model, with system as the system prompt when it is not empty.
func (b *bedrock) converse(ctx context.Context, system, user string, maxTokens int) (*converseAnswer, error) {
	var body converseRequest
	if system != "" {
		body.System = []textBlock{{Text: system}}
	}
	body.Messages = []converseMessage{{Role: "user", Content: []textBlock{{Text: user}}}}
	body.InferenceConfig.MaxTokens = maxTokens
	raw, _ := json.Marshal(body)
	endpoint := fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com/model/%s/converse", b.region, url.PathEscape(b.model))
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
	return &converseAnswer{
		Model:      b.model,
		StopReason: out.StopReason,
		Text:       text.String(),
		Usage:      converseUsage(out.Usage),
	}, nil
}

// Reasons a slow-check call has no answer.
const (
	reasonNoBundle    = "no_bundle"           // no bundle is served, so there is no system prompt
	reasonNoKey       = "no_key"              // no Bedrock key is set
	reasonAuth        = "bedrock_auth"        // Bedrock answered 401 or 403: an invalid or expired key, or no access to the model
	reasonError       = "bedrock_error"       // any other answer than 200: throttling, 5xx, validation
	reasonUnreachable = "bedrock_unreachable" // a network error or the 300 s timeout
)

// converseError is a slow-check call without an answer; it wraps the *bedrockError when Bedrock answered.
type converseError struct {
	Reason string
	err    error
}

func (e *converseError) Error() string { return e.err.Error() }
func (e *converseError) Unwrap() error { return e.err }

// slowCheck makes the slow check's Bedrock call: the served bundle's slow_check.system as the system prompt and user
// as the one user message, capped at 8000 output tokens. It runs alongside scoring and other calls; it holds no
// llama-server slot. A failure is a *converseError. One attempt: the caller retries.
func (s *server) slowCheck(ctx context.Context, user string) (*converseAnswer, error) {
	s.mu.Lock()
	m := s.cur
	s.mu.Unlock()
	if m == nil {
		return nil, &converseError{Reason: reasonNoBundle, err: errors.New("no bundle is being served")}
	}
	if s.br.key == "" {
		return nil, &converseError{Reason: reasonNoKey, err: errors.New("no Bedrock key is set; set one with sudo ecc-server bedrock login")}
	}
	start := time.Now()
	res, err := s.br.converse(ctx, m.bundle.SlowCheck.System, user, slowCheckMaxTokens)
	if err == nil {
		res.Version = m.bundle.Version
		s.br.record(nil, len(user), res, time.Since(start))
		return res, nil
	}
	ce := &converseError{Reason: reasonUnreachable, err: fmt.Errorf("Bedrock is unreachable: %w", err)}
	var be *bedrockError
	switch {
	case errors.As(err, &be) && be.auth():
		ce = &converseError{Reason: reasonAuth,
			err: fmt.Errorf("%w; set a valid key with access to %s with sudo ecc-server bedrock login", err, s.br.model)}
	case errors.As(err, &be):
		ce = &converseError{Reason: reasonError, err: err}
	}
	if ctx.Err() == nil { // a caller that gave up is not a Bedrock outage
		s.br.record(ce, len(user), nil, time.Since(start))
	}
	return nil, ce
}

// record keeps a call's outcome for status and logs it: status and sizes only, since the system prompt, the user
// text and the answer hold policy and player chat. A refused key is logged once per outage, with the fix.
func (b *bedrock) record(ce *converseError, userBytes int, res *converseAnswer, took time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.st.LastCall = time.Now()
	took = took.Round(time.Millisecond)
	if ce == nil {
		b.st.Result = "ok"
		if b.authFailing {
			slog.Info("Bedrock accepts the key again")
			b.authFailing = false
		}
		slog.Info("slow check call", "stop_reason", res.StopReason, "user_bytes", userBytes, "text_bytes", len(res.Text),
			"input_tokens", res.Usage.InputTokens, "output_tokens", res.Usage.OutputTokens, "took", took.String())
		return
	}
	b.st.Result = ce.Reason
	var be *bedrockError
	switch {
	case errors.As(ce, &be) && be.Type != "":
		b.st.Result = fmt.Sprintf("%s (%d %s)", ce.Reason, be.Status, be.Type)
	case be != nil:
		b.st.Result = fmt.Sprintf("%s (%d)", ce.Reason, be.Status)
	}
	if ce.Reason == reasonAuth {
		if !b.authFailing {
			slog.Error("Bedrock refused the key for the slow check; set a valid key with access to the model with sudo ecc-server bedrock login",
				"status", be.Status, "type", be.Type, "model", b.model)
			b.authFailing = true
		}
		return
	}
	slog.Warn("slow check call failed", "result", b.st.Result, "user_bytes", userBytes, "took", took.String())
}
