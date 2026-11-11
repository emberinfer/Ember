package ledger

import (
	"bytes"
	"crypto/ed25519"
	"path/filepath"
	"testing"
	"time"

	core "ember/core-go"
	pb "ember/proto"
)

func signedStage(t *testing.T, seed byte, id string, step, start, end uint32) *pb.ShardStepReceipt {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, 32))
	receipt := &pb.ShardStepReceipt{NodeId: core.NodeID(key.Public().(ed25519.PublicKey)),
		ChainId: "ember-p", ManifestRoot: bytes.Repeat([]byte{1}, 32), ExecutionProfileVer: "p",
		RequestId: id, Step: step, StartLayer: start, EndLayer: end,
		RequestHash: bytes.Repeat([]byte{2}, 32), ResponseHash: bytes.Repeat([]byte{3}, 32),
		ParametersHash: bytes.Repeat([]byte{4}, 32)}
	if err := core.Sign(receipt, key); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func stageID(seed byte) string {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, 32))
	return core.NodeID(key.Public().(ed25519.PublicKey))
}

func TestPipelineSettlementSplitsVerifiedWork(t *testing.T) {
	book, err := Open(filepath.Join(t.TempDir(), "pipeline.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	ids := []string{"consumer", stageID(1), stageID(2), stageID(3), stageID(4)}
	if err := book.Create(ids[0], 1000); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids[1:] {
		if err := book.Create(id, 100000); err != nil {
			t.Fatal(err)
		}
		if err := book.Stake(id, 50000); err != nil {
			t.Fatal(err)
		}
	}
	auth := []StageAuthorization{{ids[1], 0, 9}, {ids[2], 0, 9}, {ids[3], 9, 18}, {ids[4], 18, 28}}
	if err := book.FreezePipeline("request", "consumer", 56, auth, 28); err != nil {
		t.Fatal(err)
	}
	receipt := &pb.PipelineExecutionReceipt{RequestId: "request", OutputCommitment: bytes.Repeat([]byte{5}, 32), InputTokens: 2, OutputTokens: 2, ElapsedMs: 100}
	receipt.StageReceipts = append(receipt.StageReceipts,
		signedStage(t, 1, "request", 0, 0, 9), signedStage(t, 3, "request", 0, 9, 18), signedStage(t, 4, "request", 0, 18, 28),
		signedStage(t, 2, "request", 1, 0, 9), signedStage(t, 3, "request", 1, 9, 18), signedStage(t, 4, "request", 1, 18, 28))
	shares, err := book.SubmitPipeline("request", receipt, 56, 28, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{ids[1]: 9, ids[2]: 9, ids[3]: 18, ids[4]: 20}
	for _, share := range shares {
		if share.Amount != want[share.NodeID] {
			t.Fatalf("share=%+v want=%d", share, want[share.NodeID])
		}
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))
	ref, _ := pipelineReceiptRef(receipt)
	challenge := &pb.AuditChallenge{ReceiptRef: ref, Positions: []uint32{0, 1}}
	_ = core.Sign(challenge, key)
	challengeRef, _ := core.Digest(challenge)
	verdict := &pb.AuditVerdict{ChallengeRef: challengeRef, AuditorId: "full-model", Verdict: "accept"}
	_ = core.Sign(verdict, key)
	if err := book.RecordPipelineAudit("request", challenge, verdict); err != nil {
		t.Fatal(err)
	}
	if err := book.FinishPipeline("request", "accept", time.Now(), 20); err != nil {
		t.Fatal(err)
	}
	if balance, _, err := book.Balance("consumer"); err != nil || balance != 944 {
		t.Fatalf("consumer=%d err=%v", balance, err)
	}
	for id, amount := range want {
		balance, stake, err := book.Balance(id)
		if err != nil || balance != 50000+amount || stake != 50000 {
			t.Fatalf("node=%s balance=%d stake=%d err=%v", id, balance, stake, err)
		}
	}
}

func TestRecoverFrozenPipeline(t *testing.T) {
	book, err := Open(filepath.Join(t.TempDir(), "recover-pipeline.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	_ = book.Create("consumer", 100)
	_ = book.Create("stage", 100)
	_ = book.Stake("stage", 50)
	if err := book.FreezePipeline("pending", "consumer", 10,
		[]StageAuthorization{{"stage", 0, 1}}, 1); err != nil {
		t.Fatal(err)
	}
	if count, err := book.RecoverFrozen(); err != nil || count != 1 {
		t.Fatalf("recover=%d err=%v", count, err)
	}
	if state, err := book.State("pending"); err != nil || state != "cancelled" {
		t.Fatalf("state=%s err=%v", state, err)
	}
}

func TestPipelineRejectSlashesOnlyAttributedStage(t *testing.T) {
	book, err := Open(filepath.Join(t.TempDir(), "precise-slash.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	consumer, culprit, innocent := "consumer", stageID(1), stageID(2)
	if err = book.Create(consumer, 1000); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{culprit, innocent} {
		if err = book.Create(id, 1000); err != nil {
			t.Fatal(err)
		}
		if err = book.Stake(id, 500); err != nil {
			t.Fatal(err)
		}
	}
	if err = book.FreezePipeline("reject", consumer, 20, []StageAuthorization{
		{NodeID: culprit, StartLayer: 0, EndLayer: 1},
		{NodeID: innocent, StartLayer: 1, EndLayer: 2},
	}, 2); err != nil {
		t.Fatal(err)
	}
	receipt := &pb.PipelineExecutionReceipt{RequestId: "reject", OutputCommitment: bytes.Repeat([]byte{5}, 32), InputTokens: 1, OutputTokens: 1, ElapsedMs: 1,
		StageReceipts: []*pb.ShardStepReceipt{signedStage(t, 1, "reject", 0, 0, 1), signedStage(t, 2, "reject", 0, 1, 2)}}
	if _, err = book.SubmitPipeline("reject", receipt, 20, 2, 0); err != nil {
		t.Fatal(err)
	}
	auditKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))
	ref, _ := pipelineReceiptRef(receipt)
	challenge := &pb.AuditChallenge{ReceiptRef: ref, Positions: []uint32{0}}
	if err = core.Sign(challenge, auditKey); err != nil {
		t.Fatal(err)
	}
	challengeRef, _ := core.Digest(challenge)
	verdict := &pb.AuditVerdict{ChallengeRef: challengeRef, AuditorId: "full-model", Verdict: "reject", FaultNodeIds: []string{culprit}}
	if err = core.Sign(verdict, auditKey); err != nil {
		t.Fatal(err)
	}
	if err = book.RecordPipelineAudit("reject", challenge, verdict); err != nil {
		t.Fatal(err)
	}
	if err = book.FinishPipeline("reject", "reject", time.Now(), 20); err != nil {
		t.Fatal(err)
	}
	_, culpritStake, _ := book.Balance(culprit)
	_, innocentStake, _ := book.Balance(innocent)
	if culpritStake != 300 || innocentStake != 500 {
		t.Fatalf("culprit stake=%d innocent stake=%d", culpritStake, innocentStake)
	}
}
