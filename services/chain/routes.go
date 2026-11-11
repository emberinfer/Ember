package chain

import (
	"bytes"
	core "ember/core-go"
	pb "ember/proto"
	"fmt"
	"google.golang.org/protobuf/proto"
)

// ActiveProfile reads only consensus state. Wall clocks and network reachability
// never participate in auditor eligibility or settlement coverage.
func (s *State) ActiveProfile(id string, height int64) *pb.ResourceProfile {
	r, a := s.Nodes[id], s.Accounts[id]
	if r == nil || a == nil || a.Stake < s.Parameters.MinNodeStake || r.ExpiresHeight <= height {
		return nil
	}
	p := &pb.ResourceProfile{}
	if proto.Unmarshal(r.Profile, p) != nil || core.Verify(p) != nil || p.NodeId != id || core.NodeID(p.Signer) != id {
		return nil
	}
	return p
}

func HasRole(p *pb.ResourceProfile, role string) bool {
	if p == nil || p.EngineCapabilities == nil {
		return false
	}
	for _, r := range p.EngineCapabilities.Roles {
		if r == role {
			return true
		}
	}
	return false
}

func RuntimeID(p *pb.ResourceProfile) string {
	c := p.EngineCapabilities
	if c == nil {
		return ""
	}
	return fmt.Sprintf("%s/%s|torch=%s", c.EngineName, c.EngineVersion, p.TorchVersion)
}

func (s *State) EligibleAuditor(id, producer string, height int64) bool {
	return s.EligibleAuditorProfile(id, producer, s.ActiveProfile(producer, height), height)
}

func (s *State) EligibleAuditorProfile(id, producer string, p *pb.ResourceProfile, height int64) bool {
	r := s.ActiveProfile(id, height)
	if p == nil || r == nil || id == producer || !HasRole(r, "auditor") {
		return false
	}
	c := r.EngineCapabilities
	return r.ChainId == p.ChainId && r.ExecutionProfileVer == p.ExecutionProfileVer &&
		bytes.Equal(r.ManifestRoot, p.ManifestRoot) && c.ForcedPrefixReplay && c.ScoreKind == "raw-logits" &&
		c.MaxTopK >= 16 && contains(c.EvidenceLevels, "full") && RuntimeID(p) == RuntimeID(r) && p.ActualDevice == r.ActualDevice
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
