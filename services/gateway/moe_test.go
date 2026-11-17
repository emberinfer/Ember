package gateway

import (
	"crypto/ed25519"
	"crypto/rand"
	core "ember/core-go"
	pb "ember/proto"
	"ember/services/ledger"
	"encoding/base64"
	"encoding/json"
	"google.golang.org/protobuf/proto"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeMoeLedger struct {
	ledger.Backend
	profiles                               []*pb.ResourceProfile
	frozen, cancelled, submitted, finished bool
	amount                                 int64
	auth                                   []ledger.MoeAuthorization
	commitment                             []byte
}

func (f *fakeMoeLedger) NodeProfiles() ([]*pb.ResourceProfile, error) { return f.profiles, nil }
func (f *fakeMoeLedger) FreezeMoe(_, _ string, a int64, auth []ledger.MoeAuthorization, i, m uint32, commitment []byte) error {
	f.commitment = append([]byte(nil), commitment...)
	f.frozen = true
	f.amount = a
	f.auth = auth
	return nil
}
func (f *fakeMoeLedger) CancelMoe(string) error { f.cancelled = true; return nil }
func (f *fakeMoeLedger) SubmitMoe(_ string, _ *pb.PipelineExecutionReceipt, a int64) ([]ledger.StageShare, error) {
	f.submitted = a == 1
	return nil, nil
}
func (f *fakeMoeLedger) FinishMoe(_, v string, _ time.Time, _ int64) error {
	f.finished = v == "accept"
	return nil
}
func (f *fakeMoeLedger) State(string) (string, error)                     { return "dispute", nil }
func (f *fakeMoeLedger) ResolveAudit(string) (bool, error)                { return true, nil }
func (f *fakeMoeLedger) RecordedVerdict(string) (*pb.AuditVerdict, error) { return nil, nil }
func (f *fakeMoeLedger) MoeSettlementReady(string) (bool, error)          { return true, nil }
func moeFixture(t *testing.T) (*Gateway, *fakeMoeLedger) {
	t.Helper()
	dir := t.TempDir()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "scheduler.key")
	if err := os.WriteFile(keyFile, key, 0600); err != nil {
		t.Fatal(err)
	}
	fleet := map[string]any{}
	f := &fakeMoeLedger{}
	names := []string{"stage-a", "stage-b", "exp-even-1", "exp-odd-1", "exp-even-2", "exp-odd-2"}
	for i, n := range names {
		fleet[n] = map[string]string{"node_id": n}
		p := &pb.ResourceProfile{NodeId: n, ChainId: MoeProfile}
		if i < 2 {
			p.MoeUnitKind = "attention"
			p.ShardStartLayer = uint32(i * 8)
			p.ShardEndLayer = uint32((i + 1) * 8)
		} else {
			p.MoeUnitKind = "expert"
			for l := 0; l < 16; l++ {
				for e := i - 2; e < 64; e += 4 {
					p.MoeUnits = append(p.MoeUnits, &pb.MoeUnitRegistration{Layer: uint32(l), Expert: uint32(e)})
				}
			}
		}
		f.profiles = append(f.profiles, p)
	}
	raw, _ := json.Marshal(fleet)
	os.WriteFile(filepath.Join(dir, "fleet.json"), raw, 0600)
	return &Gateway{Ledger: f, ChainID: MoeProfile, Profile: "v1", Price: 1, Moe: &MoeConfig{Python: "/bin/sh", WorkDir: dir, FleetDir: dir, SchedulerKeyFile: keyFile}}, f
}
func TestMoeCoverageAndFinisher(t *testing.T) {
	g, f := moeFixture(t)
	a, e := g.moeAuthorizations(f)
	if e != nil || len(a) != 6 {
		t.Fatalf("coverage: %v", e)
	}
	g.finishMoe(f, "hold")
	if !f.finished {
		t.Fatal("not finished")
	}
	f.profiles = f.profiles[1:]
	if _, e = g.moeAuthorizations(f); e == nil {
		t.Fatal("missing stage accepted")
	}
}
func TestServeMoeExecutor(t *testing.T) {
	for _, bad := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "bad receipt"}[bad], func(t *testing.T) {
			g, f := moeFixture(t)
			receipt := &pb.PipelineExecutionReceipt{RequestId: "hold", InputTokens: 3, OutputTokens: 1, Moe: &pb.MoeExecutionData{}}
			if bad {
				receipt.InputTokens = 4
			}
			raw, _ := proto.Marshal(receipt)
			// sh ignores the module invocation; this script supplies both subprocess protocols.
			script := `#!/bin/sh
if [ "$2" = tokenize_prompt ]; then
 echo '{"input_tokens":3,"tokenizer_version":"test"}'
else
 echo '{"type":"token","text":"Paris"}'
 echo '{"type":"result","output_tokens":1,"receipt_b64":"` + base64.StdEncoding.EncodeToString(raw) + `"}'
fi
`
			path := filepath.Join(g.Moe.WorkDir, "executor")
			os.WriteFile(path, []byte(script), 0700)
			g.Moe.Python = path
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			// Keep the finisher deterministic in this test by invoking it separately above.
			g.serveMoe(w, r, request{Model: MoeProfile, MaxTokens: 2, Stream: true}, "prompt", "payer", "hold")
			if !f.frozen || f.amount != 2 || len(f.auth) != 6 {
				t.Fatal("freeze arguments")
			}
			c := &pb.ExecutionCommitment{}
			if len(f.commitment) == 0 || proto.Unmarshal(f.commitment, c) != nil || core.Verify(c) != nil || c.RequestId != "hold" || c.InputTokens != 3 || c.MaxOutputTokens != 2 {
				t.Fatal("invalid freeze pricing commitment")
			}
			if bad {
				if !f.cancelled || f.submitted || !strings.Contains(w.Body.String(), "event: error") {
					t.Fatal(w.Body.String())
				}
			} else {
				if !f.submitted || !strings.Contains(w.Body.String(), `"content":"Paris"`) || !strings.Contains(w.Body.String(), "data: [DONE]") {
					t.Fatal(w.Body.String())
				}
			}
		})
	}
}

func TestMoeContextLimitAdmission(t *testing.T) {
	g, book := moeFixture(t)
	path := filepath.Join(g.Moe.WorkDir, "tokenizer")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho '{\"input_tokens\":3,\"tokenizer_version\":\"test\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	g.Moe.Python = path
	g.ContextLimit = 4
	w := httptest.NewRecorder()
	g.serveMoe(w, httptest.NewRequest("POST", "/v1/chat/completions", nil), request{Model: MoeProfile, MaxTokens: 2, Stream: true}, "prompt", "payer", "hold")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "input_tokens 3 + max_tokens 2") || !strings.Contains(w.Body.String(), "4 tokens") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	if book.frozen {
		t.Fatal("over-limit MoE request froze funds")
	}
}
