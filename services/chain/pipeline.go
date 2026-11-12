package chain

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

type PipelineAuthorization struct {
	NodeID     string `json:"node_id"`
	StartLayer uint32 `json:"start_layer"`
	EndLayer   uint32 `json:"end_layer"`
	Profile    []byte `json:"profile,omitempty"`
	Reserved   int64  `json:"reserved,omitempty"`
}
type PipelineHold struct {
	FirstFaultNodes []string                `json:"first_fault_nodes,omitempty"`
	TotalLayers     uint32                  `json:"total_layers"`
	Authorizations  []PipelineAuthorization `json:"authorizations"`
	Receipt         []byte                  `json:"receipt,omitempty"`
	Credits         []core.StageShare       `json:"credits,omitempty"`
}

func (s *State) freezePipeline(signer string, a Action, height int64) error {
	if a.HoldID == "" || s.Holds[a.HoldID] != nil || a.Payer != signer || a.Amount <= 0 || a.TotalLayers == 0 || len(a.PipelineAuthorizations) == 0 {
		return errors.New("invalid pipeline freeze")
	}
	seen := map[string]bool{}
	modelID := ""
	authorizations := make([]PipelineAuthorization, 0, len(a.PipelineAuthorizations))
	reserved := map[string]int64{}
	for _, auth := range a.PipelineAuthorizations {
		p := s.ActiveProfile(auth.NodeID, height)
		if p == nil || auth.NodeID == signer || !HasRole(p, "shard-stage") || p.ShardTotalLayers != a.TotalLayers || (!p.ShardDynamicRange && (p.ShardStartLayer != auth.StartLayer || p.ShardEndLayer != auth.EndLayer)) {
			return errors.New("unregistered pipeline capability")
		}
		if err := s.validatePublishedNode(p); err != nil {
			return err
		}
		if _, _, err := core.ExecutedSpan(p, auth.StartLayer, auth.EndLayer); err != nil {
			return err
		}
		if p.ShardDynamicRange {
			_, raw := modelArtifacts(s.Models[p.ChainId])
			manifest := &pb.ModelManifest{}
			if proto.Unmarshal(raw, manifest) != nil || !core.AllowedRange(manifest, auth.StartLayer, auth.EndLayer) {
				return errors.New("execution violates signed cut/granularity rules")
			}
		}
		key := fmt.Sprintf("%s/%d/%d", auth.NodeID, auth.StartLayer, auth.EndLayer)
		if seen[key] || (modelID != "" && p.ChainId != modelID) {
			return errors.New("duplicate or mixed pipeline authorization")
		}
		seen[key] = true
		modelID = p.ChainId
		if len(s.Models[modelID].Manifest) == 0 && s.Models[modelID].Publication == nil {
			return errors.New("pipeline requires anchored signed layout")
		}
		weight := core.StageWeight(auth.StartLayer, auth.EndLayer)
		n := new(big.Int).Mul(big.NewInt(a.Amount), new(big.Int).SetUint64(uint64(weight)))
		n.Add(n, new(big.Int).SetUint64(uint64(a.TotalLayers-1)))
		n.Quo(n, new(big.Int).SetUint64(uint64(a.TotalLayers)))
		if !n.IsInt64() {
			return errors.New("pipeline exposure overflow")
		}
		auth.Reserved = n.Int64()
		auth.Profile = append([]byte(nil), s.Nodes[auth.NodeID].Profile...)
		value := reserved[auth.NodeID]
		if err := add(&value, auth.Reserved); err != nil {
			return err
		}
		reserved[auth.NodeID] = value
		authorizations = append(authorizations, auth)
	}
	for id, value := range reserved {
		if value > s.Accounts[id].Stake/5-s.exposure(id) {
			return errors.New("pipeline stake exposure limit")
		}
	}
	if err := s.debitBalance(signer, a.Amount); err != nil {
		return err
	}
	f, err := s.freezeFee(a, modelID, "")
	if err != nil {
		return err
	}
	s.Holds[a.HoldID] = &Hold{PrivacyTier: "P0", TokenFee: f, ID: a.HoldID, Payer: signer, Amount: a.Amount, State: "frozen", ModelID: modelID, FreezeHeight: height, ExpireHeight: height + s.Parameters.HoldTimeout, Pipeline: &PipelineHold{TotalLayers: a.TotalLayers, Authorizations: authorizations}}
	return nil
}

