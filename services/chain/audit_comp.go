package chain

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"

	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

// AuditCompParameters are per-profile governance policy, frozen at first data.
// Rates use decimal ktok (1000 tokens); one final floor avoids rounding drift.
type AuditCompParameters struct {
	FormulaVer    uint32 `json:"formula_ver"`
	OFixed        int64  `json:"o_fixed"`
	RIn           int64  `json:"r_in_per_ktok"`
	ROut          int64  `json:"r_out_per_ktok"`
	RSegPerKRow   int64  `json:"r_seg_per_krow,omitempty"`
	MarginBPS     uint32 `json:"margin_bps"`
	AuditShareBPS uint32 `json:"audit_share_bps"`
	BenchmarkRef  string `json:"benchmark_ref"`
}

func (p *AuditCompParameters) valid() bool {
	if p == nil {
		return false
	}
	if _, err := hex.DecodeString(p.BenchmarkRef); err != nil {
		return false
	}
	if !(p.OFixed >= 0 && p.RIn >= 0 && p.ROut >= 0 && p.RSegPerKRow >= 0 && p.MarginBPS >= 10000 && p.MarginBPS <= 100000 && p.AuditShareBPS == 1500 && len(p.BenchmarkRef) == 64) {
		return false
	}
	switch p.FormulaVer {
	case 2:
		return p.RSegPerKRow == 0
	case 3:
		return true
	}
	return false
}
func AuditCompAmount(p *AuditCompParameters, input, prefix uint32) (int64, error) {
	return AuditCompAmountSeg(p, input, prefix, 0)
}

// AuditCompAmountSeg is formula v3: the v2 token terms plus a segment term
// priced per thousand replayed segment rows, with the same single final
// floor. With segRows == 0 it reduces exactly to formula v2.
func AuditCompAmountSeg(p *AuditCompParameters, input, prefix, segRows uint32) (int64, error) {
	if !p.valid() {
		return 0, errors.New("invalid audit compensation policy")
	}
	if p.FormulaVer == 2 && segRows != 0 {
		return 0, errors.New("segment work requires formula v3")
	}
	n := new(big.Int).Mul(big.NewInt(p.OFixed), big.NewInt(1000))
	n.Add(n, new(big.Int).Mul(big.NewInt(p.RIn), new(big.Int).SetUint64(uint64(input))))
	n.Add(n, new(big.Int).Mul(big.NewInt(p.ROut), new(big.Int).SetUint64(uint64(prefix))))
	n.Add(n, new(big.Int).Mul(big.NewInt(p.RSegPerKRow), new(big.Int).SetUint64(uint64(segRows))))
	n.Mul(n, new(big.Int).SetUint64(uint64(p.MarginBPS)))
	n.Quo(n, big.NewInt(1000*10000))
	if !n.IsInt64() {
		return 0, errors.New("audit compensation overflow")
	}
	return n.Int64(), nil
}

type AuditWorkParameters struct {
	EpochBlocks    int64  `json:"epoch_blocks"`
	MaxPerExecutor uint64 `json:"max_per_executor"`
}

func (p AuditWorkParameters) valid() bool { return p.EpochBlocks > 0 && p.MaxPerExecutor > 0 }

type AuditWork struct {
	Auditor  string `json:"auditor"`
	Epoch    int64  `json:"epoch"`
	Input    uint32 `json:"n_in"`
	Prefix   uint32 `json:"n_out_billed"`
	SegRows  uint32 `json:"seg_rows,omitempty"`
	SegCount uint32 `json:"seg_count,omitempty"`
	Verdict  []byte `json:"verdict,omitempty"`
	Replayed bool   `json:"replayed"`
}

