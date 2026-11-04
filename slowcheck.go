package main

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// The slow check of each pending player: Kimi on Bedrock reports evidence on the triggering conversation, and the
// evidence rules (evidence.go) decide every participant's outcome. It runs as soon as a player triggers, again on
// each new line of a player it is not sure about yet, and for every pending player when the host starts. A failed
// call is retried with backoff until Bedrock answers; the mute never lifts on a failure.
//
// The transcript is the pending state's conversation up to and including the trigger, followed by the player's own
// later lines. The views are the whole transcript and the transcript cut at each decided player's trigger. Thresholds
// are those of the bundle the trigger was scored with; the system prompt is the served bundle's, and every call
// records which bundle's prompt it used.

const (
	slowWorkers    = 4
	slowFirstRetry = 30 * time.Second
	slowLastRetry  = 10 * time.Minute
	slowPromptWait = 5 * time.Second
	slowTick       = 15 * time.Second
	kimiRetries    = 3 // Kimi.ask's retries, 2, 4 and 8 seconds apart
)

var channelNames = map[string]string{"chat": "public chat", "me": "public chat", "nick": "public chat",
	"msg": "private messages", "mail": "mail", "sign": "signs placed in the world", "book": "a written book",
	"anvil": "item names"}

// channelName is the channel as the user message names it; a module chat channel such as guilds:chat is
// "guilds chat".
func channelName(channel string) string {
	if n, ok := channelNames[channel]; ok {
		return n
	}
	prefix, _, _ := strings.Cut(channel, ":")
	return prefix + " chat"
}

type slowResult int

const (
	slowDone slowResult = iota
	slowHeld
	slowRetry
	slowWait
)

// slowCase is one pending player's slow check. Fields other than answers are guarded by slowChecker.mu; answers is
// used only by the one run in progress.
type slowCase struct {
	uuid, name string
	// Calls that answered, by bundle and user message, so a re-run does not ask an unchanged view again.
	answers  map[string]*kimiCall
	running  bool
	rerun    bool
	held     bool
	failing  bool
	loaded   bool
	heldTime time.Duration
	// Held time while the current re-check runs; added to heldTime when it ends with a verdict.
	runTime  time.Duration
	lastTick time.Time
	retry    time.Duration
}

type slowChecker struct {
	s    *server
	pend *pendingStore
	acts *actionStore
	rec  *recordLog
	cap  time.Duration
	ctx  context.Context
	stop context.CancelFunc
	sem  chan struct{}
	runs sync.WaitGroup

	mu            sync.Mutex
	cases         map[string]*slowCase
	failingSince  time.Time
	answeredSince time.Time
	failure       string
	closed        bool
}

func newSlowChecker(s *server, pend *pendingStore, acts *actionStore, rec *recordLog, cap time.Duration) *slowChecker {
	ctx, stop := context.WithCancel(context.Background())
	sl := &slowChecker{s: s, pend: pend, acts: acts, rec: rec, cap: cap, ctx: ctx, stop: stop,
		sem: make(chan struct{}, slowWorkers), cases: map[string]*slowCase{}}
	go func() {
		t := time.NewTicker(slowTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sl.tick()
				pend.flush()
				acts.flush()
			}
		}
	}()
	return sl
}

// check checks a pending player: at their trigger, at start, and on each new line. A check already running for them
// runs once more when it ends, with the newer lines.
func (sl *slowChecker) check(uuid, name string) {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	if sl.closed || !sl.pend.contains(uuid) {
		return
	}
	c := sl.cases[uuid]
	if c == nil {
		c = &slowCase{uuid: uuid, name: name, answers: map[string]*kimiCall{}, retry: slowFirstRetry}
		sl.cases[uuid] = c
	}
	if c.running {
		c.rerun = true
		return
	}
	c.running = true
	sl.submit(c, 0)
}

// submit runs the case after delay. The caller holds mu.
func (sl *slowChecker) submit(c *slowCase, delay time.Duration) {
	time.AfterFunc(delay, func() {
		sl.mu.Lock()
		if sl.closed {
			sl.mu.Unlock()
			return
		}
		sl.runs.Add(1)
		sl.mu.Unlock()
		defer sl.runs.Done()
		select {
		case sl.sem <- struct{}{}:
		case <-sl.ctx.Done():
			return
		}
		defer func() { <-sl.sem }()
		sl.run(c)
	})
}

