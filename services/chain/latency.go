package chain

import (
	"bytes"
	"encoding/hex"
	"errors"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

type LatencySample struct {
	Auditor               string `json:"auditor,omitempty"`
	ConsumerObservedNS    uint64 `json:"consumer_observed_ns,omitempty"`
	ConsumerMicrosPerUnit uint64 `json:"consumer_micros_per_unit,omitempty"`
	StageReceiptRef       string `json:"stage_receipt_ref"`
	HoldID                string `json:"hold_id"`
	Step                  uint32 `json:"step"`
	StartLayer            uint32 `json:"start_layer"`
	EndLayer              uint32 `json:"end_layer"`
	ObservedNS            uint64 `json:"observed_ns"`
	Units                 uint32 `json:"units"`
	MicrosPerUnit         uint64 `json:"micros_per_unit"`
}

type LatencyStats struct {
	AuditorPercentileMicrosPerUnit uint64              `json:"auditor_percentile_micros_per_unit,omitempty"`
	Divergence                     []AuditorDivergence `json:"divergence,omitempty"`
	NodeID                         string              `json:"node_id"`
	ModelID                        string              `json:"model_id"`
	VerifiedSamples                uint64              `json:"verified_samples"`
	Window                         []LatencySample     `json:"window"`
	PercentileMicrosPerUnit        uint64              `json:"percentile_micros_per_unit"`
}

func LatencyKey(node, model string) string { return node + "|" + model }

func (s *State) validateLatencyWitnesses(h *Hold, r *pb.PipelineExecutionReceipt) error {
	p := s.Parameters.Scheduling
	if p == nil {
		return nil
	}
	if len(r.LatencyWitnesses) != len(r.StageReceipts) {
		return errors.New("latency policy requires countersigned stage observations")
	}
	excluded := map[string]bool{}
	for _, a := range h.Pipeline.Authorizations {
		excluded[a.NodeID] = true
	}
	for i, w := range r.LatencyWitnesses {
		ref, err := core.Digest(r.StageReceipts[i])
		observer := core.NodeID(w.Signer)
		if err != nil || core.Verify(w) != nil || !bytes.Equal(ref, w.StageReceiptRef) || !s.Auditors[observer] || excluded[observer] || w.StartedMonoNs == 0 || w.CompletedMonoNs <= w.StartedMonoNs || w.CompletedMonoNs-w.StartedMonoNs > p.MaxObservationNS {
			return errors.New("invalid independent latency witness")
		}
	}
	return nil
}

func (s *State) collectLatency(h *Hold) error {
	p := s.Parameters.Scheduling
	if p == nil {
		return nil
	}
	r := &pb.PipelineExecutionReceipt{}
	if proto.Unmarshal(h.Pipeline.Receipt, r) != nil {
		return errors.New("invalid latency receipt")
	}
	if err := s.validateLatencyWitnesses(h, r); err != nil {
		return err
	}
	if p.ConsumerCrossCheck != nil && (h.ConsumerLatency == nil || len(h.ConsumerLatency.Stages) != len(r.StageReceipts) || h.ConsumerLatency.Observer == h.Auditor) {
		return errors.New("independent payer latency observation required before settlement")
	}
	if s.Latencies == nil {
		s.Latencies = map[string]*LatencyStats{}
	}
	for i, w := range r.LatencyWitnesses {
		// Only the final independently assigned ACCEPT auditor can create a sample.
		if core.NodeID(w.Signer) != h.Auditor {
			continue
		}
		stage := r.StageReceipts[i]
		units := core.StageWeight(stage.StartLayer, stage.EndLayer)
		elapsed := w.CompletedMonoNs - w.StartedMonoNs
		divisor := uint64(units) * 1000
		normalized := elapsed / divisor
		if elapsed%divisor != 0 {
			normalized++
		}
		key := LatencyKey(stage.NodeId, h.ModelID)
		stats := s.Latencies[key]
		if stats == nil {
			stats = &LatencyStats{NodeID: stage.NodeId, ModelID: h.ModelID}
			s.Latencies[key] = stats
		}
		stats.VerifiedSamples++
		sample := LatencySample{StageReceiptRef: hex.EncodeToString(w.StageReceiptRef), HoldID: h.ID, Step: stage.Step, StartLayer: stage.StartLayer, EndLayer: stage.EndLayer, ObservedNS: elapsed, Units: units, MicrosPerUnit: normalized}
		if p.ConsumerCrossCheck != nil {
			sample.Auditor = h.Auditor
			sample.ConsumerObservedNS = h.ConsumerLatency.Stages[i].ObservedNS
			sample.ConsumerMicrosPerUnit = (sample.ConsumerObservedNS + divisor - 1) / divisor
		}
		stats.Window = append(stats.Window, sample)
		if len(stats.Window) > int(p.WindowSamples) {
			stats.Window = stats.Window[len(stats.Window)-int(p.WindowSamples):]
		}
		s.refreshLatency(stats)
	}
	return nil
}
