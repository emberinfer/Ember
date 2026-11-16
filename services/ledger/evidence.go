package ledger

import (
	pb "ember/proto"
	chainapp "ember/services/chain"
	"errors"
)

// EvidenceFrameLimit reads the immutable, governed genesis parameter.
func (l *ChainLedger) EvidenceFrameLimit() (uint32, error) {
	var p chainapp.Parameters
	if err := l.query("/parameters", &p); err != nil {
		return 0, err
	}
	return p.Evidence.FrameTokens(), nil
}

func (l *ChainLedger) EvidenceAccess(id, requester string) (*pb.EvidenceManifest, uint32, error) {
	hold, err := l.hold(id)
	if err != nil {
		return nil, 0, err
	}
	if hold.EvidenceManifest == nil || (requester != hold.Auditor && requester != hold.Node && requester != hold.Payer) {
		return nil, 0, errors.New("not an evidence counterparty")
	}
	limit, err := l.EvidenceFrameLimit()
	return hold.EvidenceManifest, limit, err
}
