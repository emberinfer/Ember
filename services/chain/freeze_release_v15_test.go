package chain

import (
	"bytes"
	core "ember/core-go"
	pb "ember/proto"
	"encoding/json"
	"fmt"
	"google.golang.org/protobuf/proto"
	"reflect"
	"testing"
)

func TestV15ExposureTwoPhases(t *testing.T) {
	for _, version := range []uint64{14, 15} {
		for _, state := range []string{"frozen", "dispute", "settled"} {
			for _, shape := range []string{"whole", "pipeline", "moe"} {
				t.Run(fmt.Sprintf("v%d/%s/%s", version, state, shape), func(t *testing.T) {
					s, _ := testState(t)
					s.Version = version
					h := &Hold{ID: "x", Node: "n", Amount: 100, Actual: 12, State: state}
					credits := []core.StageShare{{NodeID: "n", Amount: 5}, {NodeID: "other", Amount: 80}, {NodeID: "n", Amount: 7}}
					switch shape {
					case "pipeline":
						h.Node = ""
						h.Pipeline = &PipelineHold{Authorizations: []PipelineAuthorization{{NodeID: "n", Reserved: 40}, {NodeID: "other", Reserved: 500}, {NodeID: "n", Reserved: 60}}, Credits: credits}
					case "moe":
						h.Node = ""
						h.Moe = &MoeHold{Authorizations: []MoeAuthorization{{NodeID: "n", Reserved: 40}, {NodeID: "other", Reserved: 500}, {NodeID: "n", Reserved: 60}}, Credits: credits}
					}
					s.Holds[h.ID] = h
					want := int64(100)
					if state == "settled" {
						want = 0
					} else if state == "dispute" && version >= 15 {
						want = 12
					}
					before, _ := json.Marshal(s)
					if got := s.exposure("n"); got != want {
						t.Fatalf("exposure %d want %d", got, want)
					}
					after, _ := json.Marshal(s)
					if !bytes.Equal(before, after) {
						t.Fatal("exposure mutated state")
					}
				})
			}
		}
	}
}

func TestV15FateSettlement(t *testing.T) {
	for _, version := range []uint64{14, 15} {
		for _, kind := range []string{"U", "N", "A", "unassigned", "pending", "unaudited_only"} {
			for _, grace := range []int64{0, 3} {
				for _, height := range []int64{10, 12, 13, 100} {
					t.Run(fmt.Sprintf("v%d/%s/grace%d/h%d", version, kind, grace, height), func(t *testing.T) {
						s, a := testState(t)
						s.Version = version
						s.Parameters.EarlySettleGraceBlocks = grace
						h := sampleHold(s, a, 0)
						h.SettleHeight = 100
						h.FateHeight = 10
						switch kind {
						case "U":
							h.Unaudited = true
							h.Sampling = &SamplingRecord{Sampled: false}
						case "N":
							h.Unaudited = true
							h.NoAuditor = true
							h.Sampling = &SamplingRecord{Sampled: true}
						case "A":
							h.Auditor = a["auditor"].id
							h.AuditVerdict = "accept"
						case "pending":
							h.Auditor = a["auditor"].id
							h.Sampling = &SamplingRecord{Sampled: true}
						case "unaudited_only":
							h.Unaudited = true
						}
						err := s.applyAction(a["coordinator"].id, Action{Type: "settle", HoldID: h.ID, Outcome: "accept"}, height)
						known := kind == "U" || kind == "N" || kind == "A"
						want := (known || kind == "unaudited_only") && (height >= 100 || (version >= 15 && known && height >= 10+grace))
						if (err == nil) != want {
							t.Fatalf("settlement err=%v want success=%v", err, want)
						}
					})
				}
			}
		}
	}
}

func TestV15NoAuditorAssignment(t *testing.T) {
	for _, version := range []uint64{14, 15} {
		for _, badEntropy := range []bool{false, true} {
			t.Run(fmt.Sprintf("v%d/badEntropy%v", version, badEntropy), func(t *testing.T) {
				s, a := samplingState(t)
				s.Version = version
				s.Auditors = map[string]bool{a["node"].id: true}
				h := sampleHold(s, a, 0)
				h.SamplingPolicy.RatePPM = 1000000
				h.SettleHeight = 100
				if badEntropy {
					s.BlockHashes = nil
				}
				before := map[string]NodeAuditStats{}
				for k, v := range s.NodeAuditStats {
					before[k] = v
				}
				raw, err := Sign(s.Parameters.ChainID, a["coordinator"].key, 1, Action{Type: "assign_audit", HoldID: h.ID})
				if err != nil {
					t.Fatal(err)
				}
				_, err = s.Apply(raw, 4)
				h = s.Holds[h.ID]
				if version < 15 || badEntropy {
					if err == nil || h.NoAuditor || h.FateHeight != 0 || h.Sampling != nil {
						t.Fatalf("failed assignment changed state: %v %+v", err, h)
					}
					return
				}
				if err != nil || !h.NoAuditor || !h.Unaudited || h.FateHeight != 4 || h.Sampling == nil || !h.Sampling.Sampled || h.Auditor != "" {
					t.Fatalf("no auditor fate: %v %+v", err, h)
				}
				if len(h.AuditStatsEvents) != 2 || h.AuditStatsEvents[1].Kind != "no_auditor" || h.AuditStatsEvents[1].Node != "" {
					t.Fatal(h.AuditStatsEvents)
				}
				if !reflect.DeepEqual(before, s.NodeAuditStats) {
					t.Fatal("no auditor altered stats")
				}
				apply(t, s, a["coordinator"], 2, Action{Type: "settle", HoldID: h.ID, Outcome: "accept"}, 4)
				if !reflect.DeepEqual(before, s.NodeAuditStats) {
					t.Fatal("no auditor settlement altered stats")
				}
			})
		}
	}
}

