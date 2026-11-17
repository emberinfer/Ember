package chain

import "errors"

// TrustPolicy is local consumer configuration. Endorsements never activate a
// model in consensus. Any one of these explicit conditions may authorize serving.
type ProfileTrust struct {
	Publishers       []string `json:"publishers,omitempty"`
	MinEndorsers     int      `json:"min_endorsers,omitempty"`
	TrustedEndorsers []string `json:"trusted_endorsers,omitempty"`
}

type TrustPolicy struct {
	Profiles map[string]ProfileTrust `json:"profiles"`
}

func (s *State) CanServeModel(id string, policy *TrustPolicy) error {
	m, ok := s.Models[id]
	if !ok {
		return errors.New("model is not registered")
	}
	if m.Publication != nil && m.Publication.Invalid {
		return errors.New("publication is proven invalid")
	}
	var p ProfileTrust
	custom := false
	if policy != nil {
		p, custom = policy.Profiles[id]
	}
	if p.MinEndorsers < 0 {
		return errors.New("invalid endorsement threshold")
	}
	if m.Publication == nil && !custom {
		return nil
	} // existing genesis anchors
	if !custom {
		for account, enabled := range s.Governors {
			if enabled {
				p.TrustedEndorsers = append(p.TrustedEndorsers, account)
			}
		}
	}
	if m.Publication == nil {
		return errors.New("historical profile has no publisher or endorsements for custom policy")
	}
	for _, account := range p.Publishers {
		if account == m.Publication.Publisher {
			return nil
		}
	}
	count := 0
	for account, e := range s.Endorsements[m.Publication.ManifestRoot] {
		if e.Revoked {
			continue
		}
		count++
		for _, trusted := range p.TrustedEndorsers {
			if account == trusted {
				return nil
			}
		}
	}
	if p.MinEndorsers > 0 && count >= p.MinEndorsers {
		return nil
	}
	return errors.New("profile does not satisfy local publisher/endorsement policy")
}