func (sl *slowChecker) run(c *slowCase) {
	result := func() (r slowResult) {
		defer func() {
			if p := recover(); p != nil {
				slog.Error("the slow check failed; retrying", "player", c.name, "panic", fmt.Sprint(p))
				r = slowRetry
			}
		}()
		return sl.judge(c)
	}()
	sl.mu.Lock()
	defer sl.mu.Unlock()
	if sl.closed {
		return
	}
	switch result {
	case slowDone:
		delete(sl.cases, c.uuid)
	case slowWait:
		c.runTime = 0
		sl.submit(c, slowPromptWait)
	case slowRetry:
		// The retry reads the latest lines, so a rerun asked for meanwhile is part of it.
		c.failing, c.rerun = true, false
		// A failed run was not real verdict time.
		c.runTime = 0
		wait := c.retry
		c.retry = min(2*wait, slowLastRetry)
		sl.submit(c, wait)
	case slowHeld:
		sl.held(c)
	}
}

// held ends a run whose verdict was not sure yet. The caller holds mu.
func (sl *slowChecker) held(c *slowCase) {
	c.failing = false
	c.retry = slowFirstRetry
	c.heldTime += c.runTime
	c.runTime = 0
	if !c.held {
		c.held = true
		c.lastTick = time.Now()
	}
	// The cap ends the pending state even while the player keeps sending lines.
	if c.heldTime >= sl.cap {
		sl.lift(c)
		return
	}
	if c.rerun {
		c.rerun = false
		sl.submit(c, 0)
		return
	}
	c.running = false
}

// tick ends a held player's pending state after the cap: real verdict time, not counting time while Bedrock failed.
// Time while a re-check runs counts only once it ends with a verdict, since a call is known to fail only when it
// ends.
func (sl *slowChecker) tick() {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	now := time.Now()
	for _, c := range sl.cases {
		if c.held && !c.failing && sl.failingSince.IsZero() {
			if c.running {
				c.runTime += now.Sub(c.lastTick)
			} else {
				c.heldTime += now.Sub(c.lastTick)
			}
		}
		c.lastTick = now
		if c.held && !c.running && c.heldTime >= sl.cap {
			sl.lift(c)
		}
	}
}

// lift releases a player held past the cap. The caller holds mu.
func (sl *slowChecker) lift(c *slowCase) {
	delete(sl.cases, c.uuid)
	sl.pend.remove(c.uuid)
	sl.acts.unmute(c.uuid, c.name, roleHeld, "cap", fmt.Sprintf("%d seconds not sure", int(c.heldTime.Seconds())))
}

// transcript is a pending state's transcript: the conversation up to and including the trigger, then the player's
// later lines. triggerOf holds each speaker's first triggering line and the pending player's own trigger.
type transcript struct {
	lines     []evLine
	uuids     map[string]string
	bypass    map[string]bool
	triggerOf map[string]int
	trigger   int
	channel   string
}

func transcriptOf(st *pendingState) transcript {
	var msgs []msgRecord
	for _, l := range st.Conversation {
		msgs = append(msgs, l)
		if l.ID == st.Trigger.ID {
			break
		}
	}
	if len(msgs) == 0 || msgs[len(msgs)-1].ID != st.Trigger.ID {
		msgs = append(msgs, st.Trigger)
	}
	t := transcript{trigger: len(msgs) - 1, uuids: map[string]string{}, bypass: map[string]bool{},
		triggerOf: map[string]int{}, channel: channelName(st.Trigger.Channel)}
	msgs = append(msgs, st.Later...)
	for i, m := range msgs {
		t.lines = append(t.lines, evLine{speaker: m.Speaker, text: m.Text, pBan: m.PBan})
		if _, ok := t.uuids[m.Speaker]; !ok {
			t.uuids[m.Speaker] = m.SpeakerUUID
		}
		if m.Decision == decisionBypass {
			t.bypass[m.Speaker] = true
		}
		if _, ok := t.triggerOf[m.Speaker]; !ok && m.Decision == decisionTrigger {
			t.triggerOf[m.Speaker] = i
		}
	}
	t.triggerOf[st.Trigger.Speaker] = t.trigger
	return t
}