func (s *State) submitPipeline(signer string, a Action, height int64) error {
	h := s.Holds[a.HoldID]
	if h == nil || h.Pipeline == nil || h.State != "frozen" || height >= h.ExpireHeight || !s.Coordinators[signer] || a.Actual <= 0 || a.Actual > h.Amount {
		return errors.New("invalid pipeline submission")
	}
	receipt := &pb.PipelineExecutionReceipt{}
	if proto.Unmarshal(a.Receipt, receipt) != nil || receipt.RequestId != h.ID || receipt.InputTokens == 0 || receipt.OutputTokens == 0 || receipt.ElapsedMs <= 0 || len(receipt.OutputCommitment) != 32 {
		return errors.New("invalid pipeline aggregate")
	}
	if err := verifyTokenFee(h.TokenFee, receipt.InputTokens, receipt.OutputTokens, a.Actual, nil); err != nil {
		return err
	}
	if err := s.validateLatencyWitnesses(h, receipt); err != nil {
		return err
	}
	type span struct {
		start, end uint32
		receipt    *pb.ShardStepReceipt
	}
	coverage := map[uint32][]span{}
	work := []core.StageWork{}
	for _, stage := range receipt.StageReceipts {
		if core.Verify(stage) != nil || core.NodeID(stage.Signer) != stage.NodeId || stage.RequestId != h.ID || stage.Step >= receipt.OutputTokens || len(stage.RequestHash) != 32 || len(stage.ResponseHash) != 32 {
			return errors.New("invalid signed pipeline stage")
		}
		var p *pb.ResourceProfile
		for _, auth := range h.Pipeline.Authorizations {
			if auth.NodeID == stage.NodeId && auth.StartLayer == stage.StartLayer && auth.EndLayer == stage.EndLayer {
				candidate := &pb.ResourceProfile{}
				if proto.Unmarshal(auth.Profile, candidate) == nil {
					p = candidate
				}
				break
			}
		}
		if p == nil || stage.UnitKind != p.ShardUnitKind || stage.ShardIndex != p.ShardIndex || stage.ChainId != p.ChainId || !bytes.Equal(stage.ManifestRoot, p.ManifestRoot) || stage.ExecutionProfileVer != p.ExecutionProfileVer || !bytes.Equal(stage.ParametersHash, p.ShardParametersHash) {
			return errors.New("executed stage not authorized by frozen registration")
		}
		start, end, err := core.ExecutedSpan(p, stage.StartLayer, stage.EndLayer)
		if err != nil {
			return err
		}
		coverage[stage.Step] = append(coverage[stage.Step], span{start: start, end: end, receipt: stage})
		work = append(work, core.StageWork{NodeID: stage.NodeId, StartLayer: 0, EndLayer: core.StageWeight(stage.StartLayer, stage.EndLayer), Tokens: 1})
	}
	for step := uint32(0); step < receipt.OutputTokens; step++ {
		spans := coverage[step]
		sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
		cursor := uint32(0)
		var previous *pb.ShardStepReceipt
		for _, v := range spans {
			if v.receipt.UnitKind != "" && ((v.start > 0 && len(v.receipt.InputActivationHash) != 32) || (v.end < h.Pipeline.TotalLayers+2 && len(v.receipt.OutputActivationHash) != 32)) {
				return errors.New("missing activation boundary commitment")
			}
			if previous != nil && previous.UnitKind != "" && !bytes.Equal(previous.OutputActivationHash, v.receipt.InputActivationHash) {
				return errors.New("activation boundary discontinuity")
			}
			if v.start != cursor {
				return errors.New("pipeline gap or overlap")
			}
			cursor = v.end
			previous = v.receipt
		}
		if cursor != h.Pipeline.TotalLayers+2 {
			return errors.New("incomplete pipeline unit coverage")
		}
	}
	shares, err := core.SplitInvocationCharge(a.Actual, work)
	if err != nil {
		return err
	}
	ref, err := core.PipelineReceiptRef(receipt)
	if err != nil {
		return err
	}
	if h.TokenFee != nil {
		h.TokenFee.OutputTokens = receipt.OutputTokens
	}
	h.Actual = a.Actual
	h.ReceiptRef = hex.EncodeToString(ref)
	h.State = "dispute"
	h.SubmitHeight = height
	h.SettleHeight = height + s.Parameters.DisputeBlocks
	h.AuditInputTokens = receipt.InputTokens
	h.AuditOutputTokens = receipt.OutputTokens
	h.Pipeline.Receipt = append([]byte(nil), a.Receipt...)
	h.Pipeline.Credits = shares
	return nil
}

func (s *State) selectPipelineAuditor(h *Hold, exclude string, at ...int64) (string, []byte, error) {
	excluded := map[string]bool{exclude: true}
	if s.Parameters.Scheduling != nil && s.Parameters.Scheduling.ConsumerCrossCheck != nil {
		excluded[h.Payer] = true
	}
	for _, auth := range h.Pipeline.Authorizations {
		excluded[auth.NodeID] = true
	}
	ids := []string{}
	for id, enabled := range s.Auditors {
		if enabled && !excluded[id] {
			ids = append(ids, id)
		}
	}
	return s.drawAuditor(h, ids, at...)
}

