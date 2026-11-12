package chain

import (
	"bytes"
	"crypto/sha256"
	core "ember/core-go"
	pb "ember/proto"
	"encoding/hex"
	"encoding/json"
	"google.golang.org/protobuf/proto"
	"testing"
)

func pricedFixture(t *testing.T) (*State, map[string]actor, []actor, []PipelineAuthorization, *pb.PipelineExecutionReceipt) {
	s, a, n, auth, r := pipelineState(t)
	m := s.Models["ember-unit-test"]
	g := &pb.ChainGenesis{}
	proto.Unmarshal(m.Genesis, g)
	g.PriceInput, g.PriceOutput, g.PriceContext = 2, 3, 1
	core.Sign(g, a["governor"].key)
	m.Genesis, _ = proto.MarshalOptions{Deterministic: true}.Marshal(g)
	h := sha256.Sum256(m.Genesis)
	m.GenesisHash = hex.EncodeToString(h[:])
	s.Models[m.ID] = m
	return s, a, n, auth, r
}

func tokenCommitment(t *testing.T, signer actor, input, max uint32) []byte {
	c := &pb.ExecutionCommitment{RequestId: "units", ChainId: "ember-unit-test", ProfileVer: "units-test", InputTokens: input, MaxOutputTokens: max, InputCommitment: bytes.Repeat([]byte{5}, 32)}
	if err := core.Sign(c, signer.key); err != nil {
		t.Fatal(err)
	}
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	return b
}

func TestPricedPipelineChecksFreezeReceiptAndFinalSettlement(t *testing.T) {
	s, a, _, auth, r := pricedFixture(t)
	c := tokenCommitment(t, a["coordinator"], 2, 3)
	f := Action{Type: "freeze_pipeline", HoldID: "units", Payer: a["payer"].id, Amount: 13, TotalLayers: 4, PipelineAuthorizations: auth, PricingCommitment: c}
	bad := f
	bad.PricingCommitment = nil
	rejected(t, s, a["payer"], bad, 2)
	bad = f
	bad.Amount--
	rejected(t, s, a["payer"], bad, 2)
	apply(t, s, a["payer"], 1, f, 2)
	raw, _ := proto.Marshal(r)
	bad = Action{Type: "submit_pipeline", HoldID: "units", Actual: 8, Receipt: raw}
	rejected(t, s, a["coordinator"], bad, 3)
	r.InputTokens = 3
	bad.Receipt, _ = proto.Marshal(r)
	bad.Actual = 7
	rejected(t, s, a["coordinator"], bad, 3)
	r.InputTokens = 2
	bad.Receipt, _ = proto.Marshal(r)
	apply(t, s, a["coordinator"], 1, bad, 3)
	h := s.Holds["units"]
	var sum int64
	for _, v := range h.Pipeline.Credits {
		sum += v.Amount
	}
	if sum != 7 {
		t.Fatal("fee split lost subunits")
	}
	if err := verifyFrozenSettlement(h); err != nil {
		t.Fatal(err)
	}
	h.Actual++
	if verifyFrozenSettlement(h) == nil {
		t.Fatal("settlement failed to recompute")
	}
}

