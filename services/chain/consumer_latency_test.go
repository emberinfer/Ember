package chain

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"testing"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

func consumerLatencyFixture(t *testing.T) (*State, map[string]actor, []actor, []PipelineAuthorization, *pb.PipelineExecutionReceipt) {
	t.Helper()
	state, actors, nodes, auth, receipt := pipelineState(t)
	state.Auditors = map[string]bool{actors["auditor"].id: true}
	state.Parameters.Scheduling = &core.SchedulingPolicy{Scoring: "auditor-roundtrip-us-per-unit-v1", WitnessAuthority: "assigned-auditor", ExplorationPPM: 200000, ExplorationPeriod: 10, PercentilePPM: 900000, WindowSamples: 8, MinSamples: 2, MaxObservationNS: 1000000000,
		ConsumerCrossCheck: &core.ConsumerCrossCheckPolicy{MaxRatioPPM: 4000000, MinSamples: 4, DivergencePPM: 750000}}
	return state, actors, nodes, auth, receipt
}

func prepareConsumerHold(t *testing.T, state *State, actors map[string]actor, nodes []actor, auth []PipelineAuthorization, template *pb.PipelineExecutionReceipt, index int) (*Hold, *ConsumerLatencyObservation) {
	t.Helper()
	receipt := proto.Clone(template).(*pb.PipelineExecutionReceipt)
	height := state.Height + 1
	receipt.RequestId = fmt.Sprintf("consumer-%d", index)
	for position, stage := range receipt.StageReceipts {
		stage.RequestId = receipt.RequestId
		core.Sign(stage, nodes[position].key)
		ref, _ := core.Digest(stage)
		witness := &pb.StageLatencyWitness{StageReceiptRef: ref, StartedMonoNs: 1000000, CompletedMonoNs: 1040000}
		core.Sign(witness, actors["auditor"].key)
		receipt.LatencyWitnesses = append(receipt.LatencyWitnesses, witness)
	}
	apply(t, state, actors["payer"], state.Accounts[actors["payer"].id].Nonce+1, Action{Type: "freeze_pipeline", HoldID: receipt.RequestId, Payer: actors["payer"].id, Amount: 6, TotalLayers: 4, PipelineAuthorizations: auth}, height)
	raw, _ := proto.Marshal(receipt)
	apply(t, state, actors["coordinator"], state.Accounts[actors["coordinator"].id].Nonce+1, Action{Type: "submit_pipeline", HoldID: receipt.RequestId, Actual: 6, Receipt: raw}, height)
	hold := state.Holds[receipt.RequestId]
	observation := &ConsumerLatencyObservation{ReceiptRef: hold.ReceiptRef, ObservedNS: 10000000}
	for _, witness := range receipt.LatencyWitnesses {
		observation.Stages = append(observation.Stages, ConsumerStageObservation{StageReceiptRef: hex.EncodeToString(witness.StageReceiptRef), ObservedNS: 45000})
	}
	return hold, observation
}

func acceptConsumerHold(t *testing.T, state *State, actors map[string]actor, hold *Hold) {
	t.Helper()
	height := state.Height + 1
	apply(t, state, actors["coordinator"], state.Accounts[actors["coordinator"].id].Nonce+1, Action{Type: "assign_audit", HoldID: hold.ID}, height)
	hold = state.Holds[hold.ID]
	ref, _ := hex.DecodeString(hold.ReceiptRef)
	proof, _ := hex.DecodeString(hold.AuditProof)
	challenge := &pb.AuditChallenge{ReceiptRef: ref, VrfProof: proof}
	core.Sign(challenge, actors["auditor"].key)
	challengeRef, _ := core.Digest(challenge)
	verdict := &pb.AuditVerdict{ChallengeRef: challengeRef, AuditorId: actors["auditor"].id, Verdict: "accept", EvidenceRef: bytes.Repeat([]byte{7}, 32)}
	core.Sign(verdict, actors["auditor"].key)
	challengeRaw, _ := proto.Marshal(challenge)
	verdictRaw, _ := proto.Marshal(verdict)
	apply(t, state, actors["auditor"], state.Accounts[actors["auditor"].id].Nonce+1, Action{Type: "audit", HoldID: hold.ID, Challenge: challengeRaw, Verdict: verdictRaw}, height)
	apply(t, state, actors["coordinator"], state.Accounts[actors["coordinator"].id].Nonce+1, Action{Type: "settle", HoldID: hold.ID, Outcome: "accept"}, height+1)
}

