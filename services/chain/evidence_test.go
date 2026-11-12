package chain

import (
	"bytes"
	core "ember/core-go"
	pb "ember/proto"
	"ember/services/evidence"
	"encoding/hex"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestChunkedEvidenceFailureRefundAndRootBinding(t *testing.T) {
	s, a := testState(t)
	s.Parameters.Evidence.MaxFrameTokens = 2
	apply(t, s, a["payer"], 1, Action{Type: "freeze", HoldID: "chunks", Payer: a["payer"].id, Node: a["node"].id, Amount: 3}, 1)
	r := &pb.ExecutionReceipt{}
	proto.Unmarshal(signedReceipt(t, "chunks", a["node"]), r)
	r.OutputTokens = 3
	r.ChunkRoots[0].UptoStep = 3
	core.Sign(r.ChunkRoots[0], a["node"].key)
	store := &evidence.Store{Dir: t.TempDir()}
	h, _ := store.Put([]byte(`[1,2]`))
	h2, _ := store.Put([]byte(`[3]`))
	m := &pb.EvidenceManifest{ChunkCount: 2, TotalTokens: 3, ChunkHashes: [][]byte{h, h2}}
	m.MerkleRoot = evidence.MerkleRoot(m.ChunkHashes)
	r.EvidenceManifest = m
	core.Sign(r, a["node"].key)
	raw, _ := proto.Marshal(r)
	apply(t, s, a["coordinator"], 1, Action{Type: "submit", HoldID: "chunks", Actual: 3, Receipt: raw}, 2)
	if !proto.Equal(s.Holds["chunks"].EvidenceManifest, m) {
		t.Fatal("manifest not anchored")
	}
	apply(t, s, a["coordinator"], 2, Action{Type: "assign_audit", HoldID: "chunks"}, 3)
	hold := s.Holds["chunks"]
	var auditor actor
	for _, v := range a {
		if v.id == hold.Auditor {
			auditor = v
		}
	}
	proof, _ := hex.DecodeString(hold.AuditProof)
	challenge, verdict, _ := signedAudit(t, raw, auditor, "inconclusive", proof)
	// The signed verdict must cite the manifest, even for service failure.
	bad, _ := Sign(s.Parameters.ChainID, auditor.key, 1, Action{Type: "audit", HoldID: "chunks", Challenge: challenge, Verdict: verdict})
	if _, err := s.Apply(bad, 4); err == nil {
		t.Fatal("unbound verdict accepted")
	}
	v := &pb.AuditVerdict{}
	proto.Unmarshal(verdict, v)
	v.EvidenceRef = m.MerkleRoot
	core.Sign(v, auditor.key)
	verdict, _ = proto.Marshal(v)
	apply(t, s, auditor, 1, Action{Type: "audit", HoldID: "chunks", Challenge: challenge, Verdict: verdict}, 4)
	if s.Holds["chunks"].AuditVerdict != "service_failure" {
		t.Fatal("unavailable evidence accepted")
	}
	apply(t, s, a["coordinator"], 3, Action{Type: "settle", HoldID: "chunks", Outcome: "service_failure"}, 5)
	if s.Accounts[a["payer"].id].Balance != 1000 || s.Accounts[a["node"].id].Stake != 5000 {
		t.Fatal("failure refund changed")
	}
	if !bytes.Equal(s.Holds["chunks"].EvidenceManifest.MerkleRoot, m.MerkleRoot) {
		t.Fatal("settlement lost manifest")
	}
}

func TestEvidenceFrameParameterEnforcedOnReceipt(t *testing.T) {
	for _, kind := range []string{"missing", "bad-root", "coverage"} {
		t.Run(kind, func(t *testing.T) {
			s, a := testState(t)
			if s.Parameters.Evidence.FrameTokens() != 512 {
				t.Fatal("wrong default frame parameter")
			}
			s.Parameters.Evidence.MaxFrameTokens = 2
			apply(t, s, a["payer"], 1, Action{Type: "freeze", HoldID: "frame", Payer: a["payer"].id, Node: a["node"].id, Amount: 3}, 1)
			r := &pb.ExecutionReceipt{}
			proto.Unmarshal(signedReceipt(t, "frame", a["node"]), r)
			r.OutputTokens = 3
			r.ChunkRoots[0].UptoStep = 3
			core.Sign(r.ChunkRoots[0], a["node"].key)
			if kind != "missing" {
				m := &pb.EvidenceManifest{ChunkCount: 2, TotalTokens: 3, ChunkHashes: [][]byte{bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)}}
				m.MerkleRoot = evidence.MerkleRoot(m.ChunkHashes)
				if kind == "bad-root" {
					m.MerkleRoot[0] ^= 1
				} else {
					m.TotalTokens = 4
				}
				r.EvidenceManifest = m
			}
			core.Sign(r, a["node"].key)
			raw, _ := proto.Marshal(r)
			tx, _ := Sign(s.Parameters.ChainID, a["coordinator"].key, 1, Action{Type: "submit", HoldID: "frame", Actual: 3, Receipt: raw})
			if _, err := s.Apply(tx, 2); err == nil {
				t.Fatal("invalid evidence receipt accepted")
			}
		})
	}
}
