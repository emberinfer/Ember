package chain

import (
	"errors"
	"sort"
	"time"
)

type DeveloperGrant struct {
	Funded           int64           `json:"funded"`
	ID               string          `json:"id"`
	Developer        string          `json:"developer"`
	Amount           int64           `json:"amount"`
	Remaining        int64           `json:"remaining"`
	Published        int64           `json:"published"`
	Expires          int64           `json:"expires"`
	Approvals        map[string]bool `json:"approvals"`
	Revocations      map[string]bool `json:"revocations"`
	Issued           bool            `json:"issued"`
	Closed           bool            `json:"closed"`
	RevocationReason string          `json:"revocation_reason,omitempty"`
}

func (s *State) meterSubsidy(ch string, amount int64) error {
	p := s.GenesisSubsidy
	if s.BlockTime <= 0 {
		return errors.New("consensus time not initialized")
	}
	day := s.BlockTime / SubsidyDaySeconds
	d := p.Daily[day]
	if d == nil {
		d = map[string]int64{}
	}
	bps := map[string]int64{"A": SubsidyADailyBPS, "B": SubsidyBDailyBPS, "C": SubsidyCDailyBPS}[ch]
	total := d["A"] + d["B"] + d["C"]
	if bps == 0 || amount <= 0 || amount > ratio(p.Total, SubsidyGlobalDailyBPS, 10000)-total || amount > ratio(p.Total, bps, 10000)-d[ch] {
		return errors.New("subsidy daily channel or global limit")
	}
	d[ch] += amount
	p.Daily[day] = d
	return nil
}
func (s *State) subsidyC(actor string, a *SubsidyAction) error {
	p := s.GenesisSubsidy
	if p == nil || a == nil || a.ID == "" {
		return errors.New("invalid developer subsidy action")
	}
	if a.Op == "propose" {
		if len(s.Governors) != SubsidyGovernors || !s.Governors[actor] || p.Developers[a.ID] != nil || a.To == "" || a.To == p.Account || a.Amount <= 0 || a.Amount > ratio(p.Total, SubsidyCGrantBPS, 10000) || p.DeveloperBanned[a.To] {
			return errors.New("invalid C proposal or per-grant cap")
		}
		p.Developers[a.ID] = &DeveloperGrant{ID: a.ID, Developer: a.To, Amount: a.Amount, Published: s.BlockTime, Approvals: map[string]bool{}, Revocations: map[string]bool{}}
		return nil
	}
	g := p.Developers[a.ID]
	if g == nil {
		return errors.New("unknown developer grant")
	}
	switch a.Op {
	case "approve":
		if len(s.Governors) != SubsidyGovernors || !s.Governors[actor] || g.Issued || g.Closed || g.Approvals[actor] {
			return errors.New("invalid or duplicate C approval")
		}
		g.Approvals[actor] = true
	case "issue":
		if g.Funded >= g.Amount || g.Closed || p.DeveloperBanned[g.Developer] || len(g.Approvals) < SubsidyCApprovals || s.BlockTime < g.Published+SubsidyActivationSeconds {
			return errors.New("C needs 4-of-5 and full publication period")
		}
		tranche := a.Amount
		if tranche == 0 {
			tranche = g.Amount - g.Funded
		}
		if tranche <= 0 || tranche > g.Amount-g.Funded {
			return errors.New("invalid developer grant tranche")
		}
		if err := s.issueSubsidy("C", g.Developer, tranche, g.ID, false); err != nil {
			return err
		}
		first := !g.Issued
		g.Issued = true
		g.Funded += tranche
		g.Remaining += tranche
		// Calendar quarter reconciliation; the published deadline is unambiguous UTC.
		now := time.Unix(s.BlockTime, 0).UTC()
		month := ((int(now.Month())-1)/3+1)*3 + 1
		if first {
			g.Expires = time.Date(now.Year(), time.Month(month), 1, 0, 0, 0, 0, time.UTC).Unix()
		}
	case "distribute":
		key := "C/" + g.ID + "/" + a.To
		if actor != g.Developer || !g.Issued || g.Closed || p.DeveloperBanned[actor] || a.To == actor || a.To == "" || a.To == p.Account || !p.Notices[a.To] || p.Grants[key] != nil || a.Amount <= 0 || a.Amount > g.Remaining {
			return errors.New("invalid terminal-user distribution")
		}
		if s.Accounts[a.To] == nil {
			s.Accounts[a.To] = &Account{}
		}
		g.Remaining -= a.Amount
		p.Grants[key] = &SubsidyGrant{Account: a.To, Channel: "C", Amount: a.Amount, ClaimedAt: s.BlockTime, Deadline: s.BlockTime + SubsidyActivationSeconds}
	case "revoke":
		if len(s.Governors) != SubsidyGovernors || !s.Governors[actor] || a.Reason == "" || g.Revocations[actor] {
			return errors.New("invalid developer revocation")
		}
		if g.RevocationReason != "" && g.RevocationReason != a.Reason {
			return errors.New("revocation reasons differ")
		}
		g.RevocationReason = a.Reason
		g.Revocations[actor] = true
		if len(g.Revocations) >= SubsidyCApprovals {
			p.DeveloperBanned[g.Developer] = true
			s.recoverBannedDeveloper(g.Developer)
		}
	default:
		return errors.New("unknown C operation")
	}
	return nil
}
func (s *State) recoverDeveloper(g *DeveloperGrant, batch string) {
	if g.Closed {
		return
	}
	g.Closed = true
	if g.Remaining == 0 {
		return
	}
	p := s.GenesisSubsidy
	c := p.Channels["C"]
	c.Balance += g.Remaining
	c.Recovered += g.Remaining
	s.Accounts[p.Account].Balance += g.Remaining
	p.Events = append(p.Events, SubsidyEvent{Channel: "C", Account: g.Developer, Batch: batch + "/" + g.ID, Amount: g.Remaining, Time: s.BlockTime, Recovery: true})
	g.Remaining = 0
}
func (s *State) recoverDeveloperGrants(now int64) {
	p := s.GenesisSubsidy
	ids := make([]string, 0, len(p.Developers))
	for id := range p.Developers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		g := p.Developers[id]
		if g.Issued && !g.Closed && now >= g.Expires {
			s.recoverDeveloper(g, "quarterly-reconciliation")
		}
	}
}

func (s *State) recoverBannedDeveloper(developer string) {
	p := s.GenesisSubsidy
	ids := make([]string, 0, len(p.Developers))
	for id := range p.Developers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		g := p.Developers[id]
		if g.Developer == developer {
			s.recoverDeveloper(g, "developer-revoked")
		}
	}
}
