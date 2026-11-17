package chain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

func TestFreezeIdempotency(t *testing.T) {
	s, actors := testState(t)
	payer := actors["payer"]
	rawPayer, _ := hex.DecodeString(payer.id)
	a := Action{Type: "freeze", HoldID: "first", Payer: payer.id, Node: actors["node"].id, Amount: 100, IdempotencyKey: IdempotencyKey(rawPayer, []byte("client-nonce"))}
	apply(t, s, payer, 1, a, 1)
	balance := s.Accounts[payer.id].Balance
	// Clone exercises the persisted JSON state rather than a process-local cache.
	s, _ = s.Clone()
	a.HoldID = "retry"
	apply(t, s, payer, 2, a, 2)
	if len(s.Holds) != 1 || s.Accounts[payer.id].Balance != balance {
		t.Fatal("replay created a second debit/hold")
	}
	if s.Holds["first"].IdempotencyKey != a.IdempotencyKey {
		t.Fatal("key not bound to hold")
	}
	a.Amount++
	raw, _ := Sign(s.Parameters.ChainID, payer.key, 2, a)
	if _, err := s.Apply(raw, 2); err == nil || !strings.Contains(err.Error(), "different payload") {
		t.Fatalf("expected payload conflict: %v", err)
	}
	a.Amount--
	a.IdempotencyKey = IdempotencyKey(rawPayer, []byte("independent"))
	apply(t, s, payer, 2, a, 2)
	if len(s.Holds) != 2 || s.Accounts[payer.id].Balance != balance-100 {
		t.Fatal("different key did not create independent hold/debit")
	}
}

func TestFreezeIdempotencyReroute(t *testing.T) {
	s, actors := testState(t)
	payer := actors["payer"]
	a := Action{Type: "freeze", HoldID: "original", Payer: payer.id, Node: actors["node"].id, Amount: 100, IdempotencyKey: IdempotencyKey([]byte(payer.id), []byte("reroute"))}
	original := apply(t, s, payer, 1, a, 1)
	want := sha256.Sum256(original)
	balance, nonce := s.Accounts[payer.id].Balance, s.Accounts[payer.id].Nonce
	s, err := s.Clone()
	if err != nil {
		t.Fatal(err)
	}
	a.HoldID, a.Node = "new-transport", actors["auditor"].id
	// Both a fresh nonce and the already consumed nonce must return the original.
	for _, n := range []uint64{2, 1} {
		raw, err := Sign(s.Parameters.ChainID, payer.key, n, a)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Apply(raw, 2)
		if err != nil || got != hex.EncodeToString(want[:]) {
			t.Fatalf("reroute: hash=%s err=%v", got, err)
		}
		if len(s.Holds) != 1 || s.Accounts[payer.id].Balance != balance || s.Accounts[payer.id].Nonce != nonce {
			t.Fatal("reroute changed hold, balance, or nonce")
		}
	}
	for _, field := range []string{"amount", "profile", "model", "payer", "signer"} {
		t.Run(field, func(t *testing.T) {
			changed, signer := a, payer
			switch field {
			case "amount":
				changed.Amount++
			case "profile":
				changed.ProfileHash = "changed"
			case "model":
				changed.ModelID = "changed"
			case "payer":
				changed.Payer = actors["auditor"].id
			case "signer":
				signer = actors["auditor"]
			}
			raw, err := Sign(s.Parameters.ChainID, signer.key, 2, changed)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Apply(raw, 2); err == nil || !strings.Contains(err.Error(), "idempotency key reused with different payload") {
				t.Fatalf("expected conflict: %v", err)
			}
			if len(s.Holds) != 1 || s.Accounts[payer.id].Balance != balance || s.Accounts[payer.id].Nonce != nonce {
				t.Fatal("conflict mutated state")
			}
		})
	}
}

