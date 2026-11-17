package chain

import (
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

// Opt-outs belong to the TTL-bound registration, never the signed engine profile.
func (s *State) ExecutionAllowed(id string, height int64) bool {
	p := s.ActiveProfile(id, height)
	if p == nil {
		return false
	}
	if s.Version >= 10 {
		reg := s.Nodes[id]
		return !reg.NoExec && !(s.Version >= 13 && reg.PendingNoExec && height >= reg.ExecExitEffectiveHeight)
	}
	return legacyExecRole(p)
}
func legacyExecRole(p *pb.ResourceProfile) bool {
	return HasRole(p, "model") || HasRole(p, "executor") || HasRole(p, "shard-stage") || HasRole(p, "moe-attention") || HasRole(p, "moe-expert")
}
func (s *State) ReviewAllowed(id string, height int64) bool {
	p := s.ActiveProfile(id, height)
	if p == nil || p.EngineCapabilities == nil {
		return false
	}
	if s.Version >= 10 {
		return !s.Nodes[id].NoAudit
	}
	return HasRole(p, "auditor")
}

// Read each CURRENT record exactly once; do not rewrite signed bytes or history.
func (s *State) migrateRegistrationRoles() {
	for _, r := range s.Nodes {
		p := &pb.ResourceProfile{}
		if proto.Unmarshal(r.Profile, p) != nil {
			r.NoExec, r.NoAudit = true, true
			continue
		}
		r.NoExec, r.NoAudit = !legacyExecRole(p), !HasRole(p, "auditor")
	}
	s.Version = 10
}