func (s *State) settlePipeline(h *Hold, outcome string, height int64) error {
	if outcome == "accept" {
		if err := s.collectLatency(h); err != nil {
			return err
		}
		if err := s.creditBalance(h.Payer, h.Amount-h.Actual); err != nil {
			return err
		}
		credits := h.Pipeline.Credits
		if h.AuditCompPolicy != nil {
			parts := allocateFee(h.Actual, [3]int64{80, 15, 5})
			h.Shares = &SettlementShares{Fee: h.Actual, Executor: parts[0], Auditor: parts[1], Consumer: parts[2]}
			h.AuditEscrow = parts[1]
			if err := s.creditBalance(h.Payer, parts[2]); err != nil {
				return err
			}
			credits = append([]core.StageShare(nil), credits...)
			left := parts[0]
			for i := range credits {
				n := new(big.Int).Mul(big.NewInt(credits[i].Amount), big.NewInt(parts[0]))
				if h.Actual > 0 {
					credits[i].Amount = n.Quo(n, big.NewInt(h.Actual)).Int64()
				} else {
					credits[i].Amount = 0
				}
				left -= credits[i].Amount
			}
			for i := int64(0); i < left; i++ {
				credits[i%int64(len(credits))].Amount++
			}
			for _, r := range h.AuditReceipts {
				h.AuditEscrow -= r.AuditComp.CompSubunits
			}
			if h.AuditEscrow < 0 {
				return errors.New("pipeline audit funding exceeded")
			}
			if err := s.payAuditComp(h); err != nil {
				return err
			}
		}
		if h.AuditCompPolicy != nil {
			h.Pipeline.Credits = credits
		}
		for _, share := range credits {
			if err := s.creditBalance(share.NodeID, share.Amount); err != nil {
				return err
			}
		}
		h.State = "settled"
		if m := s.Models[h.ModelID]; m.Publication != nil {
			m.Publication.SettledRequests++
		}
		return nil
	}
	if outcome != "reject" && outcome != "service_failure" {
		return errors.New("invalid pipeline settlement outcome")
	}
	if err := s.creditBalance(h.Payer, h.Amount); err != nil {
		return err
	}
	h.State = "cancelled"
	if outcome == "reject" {
		v := &pb.AuditVerdict{}
		if proto.Unmarshal(h.VerdictBytes, v) != nil || len(v.FaultNodeIds) == 0 {
			return errors.New("pipeline rejection needs signed fault attribution")
		}
		faults := map[string]bool{}
		for _, id := range v.FaultNodeIds {
			if faults[id] {
				return errors.New("duplicate pipeline fault")
			}
			faults[id] = true
		}
		for _, share := range h.Pipeline.Credits {
			if !faults[share.NodeID] {
				continue
			}
			delete(faults, share.NodeID)
			slash, err := s.recordOffense(h, share.NodeID, height)
			if err != nil {
				return err
			}
			if !slash {
				continue
			}
			if err := s.slashOffender(h, share.NodeID, share.Amount, height); err != nil {
				return err
			}
		}
		if len(faults) > 0 {
			return errors.New("fault names an uncredited stage")
		}
		if len(h.Slashes) > 0 {
			h.State = "slashed"
		}
	}
	return nil
}

func validatePipelineFaults(h *Hold, raw []byte) error {
	v := &pb.AuditVerdict{}
	if proto.Unmarshal(raw, v) != nil || len(v.FaultNodeIds) == 0 {
		return errors.New("pipeline rejection lacks fault attribution")
	}
	allowed := map[string]bool{}
	for _, credit := range h.Pipeline.Credits {
		allowed[credit.NodeID] = true
	}
	seen := map[string]bool{}
	for _, id := range v.FaultNodeIds {
		if !allowed[id] || seen[id] {
			return errors.New("invalid pipeline fault attribution")
		}
		seen[id] = true
	}
	ids := append([]string(nil), v.FaultNodeIds...)
	sort.Strings(ids)
	if h.AuditRound == 1 {
		h.Pipeline.FirstFaultNodes = ids
	} else {
		if len(ids) != len(h.Pipeline.FirstFaultNodes) {
			return errors.New("pipeline references disagree on fault attribution")
		}
		for i, id := range ids {
			if id != h.Pipeline.FirstFaultNodes[i] {
				return errors.New("pipeline references disagree on fault attribution")
			}
		}
	}
	return nil
}
