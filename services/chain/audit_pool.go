package chain

import (
	"errors"
	"math/big"
	"sort"
)

type AuditPoolDistribution struct {
	ModelID  string           `json:"model_id"`
	Epoch    int64            `json:"epoch"`
	Amount   int64            `json:"amount"`
	Credits  map[string]int64 `json:"credits,omitempty"`
	RolledTo *int64           `json:"rolled_to,omitempty"`
}

func (s *State) fundAuditPool(h *Hold, amount, height int64) error {
	if amount == 0 {
		return nil
	}
	if amount < 0 {
		return errors.New("negative audit pool funding")
	}
	epochBlocks := h.AuditWorkPolicy.EpochBlocks
	if epochBlocks <= 0 {
		epochBlocks = s.Parameters.AuditWork.EpochBlocks
	}
	if epochBlocks <= 0 {
		return errors.New("invalid audit pool epoch")
	}
	if s.AuditPool == nil {
		s.AuditPool = map[string]map[int64]int64{}
	}
	if s.AuditPool[h.ModelID] == nil {
		s.AuditPool[h.ModelID] = map[int64]int64{}
	}
	epoch := height / epochBlocks
	n := s.AuditPool[h.ModelID][epoch]
	if err := add(&n, amount); err != nil {
		return err
	}
	s.AuditPool[h.ModelID][epoch] = n
	return nil
}
func (s *State) distributeAuditPool(model string, epoch, height int64) error {
	blocks := s.Parameters.AuditWork.EpochBlocks
	if s.Version < 8 || blocks <= 0 || epoch < 0 || epoch >= height/blocks {
		return errors.New("audit pool epoch has not ended")
	}
	// A policy-free profile has no audit allocation; distribution is a no-op.
	amount := s.AuditPool[model][epoch]
	if amount == 0 {
		return nil
	}
	work := map[string]*big.Int{}
	total := new(big.Int)
	for _, h := range s.Holds {
		if h.ModelID != model {
			continue
		}
		for _, e := range h.AuditStatsEvents {
			if e.Kind != "audit" || e.Height/blocks != epoch || e.Actual <= 0 {
				continue
			}
			if work[e.Node] == nil {
				work[e.Node] = new(big.Int)
			}
			work[e.Node].Add(work[e.Node], big.NewInt(e.Actual))
			total.Add(total, big.NewInt(e.Actual))
		}
	}
	d := AuditPoolDistribution{ModelID: model, Epoch: epoch, Amount: amount, Credits: map[string]int64{}}
	if total.Sign() == 0 {
		next := epoch + 1
		n := s.AuditPool[model][next]
		if err := add(&n, amount); err != nil {
			return err
		}
		s.AuditPool[model][next] = n
		d.RolledTo = &next
	} else {
		ids := []string{}
		for id := range work {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		rem := map[string]*big.Int{}
		left := amount
		for _, id := range ids {
			n := new(big.Int).Mul(big.NewInt(amount), work[id])
			q, r := new(big.Int), new(big.Int)
			q.QuoRem(n, total, r)
			d.Credits[id] = q.Int64()
			rem[id] = r
			left -= q.Int64()
		}
		// Largest remainder; lexicographic identity breaks equal remainders.
		sort.SliceStable(ids, func(i, j int) bool { return rem[ids[i]].Cmp(rem[ids[j]]) > 0 })
		for i := int64(0); i < left; i++ {
			d.Credits[ids[i]]++
		}
		for _, id := range ids {
			if err := s.creditBalance(id, d.Credits[id]); err != nil {
				return err
			}
		}
	}
	delete(s.AuditPool[model], epoch)
	s.AuditPoolDistributions = append(s.AuditPoolDistributions, d)
	return nil
}
