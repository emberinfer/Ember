package ledger

import (
	"context"
	"crypto/sha256"
	core "ember/core-go"
	pb "ember/proto"
	chainapp "ember/services/chain"
	"encoding/hex"
	"errors"
	"fmt"
	"google.golang.org/protobuf/proto"
	"time"
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
	var s chainapp.State
	if err = l.query("/state", &s); err != nil {
		return nil, nil, nil, err
	}
	h = s.Holds[id]
	if h == nil {
		return nil, nil, nil, errors.New("unknown hold")
	}
	producer := s.ActiveProfile(h.Node, s.Height)
	if len(h.ProducerProfile) > 0 {
		producer = &pb.ResourceProfile{}
		if proto.Unmarshal(h.ProducerProfile, producer) != nil {
			return nil, nil, nil, errors.New("invalid frozen profile")
		}
	}
	if h.State != "dispute" || !s.EligibleAuditorProfile(h.Auditor, h.Node, producer, s.Height) {
		return nil, nil, nil, errors.New("assigned auditor lacks active independent replay registration")
	}
	return h, producer, s.ActiveProfile(h.Auditor, s.Height), nil
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := l.rpc.BroadcastTxCommit(ctx, raw)
	if err != nil {
		return err
	}
	if result.CheckTx.Code != 0 || result.TxResult.Code != 0 {
		return fmt.Errorf("remote audit rejected: %s %s", result.CheckTx.Log, result.TxResult.Log)
	}
	return nil
}
