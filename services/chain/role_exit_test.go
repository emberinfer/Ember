package chain

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestExecExitLifecycle(t *testing.T) {
	s, a := liveFixture(t)
	s.Version = 13
	who := a["node"]
	id := who.id
	apply(t, s, who, s.Accounts[id].Nonce+1, Action{Type: "request_exec_exit"}, 10)
	if !s.Nodes[id].PendingNoExec || s.Nodes[id].ExecExitEffectiveHeight != 1010 {
		t.Fatal("missing delayed exit")
	}
	for _, h := range []int64{10, 1009, 1010} {
		s.Height = h
		v := s.QueryNodeStatus(id)
		if s.ExecutionAllowed(id, h) != (h < 1010) || !s.ReviewAllowed(id, h) || v.ExecExitPending != (h < 1010) || v.ExecExited != (h >= 1010) || v.ExecExitEffectiveHeight != 1010 {
			t.Fatalf("roles/view at %d: %+v", h, v)
		}
		want := "pending"
		if h >= 1010 {
			want = "effective"
		}
		if err := s.requestExecExit(id, h); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatal("duplicate", err)
		}
	}
}
func TestExecExitRejectsAtomically(t *testing.T) {
	for _, kind := range []string{"version", "missing", "expired", "opted-out"} {
		t.Run(kind, func(t *testing.T) {
			s, a := liveFixture(t)
			s.Version = 13
			who := a["node"]
			switch kind {
			case "version":
				s.Version = 12
			case "missing":
				delete(s.Nodes, who.id)
			case "expired":
				s.Nodes[who.id].ExpiresHeight = 10
			case "opted-out":
				s.Nodes[who.id].NoExec = true
			}
			before, _ := json.Marshal(s)
			raw, err := Sign(s.Parameters.ChainID, who.key, s.Accounts[who.id].Nonce+1, Action{Type: "request_exec_exit"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Apply(raw, 10); err == nil {
				t.Fatal("accepted")
			}
			after, _ := json.Marshal(s)
			if !bytes.Equal(before, after) {
				t.Fatal("rejection mutated state")
			}
		})
	}
}
func TestExecExitRenewal(t *testing.T) {
	for _, h := range []int64{20, 1010} {
		s, a := liveFixture(t)
		s.Version = 13
		who := a["node"]
		id := who.id
		if s.QueryNodeStatus(id).ExecExitPending || s.QueryNodeStatus(id).ExecExited {
			t.Fatal("unrequested view")
		}
		apply(t, s, who, s.Accounts[id].Nonce+1, Action{Type: "request_exec_exit"}, 10)
		apply(t, s, who, s.Accounts[id].Nonce+1, Action{Type: "register_node", Profile: s.Nodes[id].Profile, ExpiresHeight: 19001}, h)
		if !s.Nodes[id].PendingNoExec || s.Nodes[id].ExecExitEffectiveHeight != 1010 || s.ExecutionAllowed(id, 1010) {
			t.Fatal("renewal cleared exit")
		}
	}
}
func TestExecExitLegacyJSON(t *testing.T) {
	raw, _ := json.Marshal(&NodeRegistration{NodeID: "n", Profile: []byte("p"), RegisteredAt: 1, ExpiresHeight: 2})
	if string(raw) != `{"node_id":"n","profile":"cA==","registered_at":1,"expires_height":2}` {
		t.Fatal(string(raw))
	}
	s, a := liveFixture(t)
	s.Version = 12
	r := s.Nodes[a["node"].id]
	r.PendingNoExec = true
	r.ExecExitEffectiveHeight = 3
	if !s.ExecutionAllowed(r.NodeID, 3) || s.QueryNodeStatus(r.NodeID).ExecExited {
		t.Fatal("pre-v13 behavior changed")
	}
}
