package ledger

import (
	"crypto/ed25519"
	core "ember/core-go"
	pb "ember/proto"
	ch "ember/services/chain"
	"encoding/json"
	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	"google.golang.org/protobuf/proto"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Full nodes may reconnect and finish existing work. Enrollment must not use
// the scheduler's stricter check for another available slot.
func TestContributionCappedNodeCanEnrollButCannotSchedule(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	id := ch.AccountID(key.Public().(ed25519.PublicKey))
	profile := &pb.ResourceProfile{NodeId: id, ChainId: "model", ExecutionProfileVer: "v1"}
	if err := core.Sign(profile, key); err != nil {
		t.Fatal(err)
	}
	raw, _ := proto.Marshal(profile)
	r := &ch.NodeRegistration{NodeID: id, Profile: raw, ExpiresHeight: 100, Caps: &ch.ContributionCaps{Profiles: map[string]uint32{"model": 1}}}
	state := ch.State{Height: 2, Parameters: ch.Parameters{MinNodeStake: 1}, Nodes: map[string]*ch.NodeRegistration{id: r}, Accounts: map[string]*ch.Account{id: {Stake: 100}}, Holds: map[string]*ch.Hold{"active": {Node: id, ModelID: "model", State: "frozen", ExpireHeight: 100}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var query struct {
			ID     json.RawMessage `json:"id"`
			Params struct {
				Path string `json:"path"`
			} `json:"params"`
		}
		if err := json.NewDecoder(req.Body).Decode(&query); err != nil {
			t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		var data []byte
		if strings.HasPrefix(query.Params.Path, "/node/") {
			data, _ = json.Marshal(r)
		} else {
			data, _ = json.Marshal(state)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": query.ID, "result": map[string]any{"response": map[string]any{"code": 0, "value": data}}})
	}))
	defer server.Close()
	rpc, err := rpchttp.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	l := &ChainLedger{rpc: rpc}
	if _, err = l.NodeProfile(id); err != nil {
		t.Fatal("capped enrollment rejected", err)
	}
	if _, err = l.SchedulableNodeProfile(id); err == nil || !strings.Contains(err.Error(), "contribution unavailable") {
		t.Fatal("capped node scheduled", err)
	}
	if err = l.contributionAdmission(ch.Action{Type: "freeze", HoldID: "another", Node: id}); err == nil {
		t.Fatal("preflight ignored full slot")
	}

	// AppVersion 10: enrollment remains available to reviewers; execution does not.
	state.Version = 10
	state.Holds = nil
	r.Caps = nil
	if _, err = l.SchedulableNodeProfile(id); err != nil {
		t.Fatal("default full executor missing", err)
	}
	profiles, err := l.NodeProfiles()
	if err != nil || len(profiles) != 1 {
		t.Fatal("default full candidate missing", err)
	}
	r.NoExec = true
	if _, err = l.NodeProfile(id); err != nil {
		t.Fatal("review-only enrollment rejected", err)
	}
	if _, err = l.SchedulableNodeProfile(id); err == nil {
		t.Fatal("exec-opt-out entered gateway candidates")
	}
	profiles, err = l.NodeProfiles()
	if err != nil || len(profiles) != 0 {
		t.Fatal("exec-opt-out listed as executor", err)
	}
}
