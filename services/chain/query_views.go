package chain

import (
	pb "ember/proto"
	"errors"
	"google.golang.org/protobuf/proto"
)

type AuditRouteView struct {
	Hold     *Hold               `json:"hold"`
	Producer *pb.ResourceProfile `json:"producer"`
	Auditor  *pb.ResourceProfile `json:"auditor"`
	Eligible bool                `json:"eligible"`
}

func (s *State) QueryAuditRoute(id string) (*AuditRouteView, error) {
	h := s.LookupHold(id)
	if h == nil {
		return nil, errors.New("not found")
	}
	producer := s.ActiveProfile(h.Node, s.Height)
	if len(h.ProducerProfile) > 0 {
		producer = &pb.ResourceProfile{}
		if proto.Unmarshal(h.ProducerProfile, producer) != nil {
			return nil, errors.New("invalid frozen profile")
		}
	}
	drawn := h.Pipeline != nil && len(h.Assignments) > 0 && h.Assignments[len(h.Assignments)-1].DrawnStage != nil
	if drawn {
		d := h.Assignments[len(h.Assignments)-1].DrawnStage
		if int(d.Index) >= len(h.Pipeline.Authorizations) {
			return nil, errors.New("invalid drawn stage")
		}
		producer = &pb.ResourceProfile{}
		if proto.Unmarshal(h.Pipeline.Authorizations[d.Index].Profile, producer) != nil {
			return nil, errors.New("invalid frozen stage profile")
		}
	}
	// Non-range pipeline assignments select against the first frozen stage,
	// not Hold.Node (which is empty for pipeline holds). Match that lookup
	// for spot challenges without weakening the registered replay check.
	spot := h.Pipeline != nil && h.Pipeline.Spot != nil
	if !drawn && spot {
		if len(h.Pipeline.Authorizations) == 0 {
			return nil, errors.New("missing frozen stages")
		}
		auth := h.Pipeline.Authorizations[0]
		producer = s.ActiveProfile(auth.NodeID, s.Height)
		if len(auth.Profile) > 0 {
			producer = &pb.ResourceProfile{}
			if proto.Unmarshal(auth.Profile, producer) != nil {
				return nil, errors.New("invalid frozen stage profile")
			}
		}
	}
	eligible := producer != nil && s.EligibleAuditorProfile(h.Auditor, producer.NodeId, producer, s.Height)
	// Spot replay spans shard stages even when no legacy range was drawn.
	// Use the existing shard/full-model runtime compatibility check.
	spotShard := spot && producer != nil && producer.EngineCapabilities != nil && producer.EngineCapabilities.EngineName == "transformers-shard"
	if drawn || spotShard {
		eligible = producer != nil && s.EligibleRangeAuditor(h.Auditor, producer.NodeId, producer, s.Height)
	}
	return &AuditRouteView{h, producer, s.ActiveProfile(h.Auditor, s.Height), eligible}, nil
}

type NodeStatusView struct {
	ExecExitPending         bool              `json:"exec_exit_pending,omitempty"`
	ExecExitEffectiveHeight int64             `json:"exec_exit_effective_height,omitempty"`
	ExecExited              bool              `json:"exec_exited,omitempty"`
	Registration            *NodeRegistration `json:"registration"`
	Height                  int64             `json:"height"`
	Active                  bool              `json:"active"`
	ExecutionAllowed        bool              `json:"execution_allowed"`
	ContributionAvailable   bool              `json:"contribution_available"`
}

func (s *State) QueryNodeStatus(id string) *NodeStatusView {
	r := s.Nodes[id]
	if r == nil {
		return nil
	}
	p := &pb.ResourceProfile{}
	_ = proto.Unmarshal(r.Profile, p)
	pending := s.Version >= 13 && r.PendingNoExec
	effective := int64(0)
	if pending {
		effective = r.ExecExitEffectiveHeight
	}
	return &NodeStatusView{pending && s.Height < effective, effective, pending && s.Height >= effective, r, s.Height, s.ActiveProfile(id, s.Height) != nil, s.Version < 10 || s.ExecutionAllowed(id, s.Height), s.ContributionAvailable(id, p.ChainId, s.Height+1, 1)}
}

// ExpiredHolds never includes terminal or archived history.
func (s *State) ExpiredHolds() map[string]*Hold {
	out := map[string]*Hold{}
	for id, h := range s.Holds {
		if (h.State == "frozen" || h.State == "dispute") && h.ExpireHeight <= s.Height {
			out[id] = h
		}
	}
	return out
}
