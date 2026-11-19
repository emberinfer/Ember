package chain

import (
	"bytes"
	core "ember/core-go"
	pb "ember/proto"
	"encoding/hex"
	"encoding/json"
	"google.golang.org/protobuf/proto"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSpotPythonGatewayChainAuditor(t *testing.T) {
	s, _, _, h := spotFixture(t)
	g := gatewayFixture(t, s)
	c := h.Pipeline.Spot
	python := os.Getenv("EMBER_AUDIT_TEST_PYTHON")
	if python == "" {
		t.Fatal("EMBER_AUDIT_TEST_PYTHON required for cross-line gate")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]any{"seed": hex.EncodeToString(g.key.Seed()), "request_id": c.RequestID, "challenge_height": c.ChallengeHeight, "steps": c.StepSet, "deadline": c.Deadline, "height": c.Deadline + 1})
	cmd := exec.Command(python, "node/spot_integration_peer.py")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(root, "node")+":"+filepath.Join(root, "proto")+":"+filepath.Join(root, "core-py"))
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("peer: %v %s", err, out)
	}
	var result struct {
		Action  Action           `json:"action"`
		Receipt SpoolAttestation `json:"receipt"`
	}
	if err = json.Unmarshal(out, &result); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if err = s.verifySpool(h, result.Action.Spool, c.Deadline+1); err != nil {
		t.Fatal(err)
	}
	if err = s.verifySpool(h, &result.Receipt, c.Deadline+1); err != nil {
		t.Fatal(err)
	}
	if err = s.spotAction(h.Auditor, result.Action, c.Deadline+1); err != nil {
		t.Fatal(err)
	}
	if c.Accusation == nil {
		t.Fatal("C accusation was not accepted")
	}
}

func TestSpotRejectWithoutLegacyGradeAndSuperset(t *testing.T) {
	s, a, _, h := spotFixture(t)
	// A three-step committed execution: first review challenges step one, full
	// review corroborates one and additionally finds two. Neither verdict lies
	// about the retired offense band or node-fault set.
	h.Pipeline.SpotStepCount = 3
	h.Pipeline.Spot.StepSet = []uint64{1}
	for round := 1; round <= 2; round++ {
		aud := a["auditor"]
		if aud.id != h.Auditor {
			aud = a["auditor2"]
		}
		v := &pb.AuditVerdict{}
		_ = proto.Unmarshal(spotVerdict(h, true), v)
		if round == 2 {
			for _, p := range v.PerStep {
				if p.Step == 3 {
					p.Outcome = "match"
					p.FirstDivergenceDistance = 0
				}
			}
		}
		ref, _ := hex.DecodeString(h.ReceiptRef)
		proof, _ := hex.DecodeString(h.AuditProof)
		challenge := &pb.AuditChallenge{ReceiptRef: ref, VrfProof: proof}
		_ = core.Sign(challenge, aud.key)
		v.ChallengeRef, _ = core.Digest(challenge)
		v.AuditorId = aud.id
		v.EvidenceRef = bytes.Repeat([]byte{7}, 32)
		_ = core.Sign(v, aud.key)
		cb, _ := proto.Marshal(challenge)
		vb, _ := proto.Marshal(v)
		raw, err := Sign(s.Parameters.ChainID, aud.key, s.Accounts[aud.id].Nonce+1, Action{Type: "audit", HoldID: h.ID, Challenge: cb, Verdict: vb})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Apply(raw, int64(4+round)); err != nil {
			t.Fatal(err)
		}
		h = s.Holds[h.ID]
		if round == 1 && (h.Pipeline.Spot.Round != "esc" || h.AuditVerdict != "") {
			t.Fatal("first reject did not escalate")
		}
	}
	if h.AuditVerdict != "reject" {
		t.Fatal("superset did not corroborate")
	}
}

func TestSpotBoundExecutionCount(t *testing.T) {
	_, _, _, h := spotFixture(t)
	if h.Pipeline.SpotStepCount != 6 || len(h.Pipeline.Spot.StepSet) != 1 || h.Pipeline.Spot.StepSet[0] > 6 {
		t.Fatal("challenge not bound to signed submission execution count")
	}
	if h.Pipeline.Spot.RequestID != h.ID {
		t.Fatal("challenge request identity missing")
	}
}

func TestSpotSubmissionCountMatchesReceipt(t *testing.T) {
	for _, count := range []uint64{0, 2, 62, 1000001} {
		s, a, _, auth, r := pipelineState(t)
		s.Version = 14
		apply(t, s, a["payer"], 1, Action{Type: "freeze_pipeline", HoldID: "units", Payer: a["payer"].id, Amount: 6, TotalLayers: 4, PipelineAuthorizations: auth}, 2)
		raw, _ := proto.Marshal(r)
		rejected(t, s, a["coordinator"], Action{Type: "submit_pipeline", HoldID: "units", Actual: 6, Receipt: raw, SpotStepCount: count, CommitmentRoot: bytes.Repeat([]byte{1}, 32)}, 3)
		if s.Holds["units"].State != "frozen" {
			t.Fatal("failed count changed hold")
		}
		apply(t, s, a["coordinator"], 1, Action{Type: "submit_pipeline", HoldID: "units", Actual: 6, Receipt: raw, SpotStepCount: 1, CommitmentRoot: bytes.Repeat([]byte{1}, 32)}, 3)
		if s.Holds["units"].Pipeline.SpotStepCount != 1 {
			t.Fatal("submission count not frozen")
		}
	}
}