func TestPricedSingleBindsServingCountAndReceiptCommitment(t *testing.T) {
	s, a, n, _, _ := pricedFixture(t)
	c := tokenCommitment(t, n[0], 33, 100)
	f := Action{Type: "freeze", HoldID: "units", Payer: a["payer"].id, Node: n[0].id, Amount: 369, PricingCommitment: c}
	apply(t, s, a["payer"], 1, f, 2)
	r := &pb.ExecutionReceipt{}
	proto.Unmarshal(signedReceipt(t, "units", n[0]), r)
	r.InputTokens = 33
	ref := sha256.Sum256(c)
	r.CommitmentRef = ref[:]
	core.Sign(r, n[0].key)
	raw, _ := proto.Marshal(r)
	bad := Action{Type: "submit", HoldID: "units", Actual: 70, Receipt: raw}
	rejected(t, s, a["coordinator"], bad, 3)
	bad.Actual = 69
	r.InputTokens = 32
	core.Sign(r, n[0].key)
	bad.Receipt, _ = proto.Marshal(r)
	rejected(t, s, a["coordinator"], bad, 3)
	r.InputTokens = 33
	r.CommitmentRef = nil
	core.Sign(r, n[0].key)
	bad.Receipt, _ = proto.Marshal(r)
	rejected(t, s, a["coordinator"], bad, 3)
	r.CommitmentRef = bytes.Repeat([]byte{9}, 32)
	core.Sign(r, n[0].key)
	bad.Receipt, _ = proto.Marshal(r)
	rejected(t, s, a["coordinator"], bad, 3)
	bad.Receipt = raw
	apply(t, s, a["coordinator"], 1, bad, 3)
	apply(t, s, a["coordinator"], 2, Action{Type: "assign_audit", HoldID: "units"}, 4)
	auditor, proof := assignedActor(t, s, a, "units")
	challenge, verdict, _ := signedAudit(t, raw, auditor, "accept", proof)
	apply(t, s, auditor, 1, Action{Type: "audit", HoldID: "units", Challenge: challenge, Verdict: verdict}, 5)
	rejected(t, s, a["coordinator"], Action{Type: "settle", HoldID: "units", Outcome: "accept", Actual: 70}, 6)
	apply(t, s, a["coordinator"], 3, Action{Type: "settle", HoldID: "units", Outcome: "accept"}, 6)
	if s.Accounts[a["payer"].id].Balance != 931 || s.Accounts[n[0].id].Balance != 69 || s.SubsidyIssued != 0 {
		t.Fatal("refund or conservation failed")
	}
}

func TestLegacyHoldAndActionJSONUnchanged(t *testing.T) {
	s, a := testState(t)
	apply(t, s, a["payer"], 1, Action{Type: "freeze", HoldID: "legacy", Payer: a["payer"].id, Node: a["node"].id, Amount: 100}, 1)
	raw, _ := json.Marshal(s.Holds["legacy"])
	if bytes.Contains(raw, []byte("token_fee")) {
		t.Fatal("legacy hold bytes changed")
	}
	raw, _ = json.Marshal(Action{Type: "freeze"})
	if bytes.Contains(raw, []byte("pricing_commitment")) {
		t.Fatal("legacy tx bytes changed")
	}
}

func TestOutputOnlyAnchoredProfileStillVerifiesExactLegacyFee(t *testing.T) {
	s, a, n, _, _ := pricedFixture(t)
	m := s.Models["ember-unit-test"]
	g := &pb.ChainGenesis{}
	proto.Unmarshal(m.Genesis, g)
	g.PriceInput, g.PriceContext, g.PriceOutput = 0, 0, 7
	core.Sign(g, a["governor"].key)
	m.Genesis, _ = proto.Marshal(g)
	s.Models[m.ID] = m
	apply(t, s, a["payer"], 1, Action{Type: "freeze", HoldID: "units", Payer: a["payer"].id, Node: n[0].id, Amount: 21}, 2)
	raw := signedReceipt(t, "units", n[0])
	rejected(t, s, a["coordinator"], Action{Type: "submit", HoldID: "units", Actual: 8, Receipt: raw}, 3)
	apply(t, s, a["coordinator"], 1, Action{Type: "submit", HoldID: "units", Actual: 7, Receipt: raw}, 3)
	if err := verifyFrozenSettlement(s.Holds["units"]); err != nil {
		t.Fatal(err)
	}
	parsed := &pb.ExecutionReceipt{}
	proto.Unmarshal(raw, parsed)
	encoded, _ := proto.MarshalOptions{Deterministic: true}.Marshal(parsed)
	if !bytes.Equal(raw, encoded) {
		t.Fatal("legacy receipt bytes changed")
	}
}
