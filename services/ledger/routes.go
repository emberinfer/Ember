package ledger

import (
	"context"
	"crypto/sha256"
	core "ember/core-go"
	pb "ember/proto"
	chainapp "ember/services/chain"
	"encoding/hex"
	"errors"
	"google.golang.org/protobuf/proto"
)

// AuditRoute assigns on-chain first, then resolves the assigned identity through
// its finalized signed profile. No peer-supplied address or operator map is used.
func (l *ChainLedger) AuditRoute(id string) (*chainapp.Hold, *pb.ResourceProfile, *pb.ResourceProfile, error) {
	h, err := l.hold(id)
	if err != nil {
		return nil, nil, nil, err
	}
	if h.Auditor == "" {
		if err = l.submit(l.coordinator, chainapp.Action{Type: "assign_audit", HoldID: id}); err != nil {
			return nil, nil, nil, err
		}
	}
	var route chainapp.AuditRouteView
	if err = l.query("/audit-route/"+id, &route); err != nil {
		return nil, nil, nil, err
	}
	if route.Hold.State != "dispute" || !route.Eligible {
		return nil, nil, nil, errors.New("assigned auditor lacks active independent replay registration")
	}
	return route.Hold, route.Producer, route.Auditor, nil
}

func (l *ChainLedger) RecordRemoteAudit(id string, raw []byte) error {
	h, _, _, err := l.AuditRoute(id)
	if err != nil {
		return err
	}
	env, signer, err := chainapp.DecodeAndVerify(raw)
	if err != nil || signer != h.Auditor || env.ChainID != l.chainID || env.Action.Type != "audit" || env.Action.HoldID != id {
		return errors.New("invalid remote audit envelope")
	}
	c, v := &pb.AuditChallenge{}, &pb.AuditVerdict{}
	if proto.Unmarshal(env.Action.Challenge, c) != nil || proto.Unmarshal(env.Action.Verdict, v) != nil || core.Verify(c) != nil || core.Verify(v) != nil || core.NodeID(c.Signer) != h.Auditor || core.NodeID(v.Signer) != h.Auditor {
		return errors.New("unregistered or unassigned audit signer")
	}
	proof, err := hex.DecodeString(h.AuditProof)
	if err != nil || len(proof) != sha256.Size || string(proof) != string(c.VrfProof) {
		return errors.New("remote audit proof mismatch")
	}
	_, err = broadcastConfirm(context.Background(), l.rpc, raw)
	return err
}

// RecordAdjudicationCommit broadcasts the assigned third auditor's commitment
// before the routed audit reveal can be submitted.
func (l *ChainLedger) RecordAdjudicationCommit(id string, raw []byte) error {
	h, _, _, err := l.AuditRoute(id)
	if err != nil {
		return err
	}
	env, signer, err := chainapp.DecodeAndVerify(raw)
	if err != nil || h.Adjudication == nil || h.Adjudication.Phase != "third_commit" || signer != h.Adjudication.ThirdAuditor || env.ChainID != l.chainID || env.Action.Type != "commit_third_adjudication" || env.Action.HoldID != id {
		return errors.New("invalid remote adjudication commitment envelope")
	}
	_, err = broadcastConfirm(context.Background(), l.rpc, raw)
	return err
}