func TestV15FateHeights(t *testing.T) {
	for _, version := range []uint64{14, 15} {
		for _, kind := range []string{"U", "A"} {
			t.Run(fmt.Sprintf("v%d/%s", version, kind), func(t *testing.T) {
				s, a := samplingState(t)
				s.Version = version
				h := sampleHold(s, a, 0)
				var err error
				if kind == "U" {
					h.SamplingPolicy.RatePPM = 0
					err = s.applyAction(a["coordinator"].id, Action{Type: "assign_audit", HoldID: h.ID}, 4)
				} else {
					h.Auditor = a["auditor"].id
					err = s.recordAuditOutcome(h, h.Auditor, "accept", "ref", 4)
				}
				if err != nil {
					t.Fatal(err)
				}
				want := int64(0)
				if version >= 15 {
					want = 4
				}
				if h.FateHeight != want {
					t.Fatalf("fate height=%d want %d", h.FateHeight, want)
				}
				if kind == "U" {
					if err = s.applyAction(a["coordinator"].id, Action{Type: "assign_audit", HoldID: h.ID}, 5); err != nil {
						t.Fatal(err)
					}
					if h.FateHeight != want {
						t.Fatal("retry moved fate height")
					}
				}
			})
		}
	}
}

func TestV15RejectSlashUsesActual(t *testing.T) {
	for _, version := range []uint64{14, 15} {
		for _, actual := range []int64{12, 300} {
			t.Run(fmt.Sprintf("v%d/actual%d", version, actual), func(t *testing.T) {
				s, a := testState(t)
				s.Version = version
				s.Parameters.SlashFloorBPS = 0
				s.Parameters.Misjudge.CapBlockBPS = 10000
				h := sampleHold(s, a, 0)
				h.Amount = 1000
				h.Actual = actual
				h.AuditVerdict = "reject"
				h.Auditor = a["auditor"].id
				verdict := &pb.AuditVerdict{AuditorId: h.Auditor, Verdict: "reject", Offense: &pb.OffenseGrade{Class: "objective", Fault: "hash"}}
				if err := core.Sign(verdict, a["auditor"].key); err != nil {
					t.Fatal(err)
				}
				h.VerdictBytes, _ = proto.Marshal(verdict)
				h.SettleHeight = 100
				stake := s.Accounts[h.Node].Stake
				if err := s.applyAction(a["coordinator"].id, Action{Type: "settle", HoldID: h.ID, Outcome: "reject"}, 4); err != nil {
					t.Fatal(err)
				}
				want := min(stake, 20*actual)
				if got := stake - s.Accounts[h.Node].Stake; got != want {
					t.Fatalf("slash %d want %d", got, want)
				}
				if len(h.Slashes) != 1 || h.Slashes[0].Revenue != actual {
					t.Fatal(h.Slashes)
				}
			})
		}
	}
}

func TestV15LegacyJSONAndGraceParameter(t *testing.T) {
	raw, _ := json.Marshal(Hold{})
	for _, key := range []string{"no_auditor", "fate_height"} {
		if bytes.Contains(raw, []byte(key)) {
			t.Fatal("legacy hold encoding changed")
		}
	}
	raw, _ = json.Marshal(Parameters{})
	if bytes.Contains(raw, []byte("early_settle_grace_blocks")) {
		t.Fatal("legacy parameters encoding changed")
	}
	var p Parameters
	if err := json.Unmarshal([]byte(`{"early_settle_grace_blocks":60}`), &p); err != nil || p.EarlySettleGraceBlocks != 60 {
		t.Fatal(err, p)
	}
	raw, _ = json.Marshal(p)
	if !bytes.Contains(raw, []byte(`"early_settle_grace_blocks":60`)) {
		t.Fatal(string(raw))
	}
}

func TestV15SelectionFailureIsNotNoAuditor(t *testing.T) {
	s, a := samplingState(t)
	s.Version = 15
	h := sampleHold(s, a, 0)
	// Sampling is already final, so this fails inside selectAuditor, not sampling.
	h.Sampling = &SamplingRecord{Sampled: true}
	h.RequestID = ""
	raw, err := Sign(s.Parameters.ChainID, a["coordinator"].key, 1, Action{Type: "assign_audit", HoldID: h.ID})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(s)
	if _, err = s.Apply(raw, 4); err == nil {
		t.Fatal("invalid request identity accepted as empty pool")
	}
	after, _ := json.Marshal(s)
	if !bytes.Equal(before, after) {
		t.Fatal("failed selection mutated state")
	}
}

func TestV15RegisteredSingleNodeNoAuditor(t *testing.T) {
	s, a := samplingState(t)
	s.Version = 15
	s.Parameters.RegisteredAuditors = true
	h := sampleHold(s, a, 0)
	h.SamplingPolicy.RatePPM = 1000000
	h.SettleHeight = 100
	apply(t, s, a["coordinator"], 1, Action{Type: "assign_audit", HoldID: h.ID}, 4)
	h = s.Holds[h.ID]
	if !h.NoAuditor || !h.Unaudited || h.Sampling == nil || !h.Sampling.Sampled {
		t.Fatalf("single profile fate %+v", h)
	}
	apply(t, s, a["coordinator"], 2, Action{Type: "settle", HoldID: h.ID, Outcome: "accept"}, 4)
}
