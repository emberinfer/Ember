package chain

// migrateMoeEnablement applies only the parameters carried by the manifest.
func (s *State) migrateMoeEnablement(u MembershipUpgrade) {
	s.Version = 9
	if u.MoeEnabled != nil {
		s.Parameters.MoeEnabled = *u.MoeEnabled
	}
	if u.AuditSampling != nil {
		s.Parameters.AuditSampling = *u.AuditSampling
	}
}
