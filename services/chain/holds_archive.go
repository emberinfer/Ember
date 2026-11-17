package chain

import "reflect"

// LookupHold returns the complete historical record. Callers must not mutate
// archived records; Apply first restores a private copy for action validation.
func (s *State) LookupHold(id string) *Hold {
	if h := s.Holds[id]; h != nil {
		return h
	}
	if s.Version >= 11 && s.history != nil {
		return s.history.holds[id]
	}
	return nil
}
func (s *State) historicalHolds() map[string]*Hold {
	if s.Version < 11 || s.history == nil || len(s.history.holds) == 0 {
		return s.Holds
	}
	out := make(map[string]*Hold, len(s.Holds)+len(s.history.holds))
	for id, h := range s.history.holds {
		out[id] = h
	}
	for id, h := range s.Holds {
		out[id] = h
	}
	return out
}

// The apparent closed states are settled, cancelled and slashed (settleAt,
// settlePipeline, settleMoe, cancel and expire_hold). Ordinary submit/audit/
// settle/cancel/expiry require frozen/dispute, so have no edges from these.
// BUT registerAppeal can reopen a closed whole-model hold, and commitThird-
// Adjudication can still write an outstanding commitment. Exclude both cases
// and every unsettled deposit: only records with no remaining outgoing edge
// are terminal. Pipeline/MoE appeals are explicitly disabled by adjudicationEnabled.
func (s *State) terminalHold(h *Hold) bool {
	if h == nil {
		return false
	}
	switch h.State {
	case "settled", "cancelled", "slashed":
	default:
		return false
	}
	if c := h.Adjudication; c != nil {
		if c.Deposit.From != "" && !c.Deposit.Settled {
			return false
		}
		if c.ThirdAuditor != "" && c.ThirdCommitment == "" {
			return false
		}
	}
	if h.Pipeline == nil && h.Moe == nil && h.Adjudication == nil && (h.Sampling == nil || !h.Sampling.Sampled) && h.Auditor == "" && h.AuditVerdict == "" {
		delivery := h.SettleHeight
		if delivery == 0 {
			delivery = h.SubmitHeight
		}
		if delivery > 0 && (s.Height < delivery || s.Height-delivery <= s.Parameters.Adjudication.AppealWindowBlocks) {
			return false
		}
	}
	return true
}
func (s *State) archiveTerminalHolds() {
	if s.Version < 11 {
		return
	}
	// pruneBlockHashes installs a private archive view at every block boundary.
	for id, h := range s.Holds {
		if s.terminalHold(h) {
			s.history.holds[id] = h
			s.history.pendingHolds[id] = h
			delete(s.Holds, id)
		}
	}
}
func (s *State) restoreReferencedHold(a Action) {
	if s.Version < 11 {
		return
	}
	id := a.HoldID
	if a.Type == "privacy" && a.Privacy != nil {
		id = a.Privacy.RequestID
	}
	if s.Holds[id] == nil {
		if h := s.LookupHold(id); h != nil {
			s.Holds[id] = copyMutable(reflect.ValueOf(h)).Interface().(*Hold)
		}
	}
}
