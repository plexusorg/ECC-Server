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
	"strings"
	"sync"
	"time"
)

const bedrockCredential = "ecc-server.bedrock-api-key"

// A login check asks for the least output and no reasoning.
const loginCheckMaxTokens = 16

// bedrock calls the OpenAI-compatible Responses API of Amazon Bedrock's runtime endpoint
// (POST <endpoint>/openai/v1/responses) with the Bedrock API key, a bearer token. Each call is one attempt.
type bedrock struct {
	key      string
	endpoint string // https://bedrock-runtime.<region>.amazonaws.com
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
	Effort   string    `json:"reasoning_effort"`
	LastCall time.Time `json:"last_call,omitzero"`
	Result   string    `json:"result,omitempty"` // "ok", or the failure
	// LastUsage is the last answered call's usage; Totals sums every answered call since the host started.
	LastUsage *tokenUsage `json:"last_usage,omitempty"`
	Totals    callTotals  `json:"since_start"`
}

type callTotals struct {
	Calls    int `json:"calls"`
	Answered int `json:"answered"`
	tokenUsage
}

func newBedrock(key string) *bedrock {
	endpoint := "https://bedrock-runtime." + judgeRegion + ".amazonaws.com"
	return &bedrock{
		key:      key,
		endpoint: strings.TrimSuffix(endpoint, "/"),
		client:   &http.Client{},
		st: bedrockStatus{KeySet: key != "", Region: judgeRegion, Model: judgeModel, Tier: judgeServiceTier,
			Effort: judgeReasoningEffort},
	}
}

func (b *bedrock) status() *bedrockStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.st
	if st.LastUsage != nil {
		u := *st.LastUsage
		st.LastUsage = &u
	}
	return &st
}

// The Responses request: one user message whose input_text parts, joined, are the prompt. The first part ends at an
// explicit cache breakpoint; with the explicit cache mode only that breakpoint is cached.
type responsesRequest struct {
	Model           string           `json:"model"`
	Input           []responsesInput `json:"input"`
	MaxOutputTokens int              `json:"max_output_tokens"`
	Reasoning       *struct {
		Effort string `json:"effort"`
	} `json:"reasoning,omitempty"`
	ServiceTier        string     `json:"service_tier,omitempty"`
	PromptCacheOptions *cacheMode `json:"prompt_cache_options,omitempty"`
	Store              bool       `json:"store"`
}

type responsesInput struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content []responsesPart `json:"content"`
}

type responsesPart struct {
	Type       string     `json:"type"`
	Text       string     `json:"text"`
	Breakpoint *cacheMode `json:"prompt_cache_breakpoint,omitempty"`
}

type cacheMode struct {
	Mode string `json:"mode"`
}

type responsesResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"content"`
	} `json:"output"`
	Usage *struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens     int  `json:"cached_tokens"`
			CacheWriteTokens *int `json:"cache_write_tokens"`
		} `json:"input_tokens_details"`
		OutputTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
	ServiceTier string `json:"service_tier"`
}

// judgeAnswer is a 200 from Bedrock. An incomplete answer is still an answer; the judge checks it.
type judgeAnswer struct {
	RequestID  string
	ResponseID string
	Status     string // "completed", "incomplete", ...
	Incomplete string // incomplete_details.reason
	Text       string // every output_text part of the output messages, joined
	Usage      tokenUsage
	Tier       string // the service tier that served it
}

// bedrockError is an answer from Bedrock other than 200, or a 200 whose body is not a Responses answer or is one whose
// status is failed or cancelled (Status 200 for both: the call may succeed when made again).
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

// passing reports a failure that a later attempt may not meet: throttling or a server error.
func (e *bedrockError) passing() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// respond sends the prompt as one user message of two input_text parts, split at the byte offset split (0: one part,
// not cached): the first part carries an explicit cache breakpoint. effort "" leaves the model's default reasoning,
// tier "" the account's default tier.
func (b *bedrock) respond(ctx context.Context, prompt string, split, maxTokens int, effort, tier string) (*judgeAnswer, error) {
	body := responsesRequest{Model: judgeModel, MaxOutputTokens: maxTokens, ServiceTier: tier}
	msg := responsesInput{Type: "message", Role: "user"}
	if split > 0 && split < len(prompt) {
		body.PromptCacheOptions = &cacheMode{"explicit"}
		msg.Content = []responsesPart{{Type: "input_text", Text: prompt[:split], Breakpoint: &cacheMode{"explicit"}},
			{Type: "input_text", Text: prompt[split:]}}
	} else {
		msg.Content = []responsesPart{{Type: "input_text", Text: prompt}}
	}
	body.Input = []responsesInput{msg}
	if effort != "" {
		body.Reasoning = &struct {
			Effort string `json:"effort"`
		}{effort}
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint+"/openai/v1/responses", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	requestID := newUUID()
	req.Header.Set("X-Client-Request-Id", requestID)
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
			Error   struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		switch {
		case json.Unmarshal(data, &msg) == nil && msg.Message != "":
			e.Message = msg.Message
		case msg.Error.Message != "":
			e.Message = msg.Error.Message
			if e.Type == "" {
				e.Type = msg.Error.Type
			}
		default:
			e.Message = truncate(strings.TrimSpace(string(data)), 200)
		}
		return nil, e
	}
	var out responsesResponse
	if err := json.Unmarshal(data, &out); err != nil || out.Status == "" {
		return nil, &bedrockError{Status: resp.StatusCode, Message: "the answer is not a Responses answer"}
	}
	if out.Status == "failed" || out.Status == "cancelled" {
		// Bedrock could not make the answer: made again after a backoff, as a server error is. The usage, when stated,
		// is what the call cost.
		e := &bedrockError{Status: resp.StatusCode, Message: "the response's status is " + out.Status}
		if out.Error != nil {
			e.Type, e.Message = out.Error.Code, e.Message+": "+truncate(out.Error.Message, 200)
		}
		var a *judgeAnswer
		if u := out.Usage; u != nil {
			a = &judgeAnswer{Status: out.Status, Tier: out.ServiceTier, Usage: tokenUsage{Input: u.InputTokens,
				Output: u.OutputTokens, Cached: u.InputTokensDetails.CachedTokens,
				CacheWrite: u.InputTokensDetails.CacheWriteTokens, Reasoning: u.OutputTokensDetails.ReasoningTokens}}
		}
		return a, e
	}
	if out.Usage == nil {
		return nil, &bedrockError{Status: resp.StatusCode, Message: "the answer is not a Responses answer"}
	}
	a := &judgeAnswer{RequestID: requestID, ResponseID: out.ID, Status: out.Status, Tier: out.ServiceTier}
	if out.IncompleteDetails != nil {
		a.Incomplete = out.IncompleteDetails.Reason
	}
	var text strings.Builder
	for _, o := range out.Output {
		if o.Type != "message" {
			continue // reasoning
		}
		for _, c := range o.Content {
			if c.Type == "output_text" {
				text.WriteString(c.Text)
			}
		}
	}
	a.Text = text.String()
	u := out.Usage
	a.Usage = tokenUsage{Input: u.InputTokens, Output: u.OutputTokens, Cached: u.InputTokensDetails.CachedTokens,
		CacheWrite: u.InputTokensDetails.CacheWriteTokens, Reasoning: u.OutputTokensDetails.ReasoningTokens}
	return a, nil
}

// record keeps a judge call's outcome for status and logs it: sizes, tokens and cost only, since the prompt and the
// answer hold the owner's cases and player chat. A refused key is logged once per outage, with the fix.
func (b *bedrock) record(err error, promptBytes int, res *judgeAnswer, cost float64, took time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.st.LastCall = time.Now()
	b.st.Totals.Calls++
	took = took.Round(time.Millisecond)
	usd := fmt.Sprintf("%.4f", cost)
	if err == nil {
		b.st.Result = "ok"
		u := res.Usage
		b.st.LastUsage = &u
		t := &b.st.Totals
		t.Answered++
		t.Input += u.Input
		t.Output += u.Output
		t.Cached += u.Cached
		t.Reasoning += u.Reasoning
		if u.CacheWrite != nil {
			n := *u.CacheWrite
			if t.CacheWrite != nil {
				n += *t.CacheWrite
			}
			t.CacheWrite = &n
		}
		if b.authFailing {
			slog.Info("Bedrock accepts the key again")
			b.authFailing = false
		}
		slog.Info("judge call", "status", res.Status, "incomplete", res.Incomplete, "prompt_bytes", promptBytes,
			"text_bytes", len(res.Text), "input_tokens", u.Input, "cached_tokens", u.Cached,
			"cache_write_tokens", u.writeText(), "output_tokens", u.Output, "reasoning_tokens", u.Reasoning,
			"service_tier", res.Tier, "usd", usd, "took", took.String())
		return
	}
	b.st.Result = err.Error()
	var be *bedrockError
	if errors.As(err, &be) && be.auth() {
		if !b.authFailing {
			slog.Error("Bedrock refused the key for the judge; set a valid key with access to the model with sudo ecc-server bedrock login",
				"status", be.Status, "type", be.Type, "model", judgeModel)
			b.authFailing = true
		}
		return
	}
	slog.Warn("judge call failed", "err", err.Error(), "prompt_bytes", promptBytes, "usd", usd, "took", took.String())
}
