package ledger

import (
	"errors"
	"time"

	pb "ember/proto"
	chainapp "ember/services/chain"
	"google.golang.org/protobuf/proto"
)

// ChainLedger MoE client surface, mirroring the pipeline methods. The SQL
// Ledger predates profile-typed holds and does not serve MoE profiles;
// MoE runs against the chain ledger only.

type MoeAuthorization struct {
	NodeID     string
	Kind       string // "attention" | "expert"
	StartLayer uint32
	EndLayer   uint32
	Units      []chainapp.MoeUnitRef
}

func (l *ChainLedger) FreezeMoe(id, payer string, amount int64, authorizations []MoeAuthorization, inputTokens, maxTokens uint32, commitment []byte) error {
	auth := make([]chainapp.MoeAuthorization, len(authorizations))
	for i, v := range authorizations {
		auth[i] = chainapp.MoeAuthorization{NodeID: v.NodeID, Kind: v.Kind,
			StartLayer: v.StartLayer, EndLayer: v.EndLayer, Units: v.Units}
	}
	return l.withAccount(payer, chainapp.Action{Type: "freeze_moe", HoldID: id,
		Payer: payer, Amount: amount, MoeAuthorizations: auth,
		MoeInputTokens: inputTokens, MoeMaxTokens: maxTokens, PricingCommitment: commitment})
}

func (l *ChainLedger) SubmitMoe(id string, r *pb.PipelineExecutionReceipt, actual int64) ([]StageShare, error) {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(r)
	if err != nil {
		return nil, err
	}
	if err = l.submit(l.coordinator, chainapp.Action{Type: "submit_moe", HoldID: id, Actual: actual, Receipt: raw}); err != nil {
		return nil, err
	}
	return l.MoeCredits(id)
}

func (l *ChainLedger) FinishMoe(id, outcome string, _ time.Time, multiplier int64) error {
	if multiplier != 20 {
		return errors.New("chain slash multiplier is fixed by genesis")
	}
	h, err := l.hold(id)
	if err != nil {
		return err
	}
	if h.AuditVerdict == "service_failure" {
		outcome = h.AuditVerdict
	}
	receipts, err := chainapp.ExpectedAuditReceipts(h, outcome)
	if err != nil {
		return err
	}
	return l.submit(l.coordinator, chainapp.Action{Type: "settle", HoldID: id, Outcome: outcome, AuditReceipts: receipts})
}

func (l *ChainLedger) CancelMoe(id string) error { return l.Cancel(id) }

func (l *ChainLedger) MoeCredits(id string) ([]StageShare, error) {
	h, err := l.hold(id)
	if err != nil {
		return nil, err
	}
	if h.Moe == nil {
		return nil, errors.New("not a MoE hold")
	}
	return h.Moe.Credits, nil
}

// MoeSettlementReady reads consensus height through the existing query client.
func (l *ChainLedger) MoeSettlementReady(id string) (bool, error) {
	h, err := l.hold(id)
	if err != nil {
		return false, err
	}
	var s chainapp.State
	if err = l.query("/state", &s); err != nil {
		return false, err
	}
	return s.Height >= h.SettleHeight, nil
}
