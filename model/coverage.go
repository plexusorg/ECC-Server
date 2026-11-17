package model

import "ecc-server/core"

// CoverageRule identifies the shared complete-target routing contract.
const CoverageRule = "complete_target_v1"

// CoverageRoute checks the actual rendered target without running a model.
func CoverageRoute(raw, channel, target string, c *Tokenizer, maxTokens int) string {
	if core.SourceShortened(raw, channel) || len(c.Encode(target))+3 > maxTokens {
		return "reject"
	}
	return "c"
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
	texts := s.cStrings(e, s.context(st, e))
	if CoverageRoute(l.Raw, l.Channel, texts[len(texts)-1], s.cTok, s.spec.C.MaxTokens) == "reject" {
		return "complete target exceeds the model input budget"
	}
	return ""
}
