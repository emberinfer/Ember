package chain

import (
	"errors"
	"sort"
)

func (s *State) subsidyA(signer string, a Action) error {
	p := s.GenesisSubsidy
	if p == nil {
		return errors.New("genesis subsidy unavailable")
	}
	if a.Type == "subsidy_notice" {
		if a.Notice != SubsidyNotice {
			return errors.New("consumption notice acknowledgement required")
		}
		p.Notices[signer] = true
		return nil
	}
	if p.Retrospective != nil && p.Retrospective.Allocations[signer] > SubsidyHouseholdEquivalents*SubsidyEquivalentSubunits {
		return errors.New("higher retrospective entitlement must be selected")
	}
	if !p.Notices[signer] || p.Grants[signer] != nil {
		return errors.New("notice missing or lifetime claim already used")
	}
	amount := SubsidyHouseholdEquivalents * SubsidyEquivalentSubunits
	if err := s.issueSubsidy("A", signer, amount, "new-user", false); err != nil {
		return err
	}
	p.Grants[signer] = &SubsidyGrant{Channel: "A", Amount: amount, ClaimedAt: s.BlockTime, Deadline: s.BlockTime + SubsidyActivationSeconds}
	return nil
}

func (s *State) issueSubsidy(ch, id string, amount int64, batch string, active bool) error {
	p := s.GenesisSubsidy
	c := p.Channels[ch]
	if c == nil || amount <= 0 || c.Balance < amount || id == p.Account || !p.Notices[id] {
		return errors.New("invalid subsidy issue or missing notice")
	}
	if err := s.meterSubsidy(ch, amount); err != nil {
		return err
	}
	if s.Accounts[id] == nil {
		s.Accounts[id] = &Account{}
	}
	if active {
		if err := add(&s.Accounts[id].ConsumptionCredit, amount); err != nil {
			return err
		}
	}
	c.Balance -= amount
	c.Issued += amount
	s.Accounts[p.Account].Balance -= amount
	p.Events = append(p.Events, SubsidyEvent{Channel: ch, Account: id, Amount: amount, Batch: batch, Time: s.BlockTime})
	return nil
}

func (s *State) activateSubsidy(h *Hold) {
	p := s.GenesisSubsidy
	if p == nil || h.State != "settled" || h.Actual <= 0 || h.AuditInputTokens < SubsidyActivationInput || h.TokenFee == nil || len(h.TokenFee.Commitment) == 0 {
		return
	}
	for id, g := range p.Grants {
		recipient := id
		if g.Account != "" {
			recipient = g.Account
		}
		if recipient != h.Payer || g.Activated || g.Reclaimed || s.BlockTime >= g.Deadline {
			continue
		}
		s.Accounts[h.Payer].ConsumptionCredit += g.Amount
		g.Activated = true
	}
}

// Runs for every consensus block, including empty blocks, never local wall time.
func (s *State) advanceSubsidyTime(now int64) error {
	if s.GenesisSubsidy == nil {
		return nil
	}
	if now <= 0 || now < s.BlockTime {
		return errors.New("invalid subsidy consensus time")
	}
	s.BlockTime = now
	p := s.GenesisSubsidy
	ids := make([]string, 0, len(p.Grants))
	for id := range p.Grants {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		g := p.Grants[id]
		if g.Activated || g.Reclaimed || now < g.Deadline {
			continue
		}
		c := p.Channels[g.Channel]
		c.Balance += g.Amount
		c.Recovered += g.Amount
		s.Accounts[p.Account].Balance += g.Amount
		g.Reclaimed = true
		p.Events = append(p.Events, SubsidyEvent{Channel: g.Channel, Account: grantRecipient(id, g), Amount: g.Amount, Batch: "activation-expired", Time: now, Recovery: true})
	}
	s.recoverDeveloperGrants(now)
	return nil
}

func grantRecipient(id string, g *SubsidyGrant) string {
	if g.Account != "" {
		return g.Account
	}
	return id
}
