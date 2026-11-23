package chain

import (
	pb "ember/proto"
	"errors"
	"google.golang.org/protobuf/proto"
)

// Profiles is the offered set of model chain IDs. A missing key refuses service;
// zero means unlimited. A nil caps record preserves legacy unlimited service.
type ContributionCaps struct {
	Profiles map[string]uint32 `json:"profiles"`
	Global   uint32            `json:"global,omitempty"`
}

func validateCaps(c *ContributionCaps) error {
	if c == nil {
		return nil
	}
	if c.Profiles == nil || len(c.Profiles) > 1024 {
		return errors.New("caps require an explicit profile set (empty is allowed)")
	}
	for id := range c.Profiles {
		if id == "" {
			return errors.New("empty contribution profile")
		}
	}
	return nil
}
func (r *NodeRegistration) CapsAt(height int64) *ContributionCaps {
	if r.PendingCaps != nil && height >= r.CapsEffectiveHeight {
		return r.PendingCaps
	}
	return r.Caps
}
func (s *State) updateContributionCaps(id string, c *ContributionCaps, height int64) error {
	r := s.Nodes[id]
	if r == nil || r.ExpiresHeight <= height || c == nil {
		return errors.New("caps update requires active owner registration and explicit caps")
	}
	if err := validateCaps(c); err != nil {
		return err
	}
	if r.PendingCaps != nil && height < r.CapsEffectiveHeight {
		return errors.New("caps update already pending")
	}
	copyCaps := &ContributionCaps{Profiles: map[string]uint32{}, Global: c.Global}
	for k, v := range c.Profiles {
		copyCaps.Profiles[k] = v
	}
	r.Caps = r.CapsAt(height)
	r.PendingCaps = copyCaps
	r.CapsEffectiveHeight = height + 1
	return nil
}

// InFlight counts execution reservations until receipt submission or timeout,
// and unreplayed auditor assignments. Existing reservations survive cap changes.
func (s *State) InFlight(id, profile string, height int64) (uint32, uint32) {
	var per, total uint32
	for _, h := range s.Holds {
		count := uint32(0)
		model := h.ContributionProfile
		if model == "" {
			model = h.ModelID
		}
		if h.State == "frozen" && height < h.ExpireHeight {
			if h.Pipeline != nil {
				for _, a := range h.Pipeline.Authorizations {
					if a.NodeID == id {
						count++
					}
				}
			} else if h.Node == id {
				count++
			}
		}
		if h.State == "dispute" && h.AuditVerdict == "" && h.Auditor == id {
			count++
		}
		if model == "" {
			p := &pb.ResourceProfile{}
			if proto.Unmarshal(h.ProducerProfile, p) == nil {
				model = p.ChainId
			}
			if model == "" {
				if r := s.Nodes[h.Node]; r != nil && proto.Unmarshal(r.Profile, p) == nil {
					model = p.ChainId
				}
			}
		}
		total += count
		if model == profile {
			per += count
		}
	}
	return per, total
}
func (s *State) ContributionAvailable(id, profile string, height int64, additional uint32) bool {
	r := s.Nodes[id]
	if r == nil {
		return true
	} // legacy auditors have no registration
	c := r.CapsAt(height)
	per, total := s.InFlight(id, profile, height)
	fits := func(limit, used uint32) bool { return limit == 0 || uint64(used)+uint64(additional) <= uint64(limit) }
	if c != nil {
		limit, ok := c.Profiles[profile]
		if !ok || !fits(limit, per) || !fits(c.Global, total) {
			return false
		}
	}
	p := &pb.ResourceProfile{}
	if proto.Unmarshal(r.Profile, p) == nil && p.Contribution != nil {
		q := p.Contribution
		if q.Unavailable || !fits(q.MaxConcurrent, per) || !fits(q.GlobalConcurrent, total) {
			return false
		}
	}
	return true
}
func (s *State) checkContributionAction(a Action, height int64) error {
	counts := map[string]uint32{}
	switch a.Type {
	case "freeze":
		counts[a.Node] = 1
	case "image_request":
		if s.Version >= 16 {
			counts[a.Node] = 1
		}
	case "freeze_pipeline":
		for _, auth := range a.PipelineAuthorizations {
			counts[auth.NodeID]++
		}
	}
	for id, n := range counts {
		p := s.ActiveProfile(id, height)
		if p != nil && !s.ContributionAvailable(id, p.ChainId, height, n) {
			return errors.New("contribution unavailable: cap or qualification; no work accepted")
		}
	}
	return nil
}

// CheckContributionAdmission lets schedulers recheck the same consensus rule
// after a transaction was removed during proposal preparation or mempool recheck.
func (s *State) CheckContributionAdmission(a Action, height int64) error {
	if s.Holds[a.HoldID] != nil {
		return nil
	}
	return s.checkContributionAction(a, height)
}