func TestConsumerObservationRequiresPayerSignatureAndExactReceipt(t *testing.T) {
	state, actors, nodes, auth, receipt := consumerLatencyFixture(t)
	hold, observation := prepareConsumerHold(t, state, actors, nodes, auth, receipt, 0)
	action := Action{Type: "observe_latency", HoldID: hold.ID, ConsumerLatency: observation}
	rejected(t, state, actors["auditor"], action, 2)
	rejected(t, state, actors["coordinator"], action, 2)
	observation.ReceiptRef = "wrong-receipt"
	rejected(t, state, actors["payer"], action, 2)
	observation.ReceiptRef = hold.ReceiptRef
	observation.Stages[0].StageReceiptRef = "wrong-stage"
	rejected(t, state, actors["payer"], action, 2)
	observation.Stages[0].StageReceiptRef = hex.EncodeToString(receipt.StageReceipts[0].RequestHash)
	rejected(t, state, actors["payer"], action, 2)
	committed := &pb.PipelineExecutionReceipt{}
	proto.Unmarshal(hold.Pipeline.Receipt, committed)
	ref, _ := core.Digest(committed.StageReceipts[0])
	observation.Stages[0].StageReceiptRef = hex.EncodeToString(ref)
	observation.Stages[0].ObservedNS = observation.ObservedNS + 1
	rejected(t, state, actors["payer"], action, 2)
	observation.Stages[0].ObservedNS = 45000
	apply(t, state, actors["payer"], state.Accounts[actors["payer"].id].Nonce+1, action, 2)
	if len(state.Latencies) != 0 {
		t.Fatal("unsettled consumer observations changed scheduling")
	}
	acceptConsumerHold(t, state, actors, hold)
}

func TestConsumerDivergenceReplacesOnlyColludingPairAndRecovers(t *testing.T) {
	state, actors, nodes, auth, receipt := consumerLatencyFixture(t)
	for index := 0; index < 12; index++ {
		hold, observation := prepareConsumerHold(t, state, actors, nodes, auth, receipt, index)
		if index < 4 {
			observation.Stages[1].ObservedNS = 4000000
		}
		raw := apply(t, state, actors["payer"], state.Accounts[actors["payer"].id].Nonce+1, Action{Type: "observe_latency", HoldID: hold.ID, ConsumerLatency: observation}, state.Height)
		stored := state.Holds[hold.ID].ConsumerLatency
		if !bytes.Equal(stored.SignedTransaction, raw) || stored.Observer != actors["payer"].id {
			t.Fatal("consumer signature provenance missing")
		}
		if _, _, err := DecodeAndVerify(stored.SignedTransaction); err != nil {
			t.Fatal(err)
		}
		rejected(t, state, actors["payer"], Action{Type: "observe_latency", HoldID: hold.ID, ConsumerLatency: observation}, state.Height)
		acceptConsumerHold(t, state, actors, hold)
		if index == 3 {
			slow := state.Latencies[LatencyKey(nodes[1].id, "ember-unit-test")]
			if slow.AuditorPercentileMicrosPerUnit != 10 || slow.PercentileMicrosPerUnit != 1000 || len(slow.Divergence) != 1 || !slow.Divergence[0].Divergent {
				t.Fatalf("collusion not excluded: %+v", slow)
			}
			for _, node := range []actor{nodes[0], nodes[2]} {
				control := state.Latencies[LatencyKey(node.id, "ember-unit-test")]
				if control.PercentileMicrosPerUnit != 40 || control.Divergence[0].Divergent {
					t.Fatal("honest control changed", control)
				}
			}
		}
	}
	recovered := state.Latencies[LatencyKey(nodes[1].id, "ember-unit-test")]
	if recovered.Divergence[0].Divergent || recovered.PercentileMicrosPerUnit != 10 || len(recovered.Window) != 8 {
		t.Fatal("rolling-window recovery failed", recovered)
	}
}

