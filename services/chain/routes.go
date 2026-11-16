package chain

import (
	"bytes"
	core "ember/core-go"
	pb "ember/proto"
	"fmt"
	"google.golang.org/protobuf/proto"
)

// ActiveProfile reads only consensus state. Node-signed on-chain presence may
// determine auditor eligibility; wall clocks and Tracker/tunnel reachability
// remain outside consensus and settlement coverage.
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
	if p == nil || r == nil || id == producer || !HasRole(r, "auditor") || !bytes.Equal(r.SchemeHash, p.SchemeHash) {
		return false
	}
	c := r.EngineCapabilities
	if s.Parameters.AuditEquality.Enabled {
		if s.Version < 7 {
			return r.ChainId == p.ChainId && bytes.Equal(r.ManifestRoot, p.ManifestRoot) && c.ForcedPrefixReplay && c.MaxTopK >= 16 && contains(c.EvidenceLevels, "full")
		}
		return eligibleAuditorForClass(p, r)
	}
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

// eligibleAuditorForClass judges the auditor's capabilities relative to the
// executor profile's evidence class (AppVersion 7+). The verdict path is
// unchanged: it consumes a replayed token stream plus top-k logprobs margins
// at the first divergence, so an auditor qualifies exactly when it can
// produce that evidence for the executor's manifest.
//
//   - Full-evidence class: the executor's registration declares "full"
//     evidence. Registration validation pins this class to transformers
//     execution profiles with raw-logits scoring and forced prefix replay
//     (the 1.5B family); the auditor bar is the v6 rule, unchanged.
//   - Logprobs class: the executor runs an HTTP engine (for example vLLM)
//     declaring score_kind "logprobs" with "topk-logprobs" evidence. Such
//     engines cannot produce full evidence or forced prefix replay, so the
//     auditor bar is same-class logprobs evidence at top_k >= 16. A
//     full-evidence auditor is a strict superset and also qualifies.
//   - Anything else (missing capabilities, shard-only, mock) fails closed.
func eligibleAuditorForClass(p, r *pb.ResourceProfile) bool {
	if r.ChainId != p.ChainId || !bytes.Equal(r.ManifestRoot, p.ManifestRoot) {
		return false
	}
	pc, ac := p.EngineCapabilities, r.EngineCapabilities
	if pc == nil || ac == nil {
		return false
	}
	if contains(pc.EvidenceLevels, "full") {
		return ac.ForcedPrefixReplay && ac.MaxTopK >= 16 && contains(ac.EvidenceLevels, "full")
	}
	if pc.ScoreKind == "logprobs" && contains(pc.EvidenceLevels, "topk-logprobs") {
		if ac.MaxTopK < 16 {
			return false
		}
		return contains(ac.EvidenceLevels, "full") || (ac.ScoreKind == "logprobs" && contains(ac.EvidenceLevels, "topk-logprobs"))
	}
	return false
}
