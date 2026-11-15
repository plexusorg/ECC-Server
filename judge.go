package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"ecc-server/core"
)

// The judge's retries. An invalid answer is asked again, up to judgeAttempts answers. A call that fails (throttling,
// a server error, a response whose status is failed or cancelled, a timeout, the network) is made again after judgeBackoff, doubling each time, while the failures
// have lasted less than judgeRetryFor; then the checkpoint is pending and the core puts it again judgeRetryAfter
// later, until the sitting ends. A call the budget refuses, or one Bedrock refuses for the key (401, 403) or as a bad
// request, is not made again. The rig sets shorter times ([test]).
const (
	judgeAttempts   = 3
	judgeBackoff    = 5 * time.Second
	judgeRetryFor   = 5 * time.Minute
	judgeRetryAfter = 5 * time.Minute
)

// bedrockJudge rules a checkpoint with a Responses call: the prompt as one user message, the question and the owner's
// cases in a first part that ends at an explicit cache breakpoint, so Bedrock caches the part every prompt shares.
// Each call is charged to the spend budget first.
type bedrockJudge struct {
	ctx     context.Context
	br      *bedrock
	spend   *spend
	tier    string
	effort  string
	maxOut  int
	timeout time.Duration
	// stopped tells staff that judging stopped: kind names the cause (the README's notice kinds), text says it.
	stopped func(kind, text string)
	// record writes a line of the record.
	record func(kind string, v any)
	// backoff and retryFor: judgeBackoff and judgeRetryFor, or the rig's.
	backoff, retryFor time.Duration
}

func (j *bedrockJudge) Rule(job *core.Job) (core.Answer, error) {
	invalid := 0
	var failing time.Time
	delay := j.backoff
	for attempt := 1; ; attempt++ {
		res, err := j.call(job, attempt)
		if err != nil {
			if !passing(err) {
				j.stop(err)
				return core.Answer{}, err
			}
			if failing.IsZero() {
				failing = time.Now()
			}
			if time.Since(failing)+delay > j.retryFor {
				return core.Answer{}, fmt.Errorf("%w for %s: %w", core.ErrPending, time.Since(failing).Round(time.Second), err)
			}
			slog.Warn("the judge's call failed; trying again", "sitting", job.Sitting, "k", job.K, "in", delay.String(),
				"err", err.Error())
			select {
			case <-j.ctx.Done():
				return core.Answer{}, j.ctx.Err()
			case <-time.After(delay):
			}
			delay *= 2
			continue
		}
		failing, delay = time.Time{}, j.backoff
		a, err := checkAnswer(res, job.Mine, job.Cases)
		if err == nil {
			return a, nil
		}
		invalid++
		slog.Warn("the judge's answer is invalid", "sitting", job.Sitting, "k", job.K, "answer", invalid, "why", err.Error())
		if invalid == judgeAttempts {
			j.stopped("invalid_answers", fmt.Sprintf("ECC: the judge's answer was invalid %d times on one checkpoint "+
				"(the last: %s). Its held lines stay held and its sitting goes to the review queue.", invalid,
				truncate(err.Error(), 160)))
			return core.Answer{}, fmt.Errorf("the judge's answer was invalid %d times: %w", invalid, err)
		}
	}
}

// errNoKey: no Bedrock key is set.
var errNoKey = errors.New("no Bedrock key is set; set one with sudo ecc-server bedrock login")

const stillHeld = "Held lines stay held and their sittings go to the review queue."

// stop tells staff why a call that is not made again stopped judging: no key, the key or the request refused. The
// budget's notice is sent by call; the host stopping sends none.
func (j *bedrockJudge) stop(err error) {
	var be *bedrockError
	switch {
	case errors.Is(err, errNoKey):
		j.stopped("no_key", "ECC: no Bedrock key is set, so the judge is not called. "+stillHeld+
			" Set one with sudo ecc-server bedrock login.")
	case errors.As(err, &be) && be.auth():
		j.stopped("bedrock_key", fmt.Sprintf("ECC: Bedrock refused the judge's key (%d). %s Set a valid key with "+
			"access to the model with sudo ecc-server bedrock login.", be.Status, stillHeld))
	case errors.As(err, &be):
		j.stopped("bad_request", fmt.Sprintf("ECC: Bedrock refused the judge's request (%s). %s",
			truncate(be.Error(), 200), stillHeld))
	}
}

// passing tells whether a failed call may succeed later: throttling, a server error, a 200 that is not an answer or
// whose status is failed or cancelled, a timeout or a network failure; not a refusal by the budget or for the key, a bad request, or the host stopping.
func passing(err error) bool {
	var be *bedrockError
	switch {
	case errors.Is(err, core.ErrNotSent), errors.Is(err, context.Canceled):
		return false
	case errors.As(err, &be):
		return be.passing() || be.Status == 200
	}
	return true
}

// judgeCall is the record's line for one call: no prompt or answer text.
type judgeCall struct {
	Time        string      `json:"time"`
	Sitting     string      `json:"sitting"`
	K           int         `json:"k"`
	Hash        string      `json:"sha256"`
	Attempt     int         `json:"attempt"`
	PromptBytes int         `json:"prompt_bytes"`
	Status      string      `json:"status,omitempty"`
	Incomplete  string      `json:"incomplete,omitempty"`
	Tier        string      `json:"service_tier,omitempty"`
	Usage       *tokenUsage `json:"usage,omitempty"`
	USD         float64     `json:"usd"`
	Seconds     float64     `json:"seconds"`
	Error       string      `json:"error,omitempty"`
}

