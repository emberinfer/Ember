package chain

import (
	"bytes"
	"context"
	core "ember/core-go"
	pb "ember/proto"
	"encoding/json"
	"fmt"
	abci "github.com/cometbft/cometbft/abci/types"
	"google.golang.org/protobuf/proto"
	"path/filepath"
	"testing"
)

func TestRegistrationDefaultFullAndOptOutLottery(t *testing.T) {
	for _, opt := range []string{"default", "no-audit", "no-exec"} {
		t.Run(opt, func(t *testing.T) {
			s, a := liveFixture(t)
			who := a["auditor"]
			p := s.ActiveProfile(who.id, 2)
			p.EngineCapabilities.Roles = nil // No registration role declaration required.
			core.Sign(p, who.key)
			raw, _ := proto.Marshal(p)
			delete(s.Nodes, who.id)
			apply(t, s, who, s.Accounts[who.id].Nonce+1, Action{Type: "register_node", Profile: raw, ExpiresHeight: 19000, NoAudit: opt == "no-audit", NoExec: opt == "no-exec"}, 3)
			s.Height = 3
			if s.ExecutionAllowed(who.id, 3) != (opt != "no-exec") {
				t.Fatal("executor opt-out")
			}
			if s.EligibleAuditor(who.id, a["node"].id, 3) != (opt != "no-audit") {
				t.Fatal("review opt-out")
			}
			for id := range s.Nodes {
				if id != who.id && id != a["node"].id {
					delete(s.Nodes, id)
				}
			}
			for n := 0; n < 32; n++ {
				h := &Hold{ID: "draw", RequestID: RequestID("payer", uint64(n), []byte("full")), Node: a["node"].id, FreezeHeight: 1}
				id, _, err := s.selectAuditor(h, "")
				if opt == "no-audit" {
					if err == nil || id != "" {
						t.Fatal("NEGATIVE: review opt-out entered lottery")
					}
				} else if err != nil || id != who.id {
					t.Fatalf("default reviewer draw: %s %v", id, err)
				}
			}
			if s.ExecutionAllowed(who.id, 19000) || s.ReviewAllowed(who.id, 19000) {
				t.Fatal("opt-outs/eligibility outlived TTL")
			}
		})
	}
}

func TestRegistrationRolesMigrationItemEquivalence(t *testing.T) {
	for _, roles := range [][]string{{"model"}, {"auditor"}, {"model", "auditor"}, nil, {"shard-stage"}, {"moe-expert"}} {
		s, a := liveFixture(t)
		s.Version = 9
		setProfileCaps(t, s, a["auditor"], func(p *pb.ResourceProfile) { p.EngineCapabilities.Roles = roles })
		before := append([]byte(nil), s.Nodes[a["auditor"].id].Profile...)
		type result struct{ exec, review, ranged bool }
		observations := map[int64]result{}
		for _, h := range []int64{2, 900, 19000} {
			observations[h] = result{s.ExecutionAllowed(a["auditor"].id, h), s.EligibleAuditor(a["auditor"].id, a["node"].id, h), s.EligibleRangeAuditor(a["auditor"].id, a["node"].id, s.ActiveProfile(a["node"].id, 2), h)}
		}
		s.migrateRegistrationRoles()
		for h, want := range observations {
			got := result{s.ExecutionAllowed(a["auditor"].id, h), s.EligibleAuditor(a["auditor"].id, a["node"].id, h), s.EligibleRangeAuditor(a["auditor"].id, a["node"].id, s.ActiveProfile(a["node"].id, 2), h)}
			if got != want {
				t.Fatalf("roles=%v height=%d got=%+v want=%+v", roles, h, got, want)
			}
		}
		if !bytes.Equal(before, s.Nodes[a["auditor"].id].Profile) {
			t.Fatal("migration rewrote signed profile")
		}
	}
}

func TestRegistrationNineToTenManifestActivation(t *testing.T) {
	s, a := liveFixture(t)
	s.Version = 9
	s.Height = 8
	setProfileCaps(t, s, a["auditor"], func(p *pb.ResourceProfile) { p.EngineCapabilities.Roles = []string{"auditor"} })
	app := &Application{committed: s}
	if err := app.ConfigureMembershipUpgrade(MembershipUpgrade{ChainID: s.Parameters.ChainID, ActivationHeight: 10}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Hash()
	if err := app.activateMembership(s, &abci.FinalizeBlockRequest{Height: 9}); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Hash()
	if !bytes.Equal(before, after) {
		t.Fatal("early migration")
	}
	if err := app.activateMembership(s, &abci.FinalizeBlockRequest{Height: 11}); err == nil {
		t.Fatal("missed activation accepted")
	}
	res, err := app.FinalizeBlock(context.Background(), &abci.FinalizeBlockRequest{Height: 10})
	if err != nil {
		t.Fatal(err)
	}
	if app.staged.Version != 10 || !app.staged.Nodes[a["auditor"].id].NoExec || app.staged.Nodes[a["auditor"].id].NoAudit || res.ConsensusParamUpdates.Version.App != 10 {
		t.Fatal("activation mapping/version")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, app.staged); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadStateWithUpgrade(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := app.staged.Hash()
	got, _ := loaded.Hash()
	if !bytes.Equal(want, got) {
		t.Fatal("restart lost opt-outs")
	}
}

func TestRegistrationRangeMigrationLotteryItemEquivalence(t *testing.T) {
	for _, roles := range [][]string{{"model"}, {"auditor"}, {"model", "auditor"}, nil} {
		for _, item := range []string{"covered", "outside-range", "uncalibrated-device", "stale-liveness"} {
			s, a, h := rangeAuditFixture(t)
			s.Version = 9
			s.Height = 3
			who := a["auditor"]
			setProfileCaps(t, s, who, func(p *pb.ResourceProfile) {
				p.EngineCapabilities.Roles = roles
				switch item {
				case "outside-range":
					p.ShardDynamicRange = true
					p.ShardTotalLayers = 20
					p.ShardEndLayer = 10
					p.ShardHasEmbedding = true
				case "uncalibrated-device":
					p.ActualDevice = "cuda"
				}
			})
			if item == "stale-liveness" {
				s.Height = 10000
			}
			raw, _ := json.Marshal(h)
			first := &Hold{}
			json.Unmarshal(raw, first)
			before, _, beforeErr := s.selectPipelineAuditor(first, "")
			s.migrateRegistrationRoles()
			second := &Hold{}
			json.Unmarshal(raw, second)
			after, _, afterErr := s.selectPipelineAuditor(second, "")
			if before != after || fmt.Sprint(beforeErr) != fmt.Sprint(afterErr) {
				t.Fatalf("roles=%v item=%s: %s/%v -> %s/%v", roles, item, before, beforeErr, after, afterErr)
			}
			if (item != "covered" || !contains(roles, "auditor")) && afterErr == nil {
				t.Fatalf("ineligible item reviewed: %s %v", item, roles)
			}
		}
	}
}

func TestRegistrationOptOutsRespectCoverageTTLLock(t *testing.T) {
	s, a := liveFixture(t)
	who := a["auditor"]
	r := s.Nodes[who.id]
	action := Action{Type: "register_node", Profile: r.Profile, ExpiresHeight: r.ExpiresHeight + 1, NoAudit: true, NoExec: true}
	rejected(t, s, who, action, 4)
	action.ExpiresHeight = 20000
	apply(t, s, who, s.Accounts[who.id].Nonce+1, action, 19000)
	if !s.Nodes[who.id].NoAudit || !s.Nodes[who.id].NoExec {
		t.Fatal("replacement registration lost opt-outs")
	}
}
