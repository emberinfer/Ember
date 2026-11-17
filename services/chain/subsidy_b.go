package chain

import (
	"errors"
	"sort"
)

// Immutable, publicly reproducible genesis snapshot. Entities identify registered
// testnet operators; missing identities fail closed rather than assuming distinct.
type TestnetContribution struct {
	RequestID      string `json:"request_id"`
	RecordRef      string `json:"record_ref"`
	Payer          string `json:"payer"`
	Executor       string `json:"executor"`
	Auditor        string `json:"auditor,omitempty"`
	Settled        bool   `json:"settled"`
	AuditCompleted bool   `json:"audit_completed"`
}
type SubsidyRetrospective struct {
	Records     []TestnetContribution `json:"records"`
	Entities    map[string]string     `json:"entities"`
	Excluded    []string              `json:"excluded_self_transactions"`
	Scores      map[string]int64      `json:"scores"`
	Capped      map[string]int64      `json:"capped_subunits"`
	Allocations map[string]int64      `json:"allocations"`
	Claimed     map[string]bool       `json:"claimed"`
	Unallocated int64                 `json:"unallocated_subunits"`
	Scaled      bool                  `json:"scaled"`
}

func (s *State) scoreRetrospective(g *GenesisSubsidy) error {
	p := s.GenesisSubsidy
	r := &SubsidyRetrospective{Records: append([]TestnetContribution(nil), g.TestnetRecords...), Entities: g.Entities, Scores: map[string]int64{}, Capped: map[string]int64{}, Allocations: map[string]int64{}, Claimed: map[string]bool{}}
	sort.Slice(r.Records, func(i, j int) bool { return r.Records[i].RequestID < r.Records[j].RequestID })
	seen := map[string]bool{}
	for _, v := range r.Records {
		if v.RequestID == "" || v.RecordRef == "" || seen[v.RequestID] || g.Entities[v.Payer] == "" || g.Entities[v.Executor] == "" || (v.AuditCompleted && g.Entities[v.Auditor] == "") {
			return errors.New("invalid retrospective record or registered entity")
		}
		seen[v.RequestID] = true
		self := g.Entities[v.Payer] == g.Entities[v.Executor]
		if self {
			r.Excluded = append(r.Excluded, v.RequestID)
		}
		if v.Settled && !self {
			a, b := r.Scores[v.Executor], r.Scores[v.Payer]
			if err := add(&a, SubsidySupplyWeight); err != nil {
				return err
			}
			if err := add(&b, SubsidyConsumeWeight); err != nil {
				return err
			}
			r.Scores[v.Executor], r.Scores[v.Payer] = a, b
		}
		if v.AuditCompleted {
			a := r.Scores[v.Auditor]
			if err := add(&a, SubsidyAuditWeight); err != nil {
				return err
			}
			r.Scores[v.Auditor] = a
		}
	}
	total := int64(0)
	cap := SubsidyHouseholdEquivalents * SubsidyBCapMultiplier * SubsidyEquivalentSubunits
	for id, score := range r.Scores {
		amount, err := multiply(score, SubsidyEquivalentSubunits)
		if err != nil {
			return err
		}
		if amount > cap {
			amount = cap
		}
		r.Capped[id] = amount
		if err := add(&total, amount); err != nil {
			return err
		}
	}
	budget := p.Channels["B"].Initial
	r.Scaled = total > budget
	r.Unallocated = budget
	for id, amount := range r.Capped {
		if r.Scaled {
			amount = ratio(amount, budget, total)
		}
		r.Allocations[id] = amount
		r.Unallocated -= amount
	}
	p.Retrospective = r
	return nil
}
func (s *State) claimRetrospective(id string) error {
	p := s.GenesisSubsidy
	if p == nil || p.Retrospective == nil {
		return errors.New("retrospective unavailable")
	}
	r := p.Retrospective
	amount := r.Allocations[id]
	if !p.Notices[id] || r.Claimed[id] || p.Grants[id] != nil || amount <= 0 {
		return errors.New("invalid retrospective claim")
	}
	// A and B never stack; take the higher entitlement. If B is smaller than A,
	// the account selects the new-user grant, including its activation condition.
	if amount < SubsidyHouseholdEquivalents*SubsidyEquivalentSubunits {
		return errors.New("higher new-user entitlement must be selected")
	}
	if err := s.issueSubsidy("B", id, amount, "testnet-retrospective", true); err != nil {
		return err
	}
	r.Claimed[id] = true
	p.Grants[id] = &SubsidyGrant{Channel: "B", Amount: amount, ClaimedAt: s.BlockTime, Activated: true}
	return nil
}
