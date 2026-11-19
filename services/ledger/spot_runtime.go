package ledger

import (
	"errors"
	"time"

	pb "ember/proto"
	chain "ember/services/chain"
	"google.golang.org/protobuf/proto"
)

// SpotRuntimeState reads finalized chain facts, never a gateway job's version.
func (l *ChainLedger) SpotRuntimeState(id string) (*chain.Hold, int64, uint64, error) {
	var state struct {
		Height  int64  `json:"height"`
		Version uint64 `json:"version"`
	}
	if err := l.query("/state", &state); err != nil {
		return nil, 0, 0, err
	}
	h, err := l.hold(id)
	return h, state.Height, state.Version, err
}

func PipelineSpotAction(id string, r *pb.PipelineExecutionReceipt, actual int64, total uint32, root []byte, count uint64) (chain.Action, error) {
	if len(root) != 32 || r == nil || count == 0 || count != uint64(r.OutputTokens) {
		return chain.Action{}, errors.New("invalid pipeline spot commitment")
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(r)
	return chain.Action{Type: "submit_pipeline", HoldID: id, Actual: actual, Receipt: raw, TotalLayers: total, CommitmentRoot: root, SpotStepCount: count}, err
}
func (l *ChainLedger) SubmitPipelineSpot(id string, r *pb.PipelineExecutionReceipt, actual int64, total uint32, window time.Duration, root []byte, count uint64) ([]StageShare, error) {
	_, _, version, err := l.SpotRuntimeState(id)
	if err != nil {
		return nil, err
	}
	if version < 14 {
		return l.SubmitPipeline(id, r, actual, total, window)
	}
	a, err := PipelineSpotAction(id, r, actual, total, root, count)
	if err != nil {
		return nil, err
	}
	if err = l.submit(l.coordinator, a); err != nil {
		return nil, err
	}
	return l.PipelineCredits(id)
}
