package model

import "ecc-server/core"

// CoverageRule identifies the shared complete-target routing contract.
const CoverageRule = "complete_target_v1"

// Coverage routes of a line to score.
const (
	CoverageScored   = "scored"   // the complete target fits the classifier's input
	CoverageRejected = "rejected" // it does not; the line is rejected
)

// CoverageRoute checks the actual rendered target without running a model.
func CoverageRoute(raw, channel, target string, tok *Tokenizer, maxTokens int) string {
	if core.SourceShortened(raw, channel) || len(tok.Encode(target))+3 > maxTokens {
		return CoverageRejected
	}
	return CoverageScored
}

// RejectReason reads current context but never appends a rejected line to it.
func (s *Scorer) RejectReason(l core.Line) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	if st.name != l.Stream {
		st = newStream(l.Stream)
	}
	e := s.entryData(l)
	texts := s.inputStrings(e, s.context(st, e))
	if CoverageRoute(l.Raw, l.Channel, texts[len(texts)-1], s.tok, s.spec.Classifier.MaxTokens) == CoverageRejected {
		return "complete target exceeds the model input budget"
	}
	return ""
}