func (s *State) freezeAuditComp(h *Hold) {
	if p := s.Models[h.ModelID].AuditComp; p != nil {
		cp := *p
		h.AuditCompPolicy = &cp
	}
	h.AuditWorkPolicy = s.Parameters.AuditWork
}
func (s *State) reserveAuditWork(h *Hold, executors []string, auditor string, at ...int64) error {
	p := h.AuditWorkPolicy
	// Old direct draw records without a frozen work policy remain readable.
	if !p.valid() {
		return nil
	}
	height := s.Height
	if len(at) > 0 {
		height = at[0]
	}
	epoch := height / p.EpochBlocks
	// Admission uses a worst-case cited prefix and the measured pre-margin cost.
	// This prevents forcing known underfunded work; it cannot guarantee actual
	// wall cost or compensate a later full-refund rejection.
	if h.AuditCompPolicy != nil {
		costPolicy := *h.AuditCompPolicy
		costPolicy.MarginBPS = 10000
		var cost int64
		var err error
		if h.Moe != nil {
			// Admission bounds segment work by the frozen worst case.
			cost, err = AuditCompAmountSeg(&costPolicy, h.AuditInputTokens, h.AuditOutputTokens, h.Moe.WorstSegRows)
		} else {
			cost, err = AuditCompAmount(&costPolicy, h.AuditInputTokens, h.AuditOutputTokens)
		}
		if err != nil {
			return err
		}
		if auditBudget(h.Actual, h.AuditCompPolicy.AuditShareBPS) < cost {
			return errors.New("audit share cannot fund measured replay cost")
		}
	}
	for _, executor := range executors {
		key := fmt.Sprintf("%d/%s", epoch, executor)
		if s.AuditWorkCounts[key] >= p.MaxPerExecutor {
			return errors.New("executor epoch audit work cap reached")
		}
	}
	if s.AuditWorkCounts == nil {
		s.AuditWorkCounts = map[string]uint64{}
	}
	for _, executor := range executors {
		s.AuditWorkCounts[fmt.Sprintf("%d/%s", epoch, executor)]++
	}
	h.AuditWorks = append(h.AuditWorks, AuditWork{Auditor: auditor, Epoch: epoch, Input: h.AuditInputTokens})
	return nil
}
func auditBudget(fee int64, bps uint32) int64 {
	n := new(big.Int).Mul(big.NewInt(fee), new(big.Int).SetUint64(uint64(bps)))
	return n.Quo(n, big.NewInt(10000)).Int64()
}
func (h *Hold) citeAudit(raw, challengeRaw []byte) error {
	if h.AuditCompPolicy == nil {
		return nil
	}
	v := &pb.AuditVerdict{}
	if proto.Unmarshal(raw, v) != nil || v.NOutBilled > h.AuditOutputTokens || len(h.AuditWorks) == 0 {
		return errors.New("invalid cited audit prefix")
	}
	challenge := &pb.AuditChallenge{}
	if proto.Unmarshal(challengeRaw, challenge) != nil {
		return errors.New("invalid audit citation")
	}
	if h.Moe != nil {
		if h.AuditCompPolicy.FormulaVer != 3 {
			return errors.New("MoE audit compensation requires formula v3")
		}
		return h.citeMoeAudit(raw, v, challenge)
	}
	cited := uint32(0)
	for _, position := range challenge.Positions {
		if position >= h.AuditOutputTokens {
			return errors.New("audit position exceeds output")
		}
		if position+1 > cited {
			cited = position + 1
		}
	}
	if cited != v.NOutBilled {
		return errors.New("billed prefix differs from cited positions")
	}
	w := &h.AuditWorks[len(h.AuditWorks)-1]
	// An appeal reserves both independent reviews up front. Bind the citation
	// to its assigned auditor rather than assuming the last reserved review.
	if h.Adjudication != nil && h.Adjudication.Origin == "consumer_appeal" {
		for i := range h.AuditWorks {
			if h.AuditWorks[i].Auditor == v.AuditorId && !h.AuditWorks[i].Replayed {
				w = &h.AuditWorks[i]
				break
			}
		}
	}
	if w.Replayed || w.Auditor != v.AuditorId {
		return errors.New("audit work does not match assignment")
	}
	w.Prefix = v.NOutBilled
	w.Replayed = true
	w.Verdict = append([]byte(nil), raw...)
	return nil
}

// ExpectedAuditReceipts is also the tooling API; settlement requires exact
// equality, including shortfalls. Payment order is assignment order.
func ExpectedAuditReceipts(h *Hold, outcome string) ([]*pb.SettlementReceipt, error) {
	if h.AuditCompPolicy == nil {
		return nil, nil
	}
	remaining := int64(0)
	if outcome == "accept" {
		remaining = auditBudget(h.Actual, h.AuditCompPolicy.AuditShareBPS)
	}
	receipts := []*pb.SettlementReceipt{}
	for _, w := range h.AuditWorks {
		if !w.Replayed {
			continue
		}
		wanted, err := AuditCompAmountSeg(h.AuditCompPolicy, w.Input, w.Prefix, w.SegRows)
		if err != nil {
			return nil, err
		}
		paid := wanted
		if paid > remaining {
			paid = remaining
		}
		remaining -= paid
		version := uint32(2)
		if h.AuditCompPolicy.FormulaVer == 3 {
			version = 3
		}
		receipts = append(receipts, &pb.SettlementReceipt{Version: version, RequestId: h.RequestID, AuditComp: &pb.AuditComp{Auditor: w.Auditor, FormulaVer: h.AuditCompPolicy.FormulaVer, NIn: w.Input, NOutBilled: w.Prefix, CompSubunits: paid, SegRowsReplayed: w.SegRows, SegCount: w.SegCount}, AuditShortfallSubunits: wanted - paid})
	}
	return receipts, nil
}
func verifyAuditReceipts(h *Hold, outcome string, submitted []*pb.SettlementReceipt) error {
	expected, err := ExpectedAuditReceipts(h, outcome)
	if err != nil {
		return err
	}
	if len(expected) != len(submitted) {
		return errors.New("audit receipt count differs from frozen work")
	}
	for i := range expected {
		if !proto.Equal(expected[i], submitted[i]) {
			return errors.New("audit compensation differs from frozen formula")
		}
	}
	h.AuditReceipts = submitted
	return nil
}
func (s *State) payAuditComp(h *Hold) error {
	for _, r := range h.AuditReceipts {
		if err := s.creditBalance(r.AuditComp.Auditor, r.AuditComp.CompSubunits); err != nil {
			return err
		}
	}
	return nil
}
