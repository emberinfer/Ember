package chain

import (
	"context"
	core "ember/core-go"
	pb "ember/proto"
	"encoding/json"
	"fmt"
	abci "github.com/cometbft/cometbft/abci/types"
	"google.golang.org/protobuf/proto"
	"reflect"
	"testing"
)

func TestTargetedQueryDifferential(t *testing.T) {
	s, a := testState(t)
	s.Version = 11
	s.Height = 100
	for _, name := range []string{"node", "auditor"} {
		actor := a[name]
		p := &pb.ResourceProfile{NodeId: actor.id, ChainId: "ember-test-model", ExecutionProfileVer: "v1", ManifestRoot: []byte{9}, ActualDevice: "cpu", EngineCapabilities: &pb.EngineCapabilities{EngineName: "mock", EngineVersion: "1", Roles: []string{"model", "auditor"}, ForcedPrefixReplay: true, MaxTopK: 16, ScoreKind: "raw-logits", EvidenceLevels: []string{"full"}}}
		if err := core.Sign(p, actor.key); err != nil {
			t.Fatal(err)
		}
		raw, _ := proto.Marshal(p)
		s.Nodes[actor.id] = &NodeRegistration{NodeID: actor.id, Profile: raw, ExpiresHeight: 200}
		s.Accounts[actor.id].Stake = 5000
	}
	for i := 0; i < 300; i++ {
		id := fmt.Sprintf("terminal-%03d", i)
		s.Holds[id] = &Hold{ID: id, Node: a["node"].id, State: "cancelled", ExpireHeight: 1}
	}
	s.Holds["expired"] = &Hold{ID: "expired", State: "frozen", ExpireHeight: 99}
	s.Holds["dispute"] = &Hold{ID: "dispute", State: "dispute", ExpireHeight: 100}
	s.Holds["future"] = &Hold{ID: "future", State: "frozen", ExpireHeight: 101}
	s.pruneBlockHashes()
	s.archiveTerminalHolds()
	s.Privacy = &PrivacyState{Requests: map[string]*PrivacyRequest{"r": {ID: "r", BundleRef: "bundle", Segments: []PrivacySegment{{BundleRef: "segment"}}}}, Epochs: map[string]*PrivacyEpoch{"1": {ID: "1"}}, Operators: map[string]PrivacyOperator{"op": {PolicyHash: "hash"}}}
	app := &Application{committed: s}
	query := func(path string, want any) {
		t.Helper()
		q, err := app.Query(context.Background(), &abci.QueryRequest{Path: path})
		if err != nil || q.Code != 0 {
			t.Fatalf("%s: %v %s", path, err, q.Log)
		}
		expected, _ := json.Marshal(want)
		if string(q.Value) != string(expected) {
			t.Fatalf("%s got %s want %s", path, q.Value, expected)
		}
	}
	for i := 0; i < 300; i++ {
		id := fmt.Sprintf("terminal-%03d", i)
		query("/hold/"+id, s.LookupHold(id))
		route, err := s.QueryAuditRoute(id)
		if err != nil {
			t.Fatal(err)
		}
		query("/audit-route/"+id, route)
	}
	wantExpired := map[string]*Hold{}
	for id, h := range s.Holds {
		if (h.State == "frozen" || h.State == "dispute") && h.ExpireHeight <= s.Height {
			wantExpired[id] = h
		}
	}
	query("/expired-holds", wantExpired)
	query("/privacy/request/r", s.Privacy.Requests["r"])
	query("/privacy/epoch/1", s.Privacy.Epochs["1"])
	query("/privacy/operator/op", s.Privacy.Operators["op"])
	query("/privacy/request-by-bundle/bundle", s.Privacy.Requests["r"])
	query("/privacy/request-by-bundle/segment", s.Privacy.Requests["r"])
	for _, path := range []string{"/privacy/request/missing", "/privacy/epoch/missing", "/privacy/operator/missing", "/privacy/request-by-bundle/missing", "/audit-route/missing", "/liveness/missing", "/node-status/missing"} {
		q, _ := app.Query(context.Background(), &abci.QueryRequest{Path: path})
		if q.Code == 0 || q.Log != "not found" {
			t.Fatalf("%s %+v", path, q)
		}
	}
	for id, r := range s.Nodes {
		status := s.QueryNodeStatus(id)
		if status.Active != (s.ActiveProfile(id, s.Height) != nil) || status.ExecutionAllowed != (s.Version < 10 || s.ExecutionAllowed(id, s.Height)) {
			t.Fatal("node status differs")
		}
		p := &pb.ResourceProfile{}
		_ = proto.Unmarshal(r.Profile, p)
		if status.ContributionAvailable != s.ContributionAvailable(id, p.ChainId, s.Height+1, 1) {
			t.Fatal("contribution differs")
		}
		query("/node-status/"+id, status)
		query("/liveness/"+id, map[string]any{"nodes": map[string]*NodeRegistration{id: r}, "height": s.Height, "parameters": map[string]any{"liveness": s.Parameters.Liveness}})
	}
	// Route derivation is compared with the previous direct state lookup logic.
	id := "route"
	h := &Hold{ID: id, State: "dispute", Node: a["node"].id, Auditor: a["auditor"].id}
	s.Holds[id] = h
	route, err := s.QueryAuditRoute(id)
	if err != nil {
		t.Fatal(err)
	}
	producer := s.ActiveProfile(h.Node, s.Height)
	eligible := producer != nil && s.EligibleAuditorProfile(h.Auditor, producer.NodeId, producer, s.Height)
	if !proto.Equal(route.Producer, producer) || !proto.Equal(route.Auditor, s.ActiveProfile(h.Auditor, s.Height)) || route.Eligible != eligible || !reflect.DeepEqual(route.Hold, h) {
		t.Fatal("route differs")
	}
	if !route.Eligible {
		t.Fatal("fixture must exercise eligible auditor")
	}
	frozen := proto.Clone(producer).(*pb.ResourceProfile)
	frozen.ExecutionProfileVer = "frozen"
	frozenRaw, _ := proto.Marshal(frozen)
	h.ProducerProfile = frozenRaw
	frozenRoute, err := s.QueryAuditRoute(id)
	if err != nil || !proto.Equal(frozenRoute.Producer, frozen) {
		t.Fatal("frozen producer differs", err)
	}
	h.Pipeline = &PipelineHold{Authorizations: []PipelineAuthorization{{Profile: s.Nodes[a["node"].id].Profile}}}
	h.Assignments = []AssignmentRecord{{DrawnStage: &DrawnStage{Index: 0}}}
	rangeRoute, err := s.QueryAuditRoute(id)
	if err != nil || !proto.Equal(rangeRoute.Producer, producer) || rangeRoute.Eligible != s.EligibleRangeAuditor(h.Auditor, producer.NodeId, producer, s.Height) {
		t.Fatal("range route differs", err)
	}
	h.Pipeline = nil
	h.Assignments = nil
	h.ProducerProfile = nil
	for _, outcome := range []string{"accept", "reject", "service_failure"} {
		expected, err := s.ExpectedSettlementAuditReceipts(h, outcome, s.Height+1)
		q, _ := app.Query(context.Background(), &abci.QueryRequest{Path: "/settlement-receipts/route?outcome=" + outcome})
		if err != nil {
			if q.Code == 0 {
				t.Fatal("missing receipt error")
			}
			continue
		}
		raw, _ := json.Marshal(expected)
		if q.Code != 0 || string(q.Value) != string(raw) {
			t.Fatal("settlement differs", q.Log)
		}
	}
}
