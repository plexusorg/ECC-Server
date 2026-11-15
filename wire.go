package main

import (
	"errors"
	"slices"
	"time"

	"ecc-server/core"
)

// wireLine is one line as the plugin sends it (POST /line). Text and the names are raw: the server prepares them
// (core/text.go) as the training pipeline prepared its lines.
type wireLine struct {
	ID      string `json:"id"`
	Channel string `json:"channel"`
	// SpeakerUUID is the author's UUID, empty only on a Discord relay line; Speaker the account name (a Discord relay:
	// the Discord name as the game line shows it); Shown, on chat (and guild, where the training data had the account
	// name and the server uses it), the name as the console line shows it.
	SpeakerUUID string `json:"speaker_uuid"`
	Speaker     string `json:"speaker"`
	Shown       string `json:"shown"`
	// TargetUUID and Target: a private message's or mail's recipient, the player whose nickname is set.
	TargetUUID string `json:"target_uuid"`
	Target     string `json:"target"`
	Text       string `json:"text"`
	Trusted    bool   `json:"trusted"`
}

// Channels with a meaning of their own on the wire.
const (
	channelDiscord = "discord"
	channelMsg     = "msg"
)

func (w wireLine) check() error {
	switch {
	case w.ID == "" || w.Channel == "" || w.Speaker == "":
		return errors.New("id, channel and speaker are required")
	case w.SpeakerUUID == "" && w.Channel != channelDiscord:
		return errors.New("speaker_uuid is required on every channel but discord")
	}
	return nil
}

// coreLine is the line as the decision core takes it: its text and names prepared, the conversation and privacy
// derived (a private message is a msg line; its conversation the pair of lowercased account names), a Discord relay
// or trusted line context only.
func (w wireLine) coreLine(at time.Time) core.Line {
	l := core.Line{Ref: w.ID, Time: at, Channel: w.Channel, Conversation: w.Channel, Account: w.SpeakerUUID,
		Speaker: w.Speaker, RawName: w.Speaker, TargetAccount: w.TargetUUID, Target: w.Target, Raw: w.Text,
		Context: w.Channel == channelDiscord || w.Trusted}
	l.Key, l.Text = core.PrepareText(w.Text, w.Channel)
	l.Empty = l.Key == ""
	switch w.Channel {
	case channelDiscord:
		name := core.DiscordName(w.Speaker)
		l.Name, l.KeyName = core.Redact(name), core.KeyName(w.Channel, name, "")
		l.Speaker = "discord:" + l.Name
		l.Account = ""
	case "chat":
		shown := core.ShownName(w.Shown)
		if shown == "" {
			shown = w.Speaker
		}
		l.Name, l.KeyName, l.RawName = core.Redact(shown), core.KeyName(w.Channel, shown, w.Speaker), w.Shown
	default:
		l.Name, l.KeyName = w.Speaker, core.KeyName(w.Channel, "", w.Speaker)
	}
	if w.Channel == channelMsg {
		pair := []string{core.Lower(w.Speaker), core.Lower(w.Target)}
		slices.Sort(pair)
		l.Private, l.Conversation = true, "msg/"+pair[0]+"|"+pair[1]
	}
	return l
}
