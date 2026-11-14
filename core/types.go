// Package core is ECC-Server's decision core: per line a verdict from the fast model, per player a sitting with its
// held lines, at the checkpoints of the schedule a prompt for the judge, and the actions its ruling calls for. It has
// no clock, network or file of its own: the HTTP server and the replay drive it with the same calls, and run the
// judge calls it asks for.
package core

import (
	"errors"
	"time"
)

// Line is one line as the plugin sends it, with the number the server gives it in its stream.
type Line struct {
	// ID is the line's number in its stream as the judge sees it ("L<n>").
	ID           string
	Time         time.Time
	Channel      string
	Conversation string
	// Private marks a two-player conversation (a private message): the judge sees it among that conversation's lines
	// only.
	Private bool
	// Account is the speaker's player key (UUID); Name is the name shown with the line.
	Account       string
	Name          string
	TargetAccount string
	Target        string
	Text          string
	// Sent is how many times the line was sent in a row (1 for a single send).
	Sent   int
	Groups []string
	// Context marks a line that is no player's own (a Discord relay): it is context for other lines only.
	Context bool
	// Sitting names the sitting the line belongs to when the driver defines sittings (Config.DriverSittings).
	Sitting string
	// Ref is the driver's own id for the line (the plugin's line id); a release action lists the refs of the lines it
	// delivers.
	Ref string
}

// Verdict is the fast model's verdict on one line and the kinds it marks, in the order the judge sees them.
type Verdict struct {
	Hold  bool
	Kinds []string
	Score float64
}

// Scorer is the fast model. It gets the line and its conversation's recent lines (oldest first), with allowlisted
// links replaced by the placeholder.
type Scorer interface {
	Score(line Line, context []Line) (Verdict, error)
}

// Rulings the judge gives.
const (
	RulingNothing = "nothing"
	RulingHold    = "hold"
	RulingBan     = "ban"
)

// Answer is the judge's answer on one checkpoint.
type Answer struct {
	Ruling string   `json:"ruling"`
	By     string   `json:"by,omitempty"`
	Like   []string `json:"like,omitempty"`
	Sure   *bool    `json:"sure,omitempty"`
}

// Judge rules one checkpoint: its Prompt, with Mine and Cases to check the answer against.
type Judge interface {
	Rule(job *Job) (Answer, error)
}

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

// Action is one thing the plugin is told to do. A release names its lines by stream id (Lines) and by the driver's
// refs (Refs: each line's own, then its repeats', in the order they arrived).
type Action struct {
	Kind    string   `json:"kind"`
	Account string   `json:"account"`
	Name    string   `json:"name"`
	Sitting string   `json:"sitting"`
	Lines   []string `json:"lines,omitempty"`
	Refs    []string `json:"refs,omitempty"`
}

// RepeatWindow: a speaker's line with the same text as their previous line in the conversation, within this long of
// that line's last copy, is a repeat of it.
const RepeatWindow = 60 * time.Second

// Example is one of the owner's ruled cases, shown to the judge.
type Example struct {
	Ruling string `json:"ruling"`
	Text   string `json:"text"`
}

// Texts is what the judge's prompt carries besides the sitting: the question, the owner's cases and the answer
// format.
type Texts struct {
	Question string
	Examples []Example
	Format   string
}