// call makes one Responses call within the budget.
func (j *bedrockJudge) call(job *core.Job, attempt int) (*judgeAnswer, error) {
	if j.br.key == "" {
		return nil, fmt.Errorf("%w: %w", core.ErrNotSent, errNoKey)
	}
	r, told, err := j.spend.reserve(j.spend.worst(len(job.Prompt), j.maxOut))
	var over *errOverBudget
	if errors.As(err, &over) {
		if told {
			until := "the next UTC day"
			if over.period == "monthly" {
				until = "the next UTC month"
			}
			slog.Error("the judge's budget is used up; held lines wait for review", "budget", over.period,
				"usd", fmt.Sprintf("%.2f", over.budget))
			j.stopped("budget", fmt.Sprintf("ECC: the judge's %s budget of $%.2f is used up. Until %s, held lines "+
				"stay held and their sittings go to the review queue.", over.period, over.budget, until))
		}
		return nil, fmt.Errorf("%w: %w", core.ErrNotSent, err)
	}
	ctx, cancel := context.WithTimeout(j.ctx, j.timeout)
	defer cancel()
	start := time.Now()
	res, err := j.br.respond(ctx, job.Prompt, job.Split, j.maxOut, j.effort, j.tier)
	cost := r.usd // no answer: the call may have run
	var be *bedrockError
	switch {
	case res != nil: // an answer, or a failed one that states its usage
		cost = j.spend.cost(res.Usage)
	case errors.As(err, &be) && be.Status != 200:
		cost = 0 // Bedrock refused the request
	}
	j.spend.settle(r, cost)
	took := time.Since(start)
	j.br.record(err, len(job.Prompt), res, cost, took)
	rec := judgeCall{Time: stamp(time.Now()), Sitting: job.Sitting, K: job.K, Hash: job.Hash, Attempt: attempt,
		PromptBytes: len(job.Prompt), USD: cost, Seconds: took.Seconds()}
	if res != nil {
		rec.Status, rec.Incomplete, rec.Tier, rec.Usage = res.Status, res.Incomplete, res.Tier, &res.Usage
	}
	if err != nil {
		rec.Error = err.Error()
	}
	j.record("judge_call", rec)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// checkAnswer takes an answer: an incomplete one (its output cut at max_output_tokens, or stopped otherwise) is
// invalid; a completed one's text must pass parseAnswer.
func checkAnswer(res *judgeAnswer, mine []string, cases int) (core.Answer, error) {
	switch res.Status {
	case "completed":
		return parseAnswer(res.Text, mine, cases)
	case "incomplete":
		return core.Answer{}, fmt.Errorf("the answer is incomplete (%s)", res.Incomplete)
	}
	return core.Answer{}, fmt.Errorf("the answer's status is %q", res.Status)
}

// fence is an answer wrapped in a Markdown code fence.
var fence = regexp.MustCompile("(?s)^```(?:json)?\\s*(.*?)\\s*```$")

// parseAnswer checks the judge's answer as the measured runs did: one JSON object, optionally fenced, whose
// "rulings" list holds one entry for the one new case, with a ruling of nothing, hold or ban, for hold or ban "by" one
// of the player's lines the case shows, "like" a list of at most three of the owner's case ids, and "sure" true or
// false. "not ban", a ruling of the owner's cases, is taken as hold; its "by", when given, must be one of the
// player's lines.
func parseAnswer(raw string, mine []string, cases int) (core.Answer, error) {
	raw = strings.TrimSpace(raw)
	if m := fence.FindStringSubmatch(raw); m != nil {
		raw = m[1]
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return core.Answer{}, fmt.Errorf("not JSON: %w", err)
	}
	list, ok := obj["rulings"].([]any)
	if !ok {
		return core.Answer{}, errors.New("rulings missing or not a list")
	}
	if len(list) != 1 {
		return core.Answer{}, fmt.Errorf("%d entries for one case", len(list))
	}
	e, ok := list[0].(map[string]any)
	if !ok {
		return core.Answer{}, errors.New("entry not an object")
	}
	if c, _ := e["case"].(string); c != core.NewCase {
		return core.Answer{}, fmt.Errorf("case not in the call: %v", e["case"])
	}
	var a core.Answer
	a.Ruling, _ = e["ruling"].(string)
	a.By, _ = e["by"].(string)
	switch a.Ruling {
	case core.RulingNothing:
	case core.RulingHold, core.RulingBan:
		if !slices.Contains(mine, a.By) {
			return core.Answer{}, fmt.Errorf("by %v is not one of the player's lines", e["by"])
		}
	case core.RulingNotBan:
		if e["by"] != nil && !slices.Contains(mine, a.By) {
			return core.Answer{}, fmt.Errorf("by %v is not one of the player's lines", e["by"])
		}
		a.Ruling, a.Given = core.RulingHold, core.RulingNotBan
	default:
		return core.Answer{}, fmt.Errorf("bad ruling: %v", e["ruling"])
	}
	like, ok := e["like"].([]any)
	if !ok || len(like) > 3 {
		return core.Answer{}, fmt.Errorf("bad like %v", e["like"])
	}
	for _, x := range like {
		id, _ := x.(string)
		n, err := strconv.Atoi(strings.TrimPrefix(id, "C"))
		if !strings.HasPrefix(id, "C") || err != nil || n < 1 || n > cases || "C"+strconv.Itoa(n) != id {
			return core.Answer{}, fmt.Errorf("bad like %v", e["like"])
		}
		a.Like = append(a.Like, id)
	}
	sure, ok := e["sure"].(bool)
	if !ok {
		return core.Answer{}, errors.New("sure not true or false")
	}
	a.Sure = &sure
	return a, nil
}