// slowRun is one run of a check: what it judges and what its calls cost.
type slowRun struct {
	c                   *slowCase
	t                   transcript
	bundle              string // the trigger's bundle
	prompt              string // the bundle whose system prompt the run asks with
	tokensIn, tokensOut int
}

type decidedSpeaker struct {
	speaker string
	trigger int // -1 when they never triggered
}

func (sl *slowChecker) judge(c *slowCase) slowResult {
	st := sl.pend.snapshot(c.uuid)
	if st == nil {
		return slowDone
	}
	sl.s.mu.Lock()
	m := sl.s.cur
	sl.s.mu.Unlock()
	if m == nil {
		return slowWait
	}
	sl.mu.Lock()
	if !c.loaded {
		c.loaded = true
		c.heldTime = time.Duration(st.HeldMs) * time.Millisecond
	}
	sl.mu.Unlock()
	run := &slowRun{c: c, t: transcriptOf(st), bundle: st.Decision.Version, prompt: m.bundle.Version}
	decided := sl.decided(run)
	views := sl.views(run, decided)
	if views == nil {
		return slowRetry
	}
	lines := run.t.lines
	full := views[len(lines)-1]
	outcomes := map[string]outcome{}
	for _, d := range decided {
		var o outcome
		if d.trigger < 0 {
			o = decideUntriggered(d.speaker, full, lines, st.Decision.Corroboration)
		} else {
			o = decideTriggered(d.speaker, d.trigger, full, views[d.trigger], lines, st.Decision.Clearing)
		}
		outcomes[d.speaker] = o
		sl.recordVerdict(run, d.speaker, d.trigger, o, views)
	}
	return sl.apply(run, decided, outcomes)
}

// decided lists each judged speaker this check decides, with their trigger's index; skipped speakers are recorded.
func (sl *slowChecker) decided(run *slowRun) []decidedSpeaker {
	var out []decidedSpeaker
	for _, sp := range speakers(run.t.lines) {
		uuid := run.t.uuids[sp]
		skip := ""
		switch {
		case uuid == run.c.uuid:
			out = append(out, decidedSpeaker{sp, run.t.trigger})
			continue
		case sl.pend.contains(uuid):
			skip = "pending"
		case run.t.bypass[sp]:
			skip = "bypass"
		}
		if skip != "" {
			sl.recordVerdict(run, sp, -1, outcome{"skipped", skip}, nil)
			continue
		}
		trigger, ok := run.t.triggerOf[sp]
		if !ok {
			trigger = -1
		}
		out = append(out, decidedSpeaker{sp, trigger})
	}
	return out
}

// views returns the validated evidence of every view, keyed by the view's last index; nil when a call failed.
func (sl *slowChecker) views(run *slowRun, decided []decidedSpeaker) map[int]view {
	lines := run.t.lines
	cuts := []int{len(lines) - 1}
	for _, d := range decided {
		if d.trigger >= 0 && !slices.Contains(cuts, d.trigger) {
			cuts = append(cuts, d.trigger)
		}
	}
	sort.Ints(cuts)
	out := map[int]view{}
	for _, cut := range cuts {
		v := lines[:cut+1]
		user := userMessage(run.t.channel, v)
		key := run.prompt + "\x00" + user
		call := run.c.answers[key]
		if call == nil {
			call = sl.ask(user, speakers(v))
			if sl.ctx.Err() != nil {
				return nil
			}
			sl.observe(call)
			run.tokensIn += call.tokensIn
			run.tokensOut += call.tokensOut
			sl.recordCall(run, cut, call)
			if call.answer == nil {
				return nil
			}
			run.c.answers[key] = call
		}
		out[cut] = validate(call.answer, lines, cut, run.t.triggerOf)
	}
	return out
}

