package chain

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"

	abci "github.com/cometbft/cometbft/abci/types"
	cmted "github.com/cometbft/cometbft/crypto/ed25519"
)

// MembershipInit assigns operators to the consensus keys observed at activation.
// It is a manifest section, never persisted in pre-activation state.
type MembershipInit struct {
	ChangeDelayBlocks  int64             `json:"change_delay_blocks"`
	ChangeWindowBlocks int64             `json:"change_window_blocks"`
	ReadinessBlocks    int64             `json:"readiness_blocks"`
	MaxValidators      int               `json:"max_validators"`
	Operators          map[string]string `json:"operators"`
	OrgIDs             map[string]string `json:"org_ids,omitempty"`
}

func (init *MembershipInit) membership(s *State) (*Membership, error) {
	m := &Membership{ChangeDelayBlocks: init.ChangeDelayBlocks, ChangeWindowBlocks: init.ChangeWindowBlocks, ReadinessBlocks: init.ReadinessBlocks, MaxValidators: init.MaxValidators, Registry: map[string]*Validator{}}
	for id, operator := range init.Operators {
		pub, err := hex.DecodeString(id)
		if err != nil || len(pub) != ed25519.PublicKeySize || id != validatorID(pub) {
			return nil, errors.New("invalid membership_init pubkey (expected canonical lowercase ed25519 hex)")
		}
		org := init.OrgIDs[id]
		if org == "" {
			org = "validator-" + id
		}
		m.Registry[id] = &Validator{PubKey: pub, Operator: operator, OrgID: org, Power: EqualValidatorPower, Status: "active"}
	}
	if err := m.validate(s); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *State) migrateMembershipInit(init *MembershipInit, req *abci.FinalizeBlockRequest) error {
	if init == nil {
		s.Version = 11
		return nil
	}
	m, err := init.membership(s)
	if err != nil {
		return err
	}
	if len(req.DecidedLastCommit.Votes) != len(m.Registry) {
		return errors.New("membership_init differs from live consensus set")
	}
	seen := map[string]bool{}
	for _, vote := range req.DecidedLastCommit.Votes {
		matches := 0
		for id, v := range m.Registry {
			if bytes.Equal(cmted.PubKey(v.PubKey).Address(), vote.Validator.Address) {
				if seen[id] || vote.Validator.Power != EqualValidatorPower {
					return errors.New("membership_init differs from live consensus set")
				}
				matches++
				seen[id] = true
			}
		}
		if matches != 1 {
			return errors.New("membership_init differs from live consensus set")
		}
	}
	s.Membership = m
	s.Version = 11
	return nil
}
