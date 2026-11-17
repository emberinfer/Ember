package chain

import (
	"bytes"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
	"math"
	"math/rand"
	"testing"
)

func TestPerfAtomicDifferential(t *testing.T) {
	for _, version := range []uint64{10, 11} {
		s, a := testState(t)
		s.Version = version
		oracle, e := s.Clone()
		if e != nil {
			t.Fatal(e)
		}
		rng := rand.New(rand.NewSource(11))
		for i := 0; i < 120; i++ {
			action := Action{Type: "transfer", To: a["node"].id, Amount: int64(rng.Intn(40)) - 10}
			if i%7 == 0 {
				action.Type = "stake"
			}
			if i%11 == 0 {
				action.Type = "bad"
			}
			if i%13 == 0 {
				action.Amount = math.MaxInt64
			}
			raw, e := Sign(s.Parameters.ChainID, a["payer"].key, s.Accounts[a["payer"].id].Nonce+1, action)
			if e != nil {
				t.Fatal(e)
			}
			before, _ := s.Bytes()
			trial, e := oracle.Clone()
			if e != nil {
				t.Fatal(e)
			}
			// Force the original JSON isolation path without changing version semantics.

			h1, e1 := s.Apply(raw, int64(i+1))
			h2, e2 := trial.applyWithCopy(raw, int64(i+1), trial.Clone)

			if (e1 == nil) != (e2 == nil) || h1 != h2 {
				t.Fatalf("v%d step %d: %v / %v", version, i, e1, e2)
			}
			if e2 == nil {
				oracle = trial
			}
			after, _ := s.Bytes()
			want, _ := oracle.Bytes()
			if !bytes.Equal(after, want) {
				t.Fatalf("v%d step %d differs", version, i)
			}
			if e1 != nil && !bytes.Equal(before, after) {
				t.Fatal("failed Apply mutated state")
			}
		}
	}
}

func TestPerfAtomicPartialMutationRollback(t *testing.T) {
	for _, v := range []uint64{10, 11} {
		for _, kind := range []string{"transfer", "stake"} {
			s, a := testState(t)
			s.Version = v
			s.Accounts[a["node"].id].Balance = math.MaxInt64
			s.Accounts[a["payer"].id].Stake = math.MaxInt64
			raw, _ := Sign(s.Parameters.ChainID, a["payer"].key, 1, Action{Type: kind, To: a["node"].id, Amount: 1})
			before, _ := s.Bytes()
			if _, e := s.Apply(raw, 1); e == nil {
				t.Fatal("expected overflow")
			}
			after, _ := s.Bytes()
			if !bytes.Equal(before, after) {
				t.Fatalf("v%d %s debit survived failure", v, kind)
			}
		}
		s, a := testState(t)
		s.Version = v
		oracle, _ := s.Clone()
		for i, action := range []Action{
			{Type: "freeze", HoldID: "oracle", Payer: a["payer"].id, Node: a["node"].id, Amount: 100},
			{Type: "cancel", HoldID: "oracle"},
			{Type: "cancel", HoldID: "oracle"},
			{Type: "freeze", HoldID: "bad", Payer: a["payer"].id, Node: a["node"].id, Amount: math.MaxInt64},
		} {
			raw, _ := Sign(s.Parameters.ChainID, a["payer"].key, s.Accounts[a["payer"].id].Nonce+1, action)
			before, _ := s.Bytes()
			got, e := s.Apply(raw, int64(i+1))
			want, oe := oracle.applyWithCopy(raw, int64(i+1), oracle.Clone)
			if (e == nil) != (oe == nil) || got != want {
				t.Fatalf("v%d step%d: %v / %v", v, i, e, oe)
			}
			after, _ := s.Bytes()
			other, _ := oracle.Bytes()
			if !bytes.Equal(after, other) || (e != nil && !bytes.Equal(before, after)) {
				t.Fatal("hold oracle differs")
			}
		}
	}
}

func TestPerfCopyMatchesJSONProjection(t *testing.T) {
	s, _ := testState(t)
	s.Version = 11
	s.Holds["projection"] = &Hold{ID: "projection", State: "frozen", EvidenceManifest: &pb.EvidenceManifest{}}
	s.Holds["projection"].EvidenceManifest.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
	fast, e := s.transactionCopy()
	if e != nil {
		t.Fatal(e)
	}
	old, e := s.Clone()
	if e != nil {
		t.Fatal(e)
	}
	if !proto.Equal(fast.Holds["projection"].EvidenceManifest, old.Holds["projection"].EvidenceManifest) {
		t.Fatal("protobuf JSON projection differs")
	}
	got, _ := fast.Bytes()
	want, _ := old.Bytes()
	if !bytes.Equal(got, want) {
		t.Fatal("JSON projection differs")
	}
}
