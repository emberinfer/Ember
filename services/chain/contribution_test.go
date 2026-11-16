package chain

import (
	"context"
	core "ember/core-go"
	pb "ember/proto"
	"encoding/json"
	"fmt"
	abci "github.com/cometbft/cometbft/abci/types"
	"google.golang.org/protobuf/proto"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestContributionCapsHeightAtomicityAndRelease(t *testing.T) {
	s, a := testState(t)
	node := a["node"].id
	model := "ember-test-model"
	caps := &ContributionCaps{Profiles: map[string]uint32{model: 1}, Global: 1}
	apply(t, s, a["node"], 1, Action{Type: "update_caps", Caps: caps}, 2)
	if s.Nodes[node].CapsEffectiveHeight != 3 || s.Nodes[node].CapsAt(2) != nil {
		t.Fatal("wrong activation boundary")
	}
	// Two assignments in the update block are permitted by the old policy.
	for i := 0; i < 2; i++ {
		apply(t, s, a["payer"], uint64(i+1), Action{Type: "freeze", HoldID: fmt.Sprint(i), Payer: a["payer"].id, Node: node, Amount: 1}, 2)
	}
	rejected(t, s, a["payer"], Action{Type: "freeze", HoldID: "blocked", Payer: a["payer"].id, Node: node, Amount: 1}, 3)
	for _, h := range s.Holds {
		h.State = "dispute"
	}
	apply(t, s, a["payer"], 3, Action{Type: "freeze", HoldID: "released", Payer: a["payer"].id, Node: node, Amount: 1}, 3)
	rejected(t, s, a["payer"], Action{Type: "freeze", HoldID: "same-block", Payer: a["payer"].id, Node: node, Amount: 1}, 3)
	if len(s.OffenseRecords) != 0 || len(s.Offenses) != 0 {
		t.Fatal("refusal created fault")
	}
	// Empty offered set is a legitimate pause, while nil cannot silently reset caps.
	apply(t, s, a["node"], 2, Action{Type: "update_caps", Caps: &ContributionCaps{Profiles: map[string]uint32{}}}, 4)
	if s.ContributionAvailable(node, model, 23, 1) {
		t.Fatal("pause not enforced")
	}
}

func TestContributionOfferedSetGlobalQualificationAndPersistence(t *testing.T) {
	s, a := testState(t)
	node := a["node"].id
	model := "ember-test-model"
	r := s.Nodes[node]
	r.Caps = &ContributionCaps{Profiles: map[string]uint32{model: 2, "other": 2}, Global: 1}
	s.Holds["other"] = &Hold{Node: node, ModelID: "other", State: "frozen", ExpireHeight: 50}
	if s.ContributionAvailable(node, model, 2, 1) {
		t.Fatal("global cap ignored")
	}
	if s.ContributionAvailable(node, "excluded", 2, 1) {
		t.Fatal("offered set ignored")
	}
	delete(s.Holds, "other")
	p := s.ActiveProfile(node, 2)
	p.Contribution = &pb.ContributionSignal{Unavailable: true, Reason: "measured weights + KV exceed budget"}
	core.Sign(p, a["node"].key)
	r.Profile, _ = proto.Marshal(p)
	if s.ContributionAvailable(node, model, 2, 1) {
		t.Fatal("qualification ignored below cap")
	}
	before, _ := s.Hash()
	rejected(t, s, a["payer"], Action{Type: "freeze", HoldID: "unavailable", Payer: a["payer"].id, Node: node, Amount: 1}, 2)
	after, _ := s.Hash()
	if string(before) != string(after) {
		t.Fatal("rejected work mutated state")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, s); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadState(path)
	if err != nil || loaded.ContributionAvailable(node, model, 2, 1) {
		t.Fatal("qualification/caps not persisted", err)
	}
}

// The ABCI application serializes actual parallel submissions, including same-
// block races. Its committed hold records are the reservation authority.
func TestContributionParallelABCIRehearsal(t *testing.T) {
	s, a := testState(t)
	node := a["node"].id
	s.Nodes[node].Caps = &ContributionCaps{Profiles: map[string]uint32{"ember-test-model": 1}}
	app := &Application{committed: s}
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			raw, err := Sign(s.Parameters.ChainID, a["payer"].key, 1, Action{Type: "freeze", HoldID: fmt.Sprint("parallel-", i), Payer: a["payer"].id, Node: node, Amount: 1})
			if err != nil {
				t.Error(err)
				return
			}
			reply, err := app.CheckTx(context.Background(), &abci.CheckTxRequest{Tx: raw})
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if reply.Code == 0 {
				accepted++
			}
		}(i)
	}
	wg.Wait()
	// CheckTx is advisory; all may pass. FinalizeBlock must admit only one.
	txs := [][]byte{}
	for i := 0; i < 16; i++ {
		raw, _ := Sign(s.Parameters.ChainID, a["payer"].key, uint64(i+1), Action{Type: "freeze", HoldID: fmt.Sprint("parallel-", i), Payer: a["payer"].id, Node: node, Amount: 1})
		txs = append(txs, raw)
	}
	reply, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 2, Txs: txs})
	if err != nil {
		t.Fatal(err)
	}
	successes := 0
	for _, r := range reply.TxResults {
		if r.Code == 0 {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("admitted %d competing reservations", successes)
	}
	app.Commit(context.Background(), &abci.CommitRequest{})
	committed, _ := app.State()
	per, total := committed.InFlight(node, "ember-test-model", 2)
	if per != 1 || total != 1 || len(committed.OffenseRecords) != 0 {
		t.Fatal("reservation/fault invariant failed")
	}
	t.Logf("parallel=%d CheckTx_advisory=%d committed=%d in_flight=%d offenses=%d", len(txs), accepted, successes, per, len(committed.OffenseRecords))
	if dir := os.Getenv("CAPS_EVIDENCE_DIR"); dir != "" {
		raw, _ := json.MarshalIndent(committed, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, "abci-state.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestContributionPipelineAggregateAndAuditorReroute(t *testing.T) {
	s, a := testState(t)
	node := a["node"].id
	model := "ember-test-model"
	s.Nodes[node].Caps = &ContributionCaps{Profiles: map[string]uint32{model: 1}, Global: 1}
	action := Action{Type: "freeze_pipeline", PipelineAuthorizations: []PipelineAuthorization{{NodeID: node}, {NodeID: node}}}
	if s.checkContributionAction(action, 2) == nil {
		t.Fatal("multiple stages exceeded a single slot")
	}
	for _, name := range []string{"auditor", "auditor2"} {
		who := a[name]
		p := &pb.ResourceProfile{NodeId: who.id, ChainId: model, ExecutionProfileVer: "v1"}
		core.Sign(p, who.key)
		raw, _ := proto.Marshal(p)
		s.Nodes[who.id] = &NodeRegistration{NodeID: who.id, Profile: raw, ExpiresHeight: 100, Caps: &ContributionCaps{Profiles: map[string]uint32{model: 1}}}
		s.Accounts[who.id].Stake = 5000
	}
	s.Nodes[a["auditor"].id].Caps.Profiles = map[string]uint32{}
	apply(t, s, a["payer"], 1, Action{Type: "freeze", HoldID: "auditor-reroute", Node: node, Payer: a["payer"].id, Amount: 1}, 1)
	h := s.Holds["auditor-reroute"]
	h.State = "dispute"
	selected, _, err := s.selectAuditor(h, "", 5)
	if err != nil || selected != a["auditor2"].id {
		t.Fatalf("did not reroute capped auditor: %s %v", selected, err)
	}
	h.Auditor = selected
	if s.ContributionAvailable(selected, model, 5, 1) {
		t.Fatal("pending audit failed to consume slot")
	}
	if s.ContributionAvailable(selected, model, h.ExpireHeight+1, 1) {
		t.Fatal("unfinished audit released slot at execution timeout")
	}
	if len(s.OffenseRecords) != 0 {
		t.Fatal("capped auditor received fault")
	}
}