// apply carries out the verdicts: bans first, the other participants before the pending player, then ends the
// pending state. A participant who triggered while this check ran is left to their own check.
func (sl *slowChecker) apply(run *slowRun, decided []decidedSpeaker, outcomes map[string]outcome) slowResult {
	self := ""
	for _, d := range decided {
		uuid, o := run.t.uuids[d.speaker], outcomes[d.speaker]
		if uuid == run.c.uuid {
			self = d.speaker
		} else if bans(o) && !sl.pend.contains(uuid) {
			sl.acts.ban(uuid, d.speaker, o.role, o.cause)
		}
	}
	o := outcomes[self]
	if bans(o) {
		sl.acts.ban(run.c.uuid, self, o.role, o.cause)
	}
	if o.role == roleHeld {
		sl.mu.Lock()
		held := run.c.heldTime
		sl.mu.Unlock()
		sl.pend.setHeld(run.c.uuid, held)
		return slowHeld
	}
	sl.pend.remove(run.c.uuid)
	if o.role == roleNotInvolved {
		sl.acts.unmute(run.c.uuid, self, o.role, o.cause, "")
	}
	return slowDone
}

func bans(o outcome) bool {
	return o.role == roleOffender || o.role == roleAtRisk
}

// kimiCall is one evidence call, made as redteam.py's Kimi.ask makes it: up to three retries, two, four and eight
// seconds apart; a failed call to Bedrock, a refusal, the token cap and an answer that fails the evidence check are
// retried. answer is nil when every attempt failed.
type kimiCall struct {
	answer              []reported
	raw                 string
	version             string // the bundle whose system prompt the last answered attempt sent
	attempts            int
	errors              []string
	tokensIn, tokensOut int
	reached             bool // Bedrock answered an attempt
}

