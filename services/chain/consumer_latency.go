package chain

import (
	"encoding/hex"
	"errors"
	"math/big"
	"sort"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

type ConsumerStageObservation struct {
	StageReceiptRef string `json:"stage_receipt_ref"`
	ObservedNS      uint64 `json:"observed_ns"`
}

type ConsumerLatencyObservation struct {
	ReceiptRef        string                     `json:"receipt_ref"`
	ObservedNS        uint64                     `json:"observed_ns"`
	Stages            []ConsumerStageObservation `json:"stages,omitempty"`
	Observer          string                     `json:"observer,omitempty"`
	SignedTransaction []byte                     `json:"signed_transaction,omitempty"`
}

type AuditorDivergence struct {
	Auditor       string `json:"auditor"`
	Samples       uint32 `json:"samples"`
	Disagreements uint32 `json:"disagreements"`
	Divergent     bool   `json:"divergent"`
}

func (s *State) observeConsumerLatency(signer string, action Action) error {
	h := s.Holds[action.HoldID]
	o := action.ConsumerLatency
	if h == nil || h.State != "dispute" || signer != h.Payer || signer == h.Node || o == nil || o.Observer != "" || len(o.SignedTransaction) != 0 || h.ConsumerLatency != nil || o.ReceiptRef != h.ReceiptRef || o.ObservedNS == 0 || o.ObservedNS > 86400000000000 {
		return errors.New("invalid payer-signed receipt latency observation")
	}
	if h.Pipeline != nil {
		r := &pb.PipelineExecutionReceipt{}
		if proto.Unmarshal(h.Pipeline.Receipt, r) != nil || len(o.Stages) != len(r.StageReceipts) {
			return errors.New("consumer observations must cover every executed stage")
		}
		var total uint64
		for index, stage := range r.StageReceipts {
			ref, err := core.Digest(stage)
			observed := o.Stages[index]
			if err != nil || observed.StageReceiptRef != hex.EncodeToString(ref) || observed.ObservedNS == 0 || observed.ObservedNS > o.ObservedNS-total {
				return errors.New("consumer stage observation is unbound or outside request interval")
			}
			if policy := s.Parameters.Scheduling; policy != nil && observed.ObservedNS > policy.MaxObservationNS {
				return errors.New("consumer stage observation exceeds scheduling interval bound")
			}
			total += observed.ObservedNS
		}
	} else if len(o.Stages) != 0 {
		return errors.New("whole-model observation cannot invent stage samples")
	}
	observation := *o
	observation.Stages = append([]ConsumerStageObservation(nil), o.Stages...)
	observation.Observer = signer
	h.ConsumerLatency = &observation
	return nil
}

func latencyDisagrees(auditor, consumer uint64, ratio uint32) bool {
	if auditor > consumer {
		auditor, consumer = consumer, auditor
	}
	left := new(big.Int).Mul(new(big.Int).SetUint64(consumer), big.NewInt(1000000))
	right := new(big.Int).Mul(new(big.Int).SetUint64(auditor), new(big.Int).SetUint64(uint64(ratio)))
	return left.Cmp(right) > 0
}

func (s *State) refreshLatency(stats *LatencyStats) {
	p := s.Parameters.Scheduling
	values := make([]uint64, 0, len(stats.Window))
	for _, sample := range stats.Window {
		values = append(values, sample.MicrosPerUnit)
	}
	stats.PercentileMicrosPerUnit = core.LatencyPercentile(values, p.PercentilePPM)
	if p.ConsumerCrossCheck == nil {
		return
	}
	stats.AuditorPercentileMicrosPerUnit = stats.PercentileMicrosPerUnit
	c := p.ConsumerCrossCheck
	byAuditor := map[string]*AuditorDivergence{}
	for _, sample := range stats.Window {
		pair := byAuditor[sample.Auditor]
		if pair == nil {
			pair = &AuditorDivergence{Auditor: sample.Auditor}
			byAuditor[sample.Auditor] = pair
		}
		pair.Samples++
		if latencyDisagrees(sample.ObservedNS, sample.ConsumerObservedNS, c.MaxRatioPPM) {
			pair.Disagreements++
		}
	}
	stats.Divergence = nil
	for _, pair := range byAuditor {
		pair.Divergent = pair.Samples >= c.MinSamples && uint64(pair.Disagreements)*1000000 >= uint64(pair.Samples)*uint64(c.DivergencePPM)
		stats.Divergence = append(stats.Divergence, *pair)
	}
	sort.Slice(stats.Divergence, func(left, right int) bool { return stats.Divergence[left].Auditor < stats.Divergence[right].Auditor })
	values = values[:0]
	for _, sample := range stats.Window {
		value := sample.MicrosPerUnit
		if byAuditor[sample.Auditor].Divergent {
			value = sample.ConsumerMicrosPerUnit
		}
		values = append(values, value)
	}
	stats.PercentileMicrosPerUnit = core.LatencyPercentile(values, p.PercentilePPM)
}
