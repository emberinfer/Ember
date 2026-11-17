package chain

import "fmt"

// Retained holds remain exit liabilities even after early refunds. There is no
// allocation of individual stake units to closed requests, so block all exits.
func (s *State) checkExitWindows(node string, height int64) error {
	closeHeight := int64(0)
	active := false
	for _, h := range s.historicalHolds() {
		served := h.Node == node
		if h.Pipeline != nil {
			for _, a := range h.Pipeline.Authorizations {
				served = served || a.NodeID == node
			}
		}
		if s.Privacy != nil {
			if r := s.Privacy.Requests[h.ID]; r != nil {
				for _, m := range r.Migrations {
					served = served || m.From == node || m.To == node
				}
			}
		}
		if !served {
			continue
		}
		active = active || h.State == "frozen" || h.State == "dispute"
		// Expiry bounds requests without a receipt; settlement height also
		// covers a receipt submitted near expiry with a longer dispute period.
		if h.ExpireHeight > closeHeight {
			closeHeight = h.ExpireHeight
		}
		if h.SettleHeight > closeHeight {
			closeHeight = h.SettleHeight
		}
	}
	if height < closeHeight {
		return fmt.Errorf("request challenge window open: height %d closes %d", height, closeHeight)
	}
	// A late audit may still be actionable after the nominal deadline until
	// settlement or expiry is finalized. This also protects pending claims.
	if active || s.exposure(node) > 0 {
		return fmt.Errorf("stake remains exposed")
	}
	return nil
}