func (sl *slowChecker) ask(user string, players []string) *kimiCall {
	call := &kimiCall{errors: []string{}}
	for attempt := 0; attempt <= kimiRetries; attempt++ {
		call.attempts++
		if attempt > 0 {
			select {
			case <-sl.ctx.Done():
				return call
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
		}
		res, err := sl.s.slowCheck(sl.ctx, user)
		if err != nil {
			if sl.ctx.Err() != nil {
				return call
			}
			call.errors = append(call.errors, "api: "+truncate(err.Error(), 200))
			continue
		}
		call.reached, call.version = true, res.Version
		u := res.Usage
		call.tokensIn += u.InputTokens + u.CacheReadInputTokens + u.CacheWriteInputTokens
		call.tokensOut += u.OutputTokens
		call.raw = res.Text
		if res.StopReason == "content_filtered" || res.StopReason == "guardrail_intervened" || pyStrip(res.Text) == "" {
			call.errors = append(call.errors, "refused: "+res.StopReason)
			continue
		}
		if res.StopReason == "max_tokens" {
			call.errors = append(call.errors, "token cap")
			continue
		}
		answer, err := evidenceCheck(parseAnswer(res.Text), players)
		if err == nil {
			call.answer = answer
			break
		}
		call.errors = append(call.errors, "unparseable: "+err.Error())
	}
	return call
}

// observe tracks whether Bedrock answers, warning once when it starts failing and once when it answers again.
// bedrock.go logs each call and a refused key.
func (sl *slowChecker) observe(call *kimiCall) {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	if call.reached {
		if !sl.failingSince.IsZero() {
			slog.Info("Bedrock answers again; the slow check resumes")
		}
		sl.failingSince = time.Time{}
		if sl.answeredSince.IsZero() {
			sl.answeredSince = time.Now()
		}
		return
	}
	sl.answeredSince = time.Time{}
	if len(call.errors) > 0 {
		sl.failure = call.errors[len(call.errors)-1]
	}
	if sl.failingSince.IsZero() {
		sl.failingSince = time.Now()
		slog.Warn("Bedrock is failing; pending players stay muted and their slow checks retry", "failure", sl.failure)
	}
}

type slowLineBase struct {
	TS           string `json:"ts"`
	Player       string `json:"player"`
	PlayerUUID   string `json:"player_uuid"`
	Bundle       string `json:"bundle"`
	PromptBundle string `json:"prompt_bundle"`
	Lines        int    `json:"lines"`
}

func (run *slowRun) base() slowLineBase {
	return slowLineBase{TS: stamp(time.Now()), Player: run.c.name, PlayerUUID: run.c.uuid, Bundle: run.bundle,
		PromptBundle: run.prompt, Lines: len(run.t.lines)}
}

func (sl *slowChecker) recordCall(run *slowRun, cut int, call *kimiCall) {
	base := run.base()
	if call.version != "" {
		base.PromptBundle = call.version
	}
	sl.rec.append("slow_call", struct {
		slowLineBase
		ViewEnd   int      `json:"view_end"`
		Whole     bool     `json:"whole"`
		OK        bool     `json:"ok"`
		Attempts  int      `json:"attempts"`
		Errors    []string `json:"errors"`
		TokensIn  int      `json:"tokens_in"`
		TokensOut int      `json:"tokens_out"`
		Raw       string   `json:"raw"`
	}{base, cut + 1, cut == len(run.t.lines)-1, call.answer != nil, call.attempts, call.errors, call.tokensIn,
		call.tokensOut, call.raw})
}

type quoteLine struct {
	Line int    `json:"line"`
	Tag  string `json:"tag"`
	Text string `json:"text"`
}

type evidenceLine struct {
	Quotes  []quoteLine `json:"quotes"`
	Dropped int         `json:"dropped"`
	Given   int         `json:"given"`
	Ages    []string    `json:"ages"`
}

func (sl *slowChecker) recordVerdict(run *slowRun, speaker string, trigger int, o outcome, views map[int]view) {
	evidence := map[string]evidenceLine{}
	for cut, v := range views {
		for _, k := range v {
			if k.player != speaker {
				continue
			}
			e := evidenceLine{Quotes: []quoteLine{}, Dropped: k.dropped, Given: k.given, Ages: []string{}}
			for _, q := range k.quotes {
				e.Quotes = append(e.Quotes, quoteLine{q.index + 1, q.tag, run.t.lines[q.index].text})
			}
			for _, a := range k.ages {
				e.Ages = append(e.Ages, a.String())
			}
			evidence[fmt.Sprint(cut+1)] = e
		}
	}
	var triggerLine *int
	if trigger >= 0 {
		n := trigger + 1
		triggerLine = &n
	}
	sl.rec.append("slow_verdict", struct {
		slowLineBase
		Speaker     string                  `json:"speaker"`
		SpeakerUUID string                  `json:"speaker_uuid"`
		TriggerLine *int                    `json:"trigger_line,omitempty"`
		Outcome     string                  `json:"outcome"`
		Cause       string                  `json:"cause"`
		Evidence    map[string]evidenceLine `json:"evidence"`
		TokensIn    int                     `json:"tokens_in"`
		TokensOut   int                     `json:"tokens_out"`
	}{run.base(), speaker, run.t.uuids[speaker], triggerLine, o.role, o.cause, evidence, run.tokensIn, run.tokensOut})
}

// slowStatus is the slow check as /status shows it. Answering is nil before the first call; Since is when Bedrock
// started answering or failing.
type slowStatus struct {
	Answering *bool     `json:"answering"`
	Failure   string    `json:"failure,omitempty"`
	Since     time.Time `json:"since,omitzero"`
	Checking  []string  `json:"checking"`
	Held      []string  `json:"held"`
}

func (sl *slowChecker) status() slowStatus {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	st := slowStatus{Checking: []string{}, Held: []string{}}
	for _, c := range sl.cases {
		if c.held {
			st.Held = append(st.Held, c.name)
		} else {
			st.Checking = append(st.Checking, c.name)
		}
	}
	sortNames(st.Checking)
	sortNames(st.Held)
	switch {
	case !sl.failingSince.IsZero():
		no := false
		st.Answering, st.Failure, st.Since = &no, sl.failure, sl.failingSince
	case !sl.answeredSince.IsZero():
		yes := true
		st.Answering, st.Since = &yes, sl.answeredSince
	}
	return st
}

func sortNames(names []string) {
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
}

// close stops every check; each resumes from the pending state at the next start.
func (sl *slowChecker) close() {
	sl.mu.Lock()
	sl.closed = true
	sl.mu.Unlock()
	sl.stop()
	done := make(chan struct{})
	go func() {
		sl.runs.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		slog.Warn("a slow check did not stop within 5 seconds")
	}
}
