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

// bedrockJudge rules a checkpoint with one Converse call: the whole prompt as the one user message, the owner's cases
// before the sitting so the prompt's long identical start is cached by Bedrock across calls. Each call is charged to
// the spend budget first. An answer that fails the checks is asked for once more; a second invalid answer, a failed
// call or a call over the budget is an error, which leaves the lines held and puts the sitting on the review queue.
type bedrockJudge struct {
	ctx     context.Context
	br      *bedrock
	spend   *spend
	tier    string
	maxOut  int
	timeout time.Duration
	// told is called when a call is refused for a budget for the first time in its day or month.
	told func(*errOverBudget)
}

func (j *bedrockJudge) Rule(job *core.Job) (core.Answer, error) {
	var invalid error
	for attempt := 1; attempt <= 2; attempt++ {
		text, stop, err := j.call(job)
		if err != nil {
			if invalid != nil {
				return core.Answer{}, fmt.Errorf("the judge's answer was invalid (%v), and the call to ask again failed: %w", invalid, err)
			}
			return core.Answer{}, err
		}
		a, err := parseAnswer(text, job.Mine, job.Cases)
		if err == nil {
			return a, nil
		}
		slog.Warn("the judge's answer is invalid", "sitting", job.Sitting, "k", job.K, "attempt", attempt,
			"why", err.Error(), "stop_reason", stop)
		invalid = err
	}
	return core.Answer{}, fmt.Errorf("the judge's answer was invalid twice: %w", invalid)
}

// call makes one Converse call within the budget and returns the answer's text and stop reason.
func (j *bedrockJudge) call(job *core.Job) (string, string, error) {
	if j.br.key == "" {
		return "", "", fmt.Errorf("%w: no Bedrock key is set; set one with sudo ecc-server bedrock login", core.ErrNotSent)
	}
	r, told, err := j.spend.reserve(j.spend.worst(len(job.Prompt), j.maxOut))
	var over *errOverBudget
	if errors.As(err, &over) {
		if told {
			j.told(over)
		}
		return "", "", fmt.Errorf("%w: %w", core.ErrNotSent, err)
	}
	ctx, cancel := context.WithTimeout(j.ctx, j.timeout)
	defer cancel()
	start := time.Now()
	res, err := j.br.converse(ctx, "", job.Prompt, j.maxOut, j.tier)
	cost := r.usd // no answer: the call may have run
	var be *bedrockError
	switch {
	case err == nil:
		cost = j.spend.cost(res.Usage)
	case errors.As(err, &be):
		cost = 0 // Bedrock refused the request
	}
	j.spend.settle(r, cost)
	j.br.record(err, len(job.Prompt), res, cost, time.Since(start))
	if err != nil {
		return "", "", err
	}
	return res.Text, res.StopReason, nil
}

// fence is an answer wrapped in a Markdown code fence.
var fence = regexp.MustCompile("(?s)^```(?:json)?\\s*(.*?)\\s*```$")

// parseAnswer checks the judge's answer as the measured runs did: one JSON object, optionally fenced, whose
// "rulings" list holds one entry for the one new case, with a ruling of nothing, hold or ban, for hold or ban "by" one
// of the player's lines the case shows, "like" a list of at most three of the owner's case ids, and "sure" true or
// false.
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
	if a.Ruling != core.RulingNothing && a.Ruling != core.RulingHold && a.Ruling != core.RulingBan {
		return core.Answer{}, fmt.Errorf("bad ruling: %v", e["ruling"])
	}
	a.By, _ = e["by"].(string)
	if a.Ruling != core.RulingNothing && !slices.Contains(mine, a.By) {
		return core.Answer{}, fmt.Errorf("by %v is not one of the player's lines", e["by"])
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
