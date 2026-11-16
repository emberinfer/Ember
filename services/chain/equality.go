package chain

type AuditEqualityParameters struct {
	Enabled   bool    `json:"enabled"`
	Epsilon   float64 `json:"epsilon"`
	MaxRounds int     `json:"max_rounds"`
}

func (p *AuditEqualityParameters) defaults() {
	if !p.Enabled {
		return
	}
	if p.Epsilon == 0 {
		p.Epsilon = 1.0
	}
	if p.MaxRounds == 0 {
		p.MaxRounds = 3
	}
}
func (p AuditEqualityParameters) valid() bool {
	return !p.Enabled || (finite(p.Epsilon) && p.Epsilon > 0 && p.MaxRounds >= 2 && p.MaxRounds <= 4)
}

// migrateAuditEligibility is the 6 -> 7 activation step. It deliberately
// changes no parameters, holds, registrations or balances: the only state
// transition is Version 6 -> 7. From that height, EligibleAuditorProfile
// judges auditor eligibility relative to the executor profile's evidence
// class (see routes.go); before it, the v6 rule applies unchanged. The
// height is accepted for symmetry with migrateLiveness; nothing is
// stamped with it.
func (s *State) migrateAuditEligibility(height int64) {
	_ = height
	s.Version = 7
}