func TestConsumerWholeModelObservationProvenance(t *testing.T) {
	state, actors := testState(t)
	apply(t, state, actors["payer"], 1, Action{Type: "freeze", HoldID: "whole-latency", Payer: actors["payer"].id, Node: actors["node"].id, Amount: 5}, 1)
	receipt := signedReceipt(t, "whole-latency", actors["node"])
	apply(t, state, actors["coordinator"], 1, Action{Type: "submit", HoldID: "whole-latency", Actual: 1, Receipt: receipt}, 2)
	observation := &ConsumerLatencyObservation{ReceiptRef: state.Holds["whole-latency"].ReceiptRef, ObservedNS: 1000}
	action := Action{Type: "observe_latency", HoldID: "whole-latency", ConsumerLatency: observation}
	observation.Observer = actors["payer"].id
	rejected(t, state, actors["payer"], action, 2)
	observation.Observer = ""
	observation.Stages = []ConsumerStageObservation{{StageReceiptRef: "invented", ObservedNS: 1}}
	rejected(t, state, actors["payer"], action, 2)
	observation.Stages = nil
	raw := apply(t, state, actors["payer"], 2, action, 2)
	stored := state.Holds["whole-latency"].ConsumerLatency
	if !bytes.Equal(raw, stored.SignedTransaction) || stored.Observer != actors["payer"].id || len(state.Latencies) != 0 {
		t.Fatal("whole-model consumer provenance or scheduling isolation failed")
	}
}

func TestConsumerEnabledPolicyRequiresIndependentObservationAtSettlement(t *testing.T) {
	state, actors, nodes, auth, receipt := consumerLatencyFixture(t)
	state.Auditors[actors["payer"].id] = true
	hold, observation := prepareConsumerHold(t, state, actors, nodes, auth, receipt, 0)
	apply(t, state, actors["coordinator"], state.Accounts[actors["coordinator"].id].Nonce+1, Action{Type: "assign_audit", HoldID: hold.ID}, state.Height+1)
	hold = state.Holds[hold.ID]
	if hold.Auditor == actors["payer"].id {
		t.Fatal("payer selected as its own cross-check auditor")
	}
	ref, _ := hex.DecodeString(hold.ReceiptRef)
	proof, _ := hex.DecodeString(hold.AuditProof)
	challenge := &pb.AuditChallenge{ReceiptRef: ref, VrfProof: proof}
	core.Sign(challenge, actors["auditor"].key)
	challengeRef, _ := core.Digest(challenge)
	verdict := &pb.AuditVerdict{ChallengeRef: challengeRef, AuditorId: actors["auditor"].id, Verdict: "accept", EvidenceRef: bytes.Repeat([]byte{8}, 32)}
	core.Sign(verdict, actors["auditor"].key)
	challengeRaw, _ := proto.Marshal(challenge)
	verdictRaw, _ := proto.Marshal(verdict)
	apply(t, state, actors["auditor"], 1, Action{Type: "audit", HoldID: hold.ID, Challenge: challengeRaw, Verdict: verdictRaw}, state.Height)
	rejected(t, state, actors["coordinator"], Action{Type: "settle", HoldID: hold.ID, Outcome: "accept"}, state.Height+1)
	apply(t, state, actors["payer"], state.Accounts[actors["payer"].id].Nonce+1, Action{Type: "observe_latency", HoldID: hold.ID, ConsumerLatency: observation}, state.Height)
	apply(t, state, actors["coordinator"], state.Accounts[actors["coordinator"].id].Nonce+1, Action{Type: "settle", HoldID: hold.ID, Outcome: "accept"}, state.Height+1)
}

func TestConsumerPolicyBoundsAndOverflowSafeRatio(t *testing.T) {
	state, _, _, _, _ := consumerLatencyFixture(t)
	policy := state.Parameters.Scheduling
	for _, invalid := range []core.ConsumerCrossCheckPolicy{
		{MaxRatioPPM: 1000000, MinSamples: 1, DivergencePPM: 1},
		{MaxRatioPPM: 100000001, MinSamples: 1, DivergencePPM: 1},
		{MaxRatioPPM: 4000000, MinSamples: 0, DivergencePPM: 1},
		{MaxRatioPPM: 4000000, MinSamples: 9, DivergencePPM: 1},
		{MaxRatioPPM: 4000000, MinSamples: 1, DivergencePPM: 0},
		{MaxRatioPPM: 4000000, MinSamples: 1, DivergencePPM: 1000001},
	} {
		policy.ConsumerCrossCheck = &invalid
		if policy.Validate() == nil {
			t.Fatal("invalid cross-check policy accepted", invalid)
		}
	}
	if latencyDisagrees(1, 4, 4000000) || !latencyDisagrees(1, 5, 4000000) || !latencyDisagrees(math.MaxUint64, 1, 4000000) || latencyDisagrees(math.MaxUint64, math.MaxUint64-1, 4000000) {
		t.Fatal("ratio boundary or overflow comparison failed")
	}
}