func TestFreezeIdempotencyCoverage(t *testing.T) {
	pipeline := Action{Type: "freeze_pipeline", TotalLayers: 8, PipelineAuthorizations: []PipelineAuthorization{{NodeID: "a", StartLayer: 0, EndLayer: 4}, {NodeID: "b", StartLayer: 4, EndLayer: 8}}}
	moe := Action{Type: "freeze_moe", MoeAuthorizations: []MoeAuthorization{{Kind: "attention", NodeID: "a", EndLayer: 4}, {Kind: "expert", NodeID: "b", Units: []MoeUnitRef{{Layer: 1, Expert: 2}, {Layer: 2, Expert: 3}}}}}
	tests := []struct {
		name   string
		base   Action
		change func(*Action)
		equal  bool
	}{
		{"pipeline routing", pipeline, func(a *Action) {
			for i := range a.PipelineAuthorizations {
				a.PipelineAuthorizations[i].NodeID = "new"
				a.PipelineAuthorizations[i].Profile = []byte("derived")
				a.PipelineAuthorizations[i].Reserved = 99
			}
		}, true},
		{"pipeline boundary", pipeline, func(a *Action) { a.PipelineAuthorizations[0].EndLayer++ }, false},
		{"pipeline order", pipeline, func(a *Action) {
			a.PipelineAuthorizations[0], a.PipelineAuthorizations[1] = a.PipelineAuthorizations[1], a.PipelineAuthorizations[0]
		}, false},
		{"expert regroup", moe, func(a *Action) {
			a.MoeAuthorizations = []MoeAuthorization{{Kind: "expert", NodeID: "x", Units: []MoeUnitRef{{Layer: 2, Expert: 3}}}, {Kind: "attention", NodeID: "y", EndLayer: 4, Profile: []byte("derived"), Reserved: 100}, {Kind: "expert", NodeID: "z", Units: []MoeUnitRef{{Layer: 1, Expert: 2}}}}
		}, true},
		{"expert missing", moe, func(a *Action) { a.MoeAuthorizations[1].Units = a.MoeAuthorizations[1].Units[:1] }, false},
		{"expert extra", moe, func(a *Action) {
			a.MoeAuthorizations[1].Units = append(a.MoeAuthorizations[1].Units, MoeUnitRef{Layer: 3, Expert: 4})
		}, false},
		{"attention span", moe, func(a *Action) { a.MoeAuthorizations[0].EndLayer++ }, false},
		{"coverage kind", moe, func(a *Action) { a.MoeAuthorizations[0].Kind = "unknown" }, false},
		{"strict default", moe, func(a *Action) { a.Notice = "changed" }, false},
		{"moe tokens", moe, func(a *Action) { a.MoeInputTokens++ }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := freezePayload(tt.base)
			var incoming Action
			if err := json.Unmarshal(payload, &incoming); err != nil {
				t.Fatal(err)
			}
			tt.change(&incoming)
			got, err := freezeEconomicallyEqual(payload, incoming)
			if err != nil || got != tt.equal {
				t.Fatalf("equal=%v want=%v err=%v", got, tt.equal, err)
			}
			if !bytes.Equal(payload, freezePayload(tt.base)) {
				t.Fatal("projection mutated original")
			}
		})
	}
}

func TestFreezeIdempotencyCommitment(t *testing.T) {
	base := &pb.ExecutionCommitment{RequestId: "first", Signer: []byte("node"), Signature: []byte("sig"), InputTokens: 12, InputCommitment: []byte("input"), Seed: 7, ClientNonce: []byte("nonce")}
	marshal := func(c *pb.ExecutionCommitment) []byte {
		t.Helper()
		raw, err := proto.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	stored := freezePayload(Action{Type: "freeze", PricingCommitment: marshal(base)})
	tests := []struct {
		name   string
		change func(*pb.ExecutionCommitment)
		equal  bool
	}{
		{"routing", func(c *pb.ExecutionCommitment) {
			c.RequestId = "retry"
			c.Signer = []byte("other")
			c.Signature = []byte("new signature")
		}, true},
		{"input tokens", func(c *pb.ExecutionCommitment) { c.InputTokens++ }, false},
		{"input commitment", func(c *pb.ExecutionCommitment) { c.InputCommitment = []byte("different") }, false},
		{"seed", func(c *pb.ExecutionCommitment) { c.Seed++ }, false},
		{"client nonce", func(c *pb.ExecutionCommitment) { c.ClientNonce = []byte("different") }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := proto.Clone(base).(*pb.ExecutionCommitment)
			tt.change(c)
			got, err := freezeEconomicallyEqual(stored, Action{Type: "freeze", PricingCommitment: marshal(c)})
			if err != nil || got != tt.equal {
				t.Fatalf("equal=%v want=%v err=%v", got, tt.equal, err)
			}
		})
	}
	for _, incoming := range [][]byte{{0xff}, {0xfe}, marshal(base)} {
		a := Action{Type: "freeze", PricingCommitment: []byte{0xff}}
		got, err := freezeEconomicallyEqual(freezePayload(a), Action{Type: "freeze", PricingCommitment: incoming})
		if err != nil || got != bytes.Equal(a.PricingCommitment, incoming) {
			t.Fatalf("malformed commitment comparison: %v %v", got, err)
		}
	}
	if equal, err := freezeEconomicallyEqual([]byte("invalid JSON"), Action{}); err == nil || equal {
		t.Fatal("malformed stored payload accepted")
	}
}
