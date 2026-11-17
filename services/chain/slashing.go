package chain

import (
	"errors"
	"math/big"
)

type SlashRecord struct {
	NodeID      string `json:"node_id"`
	Height      int64  `json:"height"`
	Revenue     int64  `json:"revenue"`
	StakeBefore int64  `json:"stake_before"`
	StakeFloor  int64  `json:"stake_floor"`
	Multiplier  uint32 `json:"multiplier"`
	Amount      int64  `json:"amount"`
}

func (s *State) slashOffender(hold *Hold, node string, revenue, height int64) error {
	amount, err := s.slashAmount(hold, node, revenue, height)
	if err != nil {
		return err
	}
	return s.creditBalance(hold.Payer, amount)
}

// slashAmount applies the confirmed-cheating penalty to the node's stake
// and records it on the hold WITHOUT crediting anyone. Callers decide where
// the proceeds go: Plan A credits the payer (slashOffender); MoE Plan B
// routes them through the honest-payment waterfall first.
func (s *State) slashAmount(hold *Hold, node string, revenue, height int64) (int64, error) {
	account := s.Accounts[node]
	if account == nil || account.Stake < 0 || revenue < 0 || height < 0 {
		return 0, errors.New("invalid confirmed-cheating slash")
	}
	merged := false
	prior := make([]int64, 0, 4)
	for _, previous := range s.Offenses[node] {
		if previous == height {
			merged = true
		}
		if previous > height {
			return 0, errors.New("confirmed offense height regression")
		}
		if previous != height && height-previous < s.Parameters.SlashRepeatWindow {
			prior = append(prior, previous)
		}
	}
	multiplier := uint32(1) << uint(min(len(prior), 3))
	if s.BlockSlashes == nil {
		s.BlockSlashes = map[string]*BlockSlash{}
	}
	block := s.BlockSlashes[node]
	if block != nil && block.Height > height {
		return 0, errors.New("block slash height regression")
	}
	if block == nil || block.Height != height {
		limit := new(big.Int).Mul(big.NewInt(account.Stake), new(big.Int).SetUint64(uint64(s.Parameters.Misjudge.CapBlockBPS)))
		limit.Quo(limit, big.NewInt(10000))
		protected := new(big.Int).Mul(big.NewInt(account.Stake), new(big.Int).SetUint64(uint64(s.Parameters.SlashFloorBPS)))
		protected.Add(protected, big.NewInt(9999)).Quo(protected, big.NewInt(10000))
		if limit.Cmp(protected) < 0 {
			limit.Set(protected)
		}
		block = &BlockSlash{Height: height, StakeStart: account.Stake, Limit: limit.Int64(), Multiplier: multiplier}
		s.BlockSlashes[node] = block
	}
	multiplier = block.Multiplier
	stake := big.NewInt(account.Stake)
	floor := new(big.Int).Mul(stake, new(big.Int).SetUint64(uint64(s.Parameters.SlashFloorBPS)))
	floor.Add(floor, big.NewInt(9999)).Quo(floor, big.NewInt(10000))
	penalty := new(big.Int).Mul(big.NewInt(revenue), big.NewInt(s.Parameters.SlashMultiplier))
	if penalty.Cmp(floor) < 0 {
		penalty.Set(floor)
	}
	penalty.Mul(penalty, new(big.Int).SetUint64(uint64(multiplier)))
	if penalty.Cmp(stake) > 0 {
		penalty.Set(stake)
	}
	remaining := big.NewInt(block.Limit - block.Paid)
	if penalty.Cmp(remaining) > 0 {
		penalty.Set(remaining)
	}
	amount := penalty.Int64()
	seenStats := false
	for _, r := range hold.Slashes {
		if r.NodeID == node {
			seenStats = true
		}
	}
	if !seenStats {
		s.auditStatsEvent(hold, AuditStatsEvent{Kind: "slash", Node: node, Height: height})
	}
	hold.Slashes = append(hold.Slashes, SlashRecord{NodeID: node, Height: height, Revenue: revenue,
		StakeBefore: account.Stake, StakeFloor: floor.Int64(), Multiplier: multiplier, Amount: amount})
	account.Stake -= amount
	block.Paid += amount
	if merged {
		return amount, nil
	}
	prior = append(prior, height)
	if len(prior) > 3 {
		prior = prior[len(prior)-3:]
	}
	if s.Offenses == nil {
		s.Offenses = map[string][]int64{}
	}
	s.Offenses[node] = prior
	return amount, nil
}

// slashWithPenaltyCosts pays adjudication costs only from collected penalties.
func (s *State) slashWithPenaltyCosts(h *Hold, node string, revenue, height int64) error {
	amount, err := s.slashAmount(h, node, revenue, height)
	if err != nil {
		return err
	}
	if c := h.Adjudication; c != nil {
		due := min(c.PenaltyCosts-c.PenaltyCostsPaid, amount)
		if due > 0 {
			if err := s.fundAuditPool(h, due, height); err != nil {
				return err
			}
			c.PenaltyCostsPaid += due
			amount -= due
		}
	}
	return s.creditBalance(h.Payer, amount)
}
