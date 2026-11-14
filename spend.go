package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// spend keeps what the judge's calls cost in the current UTC day and month, in <data>/spend.json, rewritten on every
// change so a restart keeps the totals. Before a call the caller reserves the call's worst case: the whole prompt at
// the dearest input price plus the output limit at the output price. The reservation is saved as spent at once, so a
// call cut off by a crash or a restart stays charged its worst case; when the call returns, its cost from Bedrock's
// usage numbers replaces the reservation. A reservation that would take the day's or the month's total past its budget
// waits while other calls are in flight and could still settle below their worst case, and is refused once none is.
type spend struct {
	path            string
	daily, monthly  float64
	in, out, cr, cw float64 // dollars per token, after the tier's discount

	mu      sync.Mutex
	settled *sync.Cond // signalled when a reservation settles
	st      spendState
	unsaved bool
}

type spendState struct {
	Day      string  `json:"day"`
	DayUSD   float64 `json:"day_usd"`
	Calls    int     `json:"day_calls"`
	Month    string  `json:"month"`
	MonthUSD float64 `json:"month_usd"`
	// In flight: the worst cases of the calls not yet settled, already counted in DayUSD and MonthUSD.
	ReservedUSD float64 `json:"reserved_usd"`
	// The day and month whose budget's use-up staff were told about.
	ToldDay   string `json:"told_day,omitempty"`
	ToldMonth string `json:"told_month,omitempty"`
}

// errOverBudget: a call was not made because its worst case would take a total past its budget.
type errOverBudget struct {
	period string // "daily" or "monthly"
	budget float64
}

func (e *errOverBudget) Error() string {
	return fmt.Sprintf("the judge's %s budget of $%.2f is used up", e.period, e.budget)
}

func openSpend(cfg Config) (*spend, error) {
	b := cfg.Budget
	f := 1e-6
	if cfg.Judge.ServiceTier == "flex" {
		f /= 2
	}
	s := &spend{path: filepath.Join(cfg.Data, "spend.json"), daily: b.DailyUSD, monthly: b.MonthlyUSD,
		in: b.InputPerMillion * f, out: b.OutputPerMillion * f, cr: b.CacheReadPerMillion * f,
		cw: b.CacheWritePerMillion * f}
	s.settled = sync.NewCond(&s.mu)
	if err := readJSON(s.path, &s.st); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("spend: %w", err)
	}
	if s.st.ReservedUSD != 0 {
		slog.Warn("calls to the judge were in flight when the server stopped; they stay charged their worst case",
			"usd", fmt.Sprintf("%.4f", s.st.ReservedUSD))
		s.st.ReservedUSD = 0
	}
	s.mu.Lock()
	s.roll(time.Now())
	s.save()
	s.mu.Unlock()
	return s, nil
}

// roll starts a new day's or month's total when the UTC date has moved on. The caller holds mu.
func (s *spend) roll(now time.Time) {
	day, month := now.UTC().Format(time.DateOnly), now.UTC().Format("2006-01")
	if s.st.Day != day {
		s.st.Day, s.st.DayUSD, s.st.Calls = day, 0, 0
	}
	if s.st.Month != month {
		s.st.Month, s.st.MonthUSD = month, 0
	}
}

// worst is the most a call with a prompt of this many bytes can cost: every byte a token (a token is at least one
// byte) plus a margin for the chat template, at the dearest input price, and the whole output limit.
func (s *spend) worst(promptBytes, maxOutput int) float64 {
	return float64(promptBytes+256)*max(s.in, s.cr, s.cw) + float64(maxOutput)*s.out
}

// reservation is one call's reserved worst case, charged to the day and month it was made in.
type reservation struct {
	day, month string
	usd        float64
}

// reserve charges a call's worst case before it is made. It returns *errOverBudget, with told set the first time
// that budget is refused in its day or month, when the call does not fit.
func (s *spend) reserve(usd float64) (r reservation, told bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		s.roll(time.Now())
		var over *errOverBudget
		switch {
		case s.st.DayUSD+usd > s.daily:
			over = &errOverBudget{"daily", s.daily}
		case s.st.MonthUSD+usd > s.monthly:
			over = &errOverBudget{"monthly", s.monthly}
		}
		if over == nil {
			break
		}
		if s.st.ReservedUSD > 0 {
			// A call in flight settles at its actual cost, which may make room.
			s.settled.Wait()
			continue
		}
		switch {
		case over.period == "daily" && s.st.ToldDay != s.st.Day:
			s.st.ToldDay, told = s.st.Day, true
		case over.period == "monthly" && s.st.ToldMonth != s.st.Month:
			s.st.ToldMonth, told = s.st.Month, true
		}
		if told {
			s.save()
		}
		return reservation{}, told, over
	}
	s.st.DayUSD += usd
	s.st.MonthUSD += usd
	s.st.ReservedUSD += usd
	s.st.Calls++
	s.save()
	return reservation{s.st.Day, s.st.Month, usd}, false, nil
}

// tokenUsage is Bedrock's token counts for one call.
type tokenUsage struct {
	Input      int `json:"input_tokens"`
	Output     int `json:"output_tokens"`
	CacheRead  int `json:"cache_read_input_tokens"`
	CacheWrite int `json:"cache_write_input_tokens"`
}

func (s *spend) cost(u tokenUsage) float64 {
	return float64(u.Input)*s.in + float64(u.Output)*s.out + float64(u.CacheRead)*s.cr + float64(u.CacheWrite)*s.cw
}

// settle replaces a reservation with the call's cost: its usage when Bedrock answered, nothing when Bedrock refused
// the request, and the reserved worst case when the call got no answer (it may have run).
func (s *spend) settle(r reservation, cost float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.ReservedUSD = max(0, s.st.ReservedUSD-r.usd)
	if s.st.Day == r.day {
		s.st.DayUSD += cost - r.usd
	}
	if s.st.Month == r.month {
		s.st.MonthUSD += cost - r.usd
	}
	s.save()
	s.settled.Broadcast()
}

func (s *spend) status() spendStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roll(time.Now())
	return spendStatus{Day: s.st.Day, DayUSD: s.st.DayUSD, DailyBudget: s.daily, Calls: s.st.Calls,
		Month: s.st.Month, MonthUSD: s.st.MonthUSD, MonthlyBudget: s.monthly, ReservedUSD: s.st.ReservedUSD}
}

type spendStatus struct {
	Day           string  `json:"day"`
	DayUSD        float64 `json:"day_usd"`
	DailyBudget   float64 `json:"daily_budget_usd"`
	Calls         int     `json:"day_calls"`
	Month         string  `json:"month"`
	MonthUSD      float64 `json:"month_usd"`
	MonthlyBudget float64 `json:"monthly_budget_usd"`
	ReservedUSD   float64 `json:"in_flight_reserved_usd"`
}

// save writes the totals. A failed write keeps them in memory and is retried on the next change. The caller holds mu.
func (s *spend) save() {
	data, _ := json.Marshal(s.st)
	if err := writeAtomic(s.path, data); err != nil {
		if !s.unsaved {
			slog.Error("could not save the judge's spend; retrying on the next call", "err", err)
		}
		s.unsaved = true
		return
	}
	s.unsaved = false
}
