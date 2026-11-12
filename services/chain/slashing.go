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
	account := s.Accounts[node]
	if account == nil || account.Stake < 0 || revenue < 0 || height < 0 {
		return errors.New("invalid confirmed-cheating slash")
	}
	merged := false
	prior := make([]int64, 0, 4)
	for _, previous := range s.Offenses[node] {
		if previous == height {
			merged = true
		}
		if previous > height {
			return errors.New("confirmed offense height regression")
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
		return errors.New("block slash height regression")
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
	if err := s.creditBalance(hold.Payer, amount); err != nil {
		return err
	}
	hold.Slashes = append(hold.Slashes, SlashRecord{NodeID: node, Height: height, Revenue: revenue,
		StakeBefore: account.Stake, StakeFloor: floor.Int64(), Multiplier: multiplier, Amount: amount})
	account.Stake -= amount
	block.Paid += amount
	if merged {
		return nil
	}
	prior = append(prior, height)
	if len(prior) > 3 {
		prior = prior[len(prior)-3:]
	}
	if s.Offenses == nil {
		s.Offenses = map[string][]int64{}
	}
	s.Offenses[node] = prior
	return nil
}
