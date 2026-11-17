package chain

import pb "ember/proto"

// ConsumerGrayParameters governs payment only, independently of executor slashing.
type ConsumerGrayParameters struct {
	WindowBlocks      int64  `json:"window_blocks"`
	GlobalMinSamples  uint32 `json:"global_min_samples"`
	GlobalGrayRateBPS uint32 `json:"global_gray_rate_bps"`
	PairMinSamples    uint32 `json:"pair_min_samples"`
	PairMinGray       uint32 `json:"pair_min_gray"`
	PairGrayRateBPS   uint32 `json:"pair_gray_rate_bps"`
}

func (p *ConsumerGrayParameters) defaults() {
	if p.WindowBlocks == 0 {
		p.WindowBlocks = 1000
	}
	if p.GlobalMinSamples == 0 {
		p.GlobalMinSamples = 20
	}
	if p.GlobalGrayRateBPS == 0 {
		p.GlobalGrayRateBPS = 5000
	}
	if p.PairMinSamples == 0 {
		p.PairMinSamples = 10
	}
	if p.PairMinGray == 0 {
		p.PairMinGray = 5
	}
	if p.PairGrayRateBPS == 0 {
		p.PairGrayRateBPS = 5000
	}
}

func (p ConsumerGrayParameters) valid() bool {
	return p.WindowBlocks > 0 && p.GlobalMinSamples > 0 && p.GlobalGrayRateBPS > 0 && p.GlobalGrayRateBPS <= 10000 && p.PairMinSamples > 0 && p.PairMinGray > 0 && p.PairGrayRateBPS > 0 && p.PairGrayRateBPS <= 10000
}

type ConsumerGraySnapshot struct {
	Charge     bool                   `json:"charge"`
	Scope      string                 `json:"scope"`
	Parameters ConsumerGrayParameters `json:"parameters"`
}

// Samples use settlement height and hold identity, including charged gray rejects.
type ConsumerSample struct {
	Consumer string `json:"consumer"`
	Node     string `json:"node"`
	Height   int64  `json:"height"`
	Gray     bool   `json:"gray"`
	HoldID   string `json:"hold_id"`
}

func (s *State) freezeConsumerGray(h *Hold, height int64) {
	if h.Pipeline != nil {
		return
	}
	p := s.Parameters.Misjudge.ConsumerGray
	// Version-5 states predating this policy have no consumer parameter block.
	p.defaults()
	snapshot := &ConsumerGraySnapshot{Parameters: p}
	var total, gray, pairTotal, pairGray uint64
	for _, sample := range s.ConsumerSamples {
		if sample.Consumer != h.Payer || height < sample.Height || height-sample.Height >= p.WindowBlocks {
			continue
		}
		total++
		if sample.Gray {
			gray++
		}
		if sample.Node == h.Node {
			pairTotal++
			if sample.Gray {
				pairGray++
			}
		}
	}
	if total >= uint64(p.GlobalMinSamples) && grayRateAtLeast(gray, total, p.GlobalGrayRateBPS) {
		snapshot.Charge, snapshot.Scope = true, "global"
	} else if pairTotal >= uint64(p.PairMinSamples) && pairGray >= uint64(p.PairMinGray) && grayRateAtLeast(pairGray, pairTotal, p.PairGrayRateBPS) {
		snapshot.Charge, snapshot.Scope = true, "pair"
	}
	h.ConsumerGray = snapshot
}

// Equivalent to gray*10000 >= total*bps, without overflowing either product.
func grayRateAtLeast(gray, total uint64, bps uint32) bool {
	required := total/10000*uint64(bps) + (total%10000*uint64(bps)+9999)/10000
	return gray >= required
}

func (s *State) consumerPaymentOutcome(h *Hold, outcome string, height int64) (string, error) {
	if outcome != "reject" || h.Pipeline != nil || h.ConsumerGray == nil || !h.ConsumerGray.Charge {
		return outcome, nil
	}
	r, err := s.classifyOffense(h, h.Node, height)
	if err != nil {
		return "", err
	}
	if r.Status == "gray" || r.Status == "pending-review" {
		return "accept", nil
	}
	return outcome, nil
}

func (s *State) recordConsumerSample(h *Hold, outcome string, height int64) {
	if h.Pipeline != nil || (outcome != "accept" && outcome != "reject") {
		return
	}
	if _, exists := s.ConsumerSamples[h.ID]; exists {
		return
	}
	gray := false
	if outcome == "reject" {
		for i := len(s.OffenseRecords) - 1; i >= 0; i-- {
			r := s.OffenseRecords[i]
			if r.HoldID == h.ID && r.NodeID == h.Node {
				gray = r.Status == "gray" || r.Status == "pending-review"
				break
			}
		}
	}
	if s.ConsumerSamples == nil {
		s.ConsumerSamples = map[string]ConsumerSample{}
	}
	s.ConsumerSamples[h.ID] = ConsumerSample{Consumer: h.Payer, Node: h.Node, Height: height, Gray: gray, HoldID: h.ID}
}

// ExpectedSettlementAuditReceipts preserves the signed audit outcome while using
// the normal payment budget for a frozen, chargeable gray request. Tooling must
// refresh state and retry if classification changes before transaction inclusion.
func (s *State) ExpectedSettlementAuditReceipts(h *Hold, outcome string, height int64) ([]*pb.SettlementReceipt, error) {
	payment, err := s.consumerPaymentOutcome(h, outcome, height)
	if err != nil {
		return nil, err
	}
	return ExpectedAuditReceipts(h, payment)
}
