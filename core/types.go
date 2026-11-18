// Package core is ECC-Server's decision core: per line a verdict from the fast model, per player a session with its
// held lines, at the checkpoints of the schedule a prompt for the judge, and the actions its ruling calls for. It has
// no clock, network or file of its own: the HTTP server and the replay drive it with the same calls, and run the
// judge calls it asks for.
package core

import (
	"errors"
	"time"
)

// Line is one line with its text prepared (text.go), with the number the server gives it in its stream.
type Line struct {
	// ID is the line's number in its stream as the judge sees it ("L<n>"); Stream names the stream (set by the engine).
	ID           string
	Stream       string
	Time         time.Time
	Channel      string
	Conversation string
	// Private marks a two-player conversation (a private message): the judge sees it among that conversation's lines
	// only.
	Private bool
	// Account is the speaker's player key (UUID); Name is the name the judge sees with the line (the prepared shown
	// name on chat and guild, the account name elsewhere, a Discord relay's prepared name); Speaker is the key the fast
	// model knows the speaker by: the account name, or for a Discord relay "discord:" and its prepared name. RawName is
	// the name as sent, for the record.
	Account       string
	Name          string
	Speaker       string
	RawName       string
	TargetAccount string
	Target        string
	// Text is the prepared text (steps 1-7); Raw the text as sent. Key is the text and KeyName the name a repeat is
	// keyed by (KeyName): the text after step 6 and the name before step 7. Empty marks a text that preparation
	// emptied: the line is delivered and dropped.
	Text    string
	Raw     string
	Key     string
	KeyName string
	Empty   bool
	// Sent is how many times the line was sent (1 for a single send): it and its repeats.
	Sent int
	// Context marks a line that is context for other lines only: no player's own (a Discord relay), or a trusted
	// player's, which the plugin delivered at once. It is never scored, held or judged and opens no session.
	Context bool
	// Session names the session the line belongs to when the driver defines sessions (Config.DriverSessions).
	Session string
	// Ref is the driver's own id for the line (the plugin's line id); a release action lists the refs of the lines it
	// delivers.
	Ref string
}

// Verdict is the fast model's verdict on one line, the kinds it marks (in the order the judge sees them), and its
// scores when it gives them: SH's and, for a line SH passed, C's hold logit.
type Verdict struct {
	Hold  bool
	Kinds []string
	SH    *float64
	C     *float64
}

// Scorer is the fast model. It takes every line of the stream that is not a repeat, in the order the engine takes
// them, with ModelText applied and each allowed link replaced by the canonical link: Score for a line to score, See for a
// line that is only context for later lines. It keeps the context it needs from those lines itself.
type Scorer interface {
	Score(line Line) (Verdict, error)
	See(line Line)
}

// CoverageScorer checks the complete target without remembering it or running inference.
type CoverageScorer interface {
	RejectReason(line Line) string
}

// Rulings the judge gives.
const (
	RulingNothing = "nothing"
	RulingHold    = "hold"
	RulingBan     = "ban"
	// RulingNotBan is a ruling of the owner's cases; a judge that answers it is taken as ruling hold.
	RulingNotBan = "not ban"
)

// Answer is the judge's answer on one checkpoint.
type Answer struct {
	Ruling string `json:"ruling"`
	// Given is the judge's own word when the core takes it as another ruling (not ban as hold).
	Given string   `json:"given,omitempty"`
	By    string   `json:"by,omitempty"`
	Like  []string `json:"like,omitempty"`
	Sure  *bool    `json:"sure,omitempty"`
	// TrustedHuman is set only by a trusted driver, never from a saved model answer.
	TrustedHuman bool    `json:"-"`
	RequestID    string  `json:"request_id,omitempty"`
	ResponseID   string  `json:"response_id,omitempty"`
	PromptHash   string  `json:"prompt_sha256,omitempty"`
	Raw          string  `json:"raw,omitempty"`
	Confirmation *Answer `json:"confirmation,omitempty"`
}

// effect separates the model's diagnostic answer from the host's enforcement.
func (a Answer) effect(job *Job) (string, string) {
	if a.TrustedHuman {
		return a.Ruling, ""
	}
	if a.Sure == nil || !*a.Sure {
		return RulingHold, "the judge was not firm"
	}
	if a.Ruling != RulingBan {
		return a.Ruling, ""
	}
	b := a.Confirmation
	if b == nil {
		return RulingHold, "the ban has no independent confirmation"
	}
	if b.Sure == nil || !*b.Sure {
		return RulingHold, "the ban confirmation was not firm"
	}
	if b.Ruling != RulingBan {
		return RulingHold, "the firm judge rulings disagree"
	}
	if a.RequestID == "" || b.RequestID == "" || a.RequestID == b.RequestID ||
		a.ResponseID == "" || b.ResponseID == "" || a.ResponseID == b.ResponseID ||
		a.PromptHash != job.Hash || b.PromptHash != job.Hash {
		return RulingHold, "the ban confirmation has no independent response on this prompt"
	}
	return RulingBan, ""
}

// Judge rules one checkpoint: its Prompt, with Mine and Cases to check the answer against.
type Judge interface {
	Rule(job *Job) (Answer, error)
}

// ErrLater is the driver's error for a line it answered hold before its score (errors.Is): it hands the verdict to
// Engine.Rescored once the line is scored. Until then the line is held but none of its session's held lines for the
// judge, and the session's checkpoints wait for it.
var ErrLater = errors.New("answered before its score")

// ErrNotSent is the error a driver hands to Engine.Rule for a checkpoint whose prompt it chose not to send.
var ErrNotSent = errors.New("the checkpoint was not sent to the judge")

// Recorder takes the record's lines: kind names the line type, v is a struct.
type Recorder interface {
	Record(kind string, v any)
}

// Actions the plugin carries out.
const (
	ActionBan     = "ban"     // indefinite ban, kick at once; the player's held lines are dropped
	ActionWarn    = "warn"    // the held lines stay held; the player is shown the warning
	ActionRelease = "release" // deliver these held lines late, in order
)

// Action is one thing the plugin is told to do, for the session's player: Account is their key (Line.Account) and
// Speaker their account name (Line.Speaker), never a shown name. A release names its lines by stream id (Lines) and by
// the driver's refs (Refs: the lines' own and their repeats', in the order they arrived).
type Action struct {
	Kind    string   `json:"kind"`
	Account string   `json:"account"`
	Speaker string   `json:"speaker"`
	Session string   `json:"session"`
	Lines   []string `json:"lines,omitempty"`
	Refs    []string `json:"refs,omitempty"`
}

// RepeatWindow: a line with the same speaker (shown name, any case), channel, target and text as an earlier line, within
// this long of that line's last copy (in whole seconds), is a repeat of it, whatever lines came between.
const RepeatWindow = 60 * time.Second

// Example is one of the owner's ruled cases, shown to the judge.
type Example struct {
	Ruling string `json:"ruling"`
	Text   string `json:"text"`
}

// Texts is what the judge's prompt carries besides the session: the question, the owner's cases and the answer
// format.
type Texts struct {
	Question string
	Examples []Example
	Format   string
}
