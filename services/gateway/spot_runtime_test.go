package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "ember/core-go"
	pb "ember/proto"
	"ember/services/auditqueue"
	chain "ember/services/chain"
	"ember/services/jsonspool"
	"ember/services/ledger"
	"ember/services/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

func TestPipelineSpotRuntimeTransfer(t *testing.T) {
	python := os.Getenv("EMBER_AUDIT_TEST_PYTHON")
	if python == "" {
		t.Fatal("EMBER_AUDIT_TEST_PYTHON required")
	}
	root, _ := filepath.Abs("../..")
	raw, err := os.ReadFile(filepath.Join(root, "node/auditor/testdata/pipeline-record-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Job      json.RawMessage        `json:"job"`
		Manifest PipelineSpotCommitment `json:"manifest"`
	}
	if err = json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	original, err := os.CreateTemp(t.TempDir(), "source")
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	spoolJob := append(append([]byte(nil), golden.Job[:len(golden.Job)-1]...), []byte(`,"genesis":""}`)...)
	spoolJob = bytes.Replace(spoolJob, []byte(`"openings":[{`), []byte(`"openings":[{ `), 1)
	original.Write(spoolJob)
	source, err := jsonspool.Index(original)
	if err != nil {
		t.Fatal(err)
	}
	q, err := auditqueue.Open(filepath.Join(t.TempDir(), "audit"))
	if err != nil {
		t.Fatal(err)
	}
	id := golden.Manifest.RequestID
	if err = q.PutStream(id, func(w io.Writer) error { return WritePipelineAudit(w, spoolJob, source) }); err != nil {
		t.Fatal(err)
	}
	final, err := q.OpenJob(id)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close()
	indexed, err := jsonspool.Index(final)
	if err != nil {
		t.Fatal(err)
	}
	var receiptHex string
	indexed.Decode("pipeline_receipt", &receiptHex)
	receiptRaw, _ := hex.DecodeString(receiptHex)
	receipt := new(pb.PipelineExecutionReceipt)
	proto.Unmarshal(receiptRaw, receipt)
	e := &PipelineExecution{Source: indexed, Receipt: receipt}
	transient, err := BuildPipelineSpotCommitment(&PipelineExecution{Source: source, Receipt: receipt}, id, "tiny")
	if err != nil || transient.CommitmentRoot == golden.Manifest.CommitmentRoot {
		t.Fatal("whitespace fixture must differ before compact", err)
	}
	m, err := BuildDurablePipelineSpotCommitment(q, id, "tiny", receipt)
	if err != nil {
		t.Fatal(err)
	}
	if m.CommitmentRoot != golden.Manifest.CommitmentRoot {
		t.Fatal("final compacted spool root differs from B golden")
	}
	digest, _ := hex.DecodeString(m.CommitmentRoot)
	action, err := ledger.PipelineSpotAction(id, receipt, 6, 2, digest, m.SpotStepCount)
	if err != nil || !bytes.Equal(action.CommitmentRoot, digest) || action.SpotStepCount != 2 {
		t.Fatal("submit action missing root/count", err)
	}
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	store, err := OpenSpotStore(filepath.Join(t.TempDir(), "spot"), key)
	if err != nil {
		t.Fatal(err)
	}
	auditorPub, auditorKey, _ := ed25519.GenerateKey(rand.Reader)
	auditor := core.NodeID(auditorPub)
	c := &chain.SpotChallenge{RequestID: id, Auditor: auditor, StepSet: []uint64{1}, ChallengeHeight: 10, Round: "1", Deadline: 130}
	delivery, err := store.Prepare(e, m, c, 11)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Absence(c, 131); err == nil {
		t.Fatal("absence signed over timely record")
	}
	var full bytes.Buffer
	if err = WritePipelineSpotAudit(&full, indexed, delivery, "escalation"); err != nil {
		t.Fatal(err)
	}
	var fullJob map[string]json.RawMessage
	if err = json.Unmarshal(full.Bytes(), &fullJob); err != nil {
		t.Fatal(err)
	}
	var fullInvocations []json.RawMessage
	if err = json.Unmarshal(fullJob["invocations"], &fullInvocations); err != nil || len(fullInvocations) != 4 {
		t.Fatal("full channel lost invocations", err)
	}
	retryChallenge := *c
	retryChallenge.Round = "2"
	retryChallenge.ChallengeHeight = 100
	retry, err := store.Prepare(e, m, &retryChallenge, 101)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := json.Marshal(delivery.Packages)
	retryJSON, _ := json.Marshal(retry.Packages)
	if !bytes.Equal(firstJSON, retryJSON) {
		t.Fatal("round two changed package set")
	}
	receiptAtt, err := store.Receipt(id, 10)
	if err != nil || !ed25519.Verify(pub, chain.SpoolSigningBytes(receiptAtt), receiptAtt.Signature) {
		t.Fatal("receipt retrieval/signature", err)
	}
	stateDir := t.TempDir()
	os.MkdirAll(filepath.Join(stateDir, "spot-context"), 0700)
	os.MkdirAll(filepath.Join(stateDir, "mirror"), 0700)
	os.WriteFile(filepath.Join(stateDir, "seed"), []byte(hex.EncodeToString(auditorKey.Seed())), 0600)
	contextData := map[string]any{"kind": "pipeline", "request_id": id, "invocation_request_id": "request", "total_steps": 2, "profile_id": "tiny", "request_tokens": []int{1, 2}, "claimed_tokens": []int{7, 8}, "prompt": "fixture", "seed": 0, "metric": "legacy-ranked-v1", "epsilon": 0.01, "gateway_public": hex.EncodeToString(pub)}
	writeJSON := func(path string, v any) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON(filepath.Join(stateDir, "spot-context", id+".json"), contextData)
	hold := &chain.Hold{ID: id, State: "dispute", Auditor: auditor, ReceiptRef: hex.EncodeToString(make([]byte, 32)), AuditProof: hex.EncodeToString(make([]byte, 32)), Pipeline: &chain.PipelineHold{SpotStepCount: 2, CommitmentRoot: digest, Spot: c}}
	writeJSON(filepath.Join(stateDir, "hold.json"), hold)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relay := tunnel.New()
	relay.Legacy = true
	relay.SchedulerKey = key
	server := grpc.NewServer()
	pb.RegisterNodeTunnelServer(server, relay)
	go server.Serve(listener)
	defer server.Stop()
	peer := exec.Command(python, "-m", "agent.spot_runtime_peer", listener.Addr().String(), core.NodeID(pub), stateDir)
	peer.Env = append(os.Environ(), "PYTHONPATH="+root+"/node:"+root+"/proto:"+root+"/core-py")
	logFile, err := os.Create(filepath.Join(stateDir, "peer.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	peer.Stdout = logFile
	peer.Stderr = logFile
	if err = peer.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { peer.Process.Kill(); peer.Wait() }()
	deadline := time.Now().Add(20 * time.Second)
	for !relay.Online(auditor) {
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(logFile.Name())
			t.Fatalf("peer not online: %s", b)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for variantIndex, variant := range []string{"accept", "response", "token"} {
		t.Run(variant, func(t *testing.T) {
			c.ChallengeHeight = int64(10 + variantIndex)
			delivery, err = store.Prepare(e, m, c, 15)
			if err != nil {
				t.Fatal(err)
			}
			writeJSON(filepath.Join(stateDir, "hold.json"), hold)
			b, _ := json.Marshal(delivery)
			var changed map[string]any
			json.Unmarshal(b, &changed)
			packages := changed["packages"].([]any)
			p := packages[0].(map[string]any)
			if variant == "response" {
				p["last_response"] = p["last_response"].(string) + "00"
			}
			if variant == "token" {
				openingRaw, _ := hex.DecodeString(p["opening"].(string))
				var value map[string]any
				json.Unmarshal(openingRaw, &value)
				value["token"] = 9
				openingRaw, _ = json.Marshal(value)
				p["opening"] = hex.EncodeToString(openingRaw)
			}
			// Force the real AuditTransfer chunked path with inert transport padding.
			payload, _ := json.Marshal(map[string]any{"kind": "spotcheck", "mode": "normal", "spot_delivery": changed, "padding": string(bytes.Repeat([]byte("x"), 2<<20))})
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var result struct {
				VerdictKind string `json:"verdict_kind"`
				PerStep     []struct {
					Outcome string `json:"outcome"`
				} `json:"per_step"`
				Envelope string `json:"envelope"`
			}
			err := relay.AuditReader(ctx, auditor, id, "runtime-"+variant, bytes.NewReader(payload), int64(len(payload)), func(frame *pb.TunnelFrame) error {
				if frame.Kind != "audit_result" {
					return fmt.Errorf("%s: %s", frame.Kind, frame.Payload)
				}
				return json.Unmarshal(frame.Payload, &result)
			})
			if err != nil {
				b, _ := os.ReadFile(logFile.Name())
				t.Fatalf("transfer/replay: %v\n%s", err, b)
			}
			if variant == "accept" {
				inline, _ := json.Marshal(map[string]any{"kind": "spotcheck", "mode": "normal", "spot_delivery": changed})
				err = relay.AuditReader(ctx, auditor, id, "inline-retry", bytes.NewReader(inline), int64(len(inline)), func(frame *pb.TunnelFrame) error {
					if frame.Kind != "audit_result" {
						return fmt.Errorf("inline retry %s: %s", frame.Kind, frame.Payload)
					}
					return json.Unmarshal(frame.Payload, &result)
				})
				if err != nil {
					t.Fatal("completed spot retry must reuse signed result", err)
				}
			}
			// A separate request ID avoids Agent's deliberate cached-verdict behavior.
			if result.VerdictKind != "accept" && variant == "accept" {
				t.Fatalf("accept result: %+v", result)
			}
			if variant != "accept" && result.PerStep[0].Outcome != "binding_fail" {
				t.Fatalf("tamper missed: %+v", result)
			}
			envelopeRaw, _ := base64.StdEncoding.DecodeString(result.Envelope)
			_, signer, err := chain.DecodeAndVerifyVersion(envelopeRaw, 14)
			if err != nil || signer != auditor {
				t.Fatal("C action envelope", err)
			}
		})
	}
}

// Apply's spot_unopened handler calls A's private verifySpool directly.
func TestGoAbsenceToCAndChain(t *testing.T) {
	python := os.Getenv("EMBER_AUDIT_TEST_PYTHON")
	if python == "" {
		t.Fatal("EMBER_AUDIT_TEST_PYTHON required")
	}
	public, key, _ := ed25519.GenerateKey(rand.Reader)
	auditorPublic, auditorKey, _ := ed25519.GenerateKey(rand.Reader)
	gatewayID, auditorID := core.NodeID(public), core.NodeID(auditorPublic)
	store, err := OpenSpotStore(t.TempDir(), key)
	if err != nil {
		t.Fatal(err)
	}
	c := &chain.SpotChallenge{RequestID: strings.Repeat("ac", 32), Auditor: auditorID, ChallengeHeight: 10, Deadline: 130, Round: "1", StepSet: []uint64{1}}
	if _, err = store.Absence(c, 130); err == nil {
		t.Fatal("premature absence")
	}
	att, err := store.Absence(c, 131)
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]any{"attestation": att, "steps": c.StepSet, "deadline": c.Deadline, "challenge": c, "gateway_public": hex.EncodeToString(public)})
	cmd := exec.Command(python, "../../node/spot_absence_peer.py")
	cmd.Env = append(os.Environ(), "PYTHONPATH=../../node:../../proto:../../core-py")
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("C unopened claim: %v %s", err, output)
	}
	var action chain.Action
	if err = json.Unmarshal(output, &action); err != nil {
		t.Fatal(err)
	}
	profile := &pb.ResourceProfile{NodeId: gatewayID, EngineCapabilities: &pb.EngineCapabilities{Roles: []string{"gateway"}}}
	core.Sign(profile, key)
	rawProfile, _ := proto.Marshal(profile)
	state := &chain.State{Version: 14, Parameters: chain.Parameters{ChainID: "spot-harness"}, Accounts: map[string]*chain.Account{auditorID: {}, gatewayID: {}}, Nodes: map[string]*chain.NodeRegistration{gatewayID: {NodeID: gatewayID, Profile: rawProfile, ExpiresHeight: 1000}}, Holds: map[string]*chain.Hold{c.RequestID: {ID: c.RequestID, State: "dispute", Auditor: auditorID, Pipeline: &chain.PipelineHold{Spot: c}}}}
	transaction, err := chain.Sign("spot-harness", auditorKey, 1, action)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = state.Apply(transaction, 131); err != nil {
		t.Fatal("A verifySpool/Apply rejected Go -> C claim", err)
	}
	if state.Holds[c.RequestID].Pipeline.Spot.Accusation == nil {
		t.Fatal("absence accusation not persisted")
	}
}

func TestSpotRetentionAndDelivery(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	store, _ := OpenSpotStore(t.TempDir(), key)
	q, _ := auditqueue.Open(t.TempDir())
	q.Put("hold", []byte(`{"kind":"pipeline"}`))
	h := &chain.Hold{ID: "hold", State: "dispute", Pipeline: &chain.PipelineHold{Spot: &chain.SpotChallenge{}}}
	if removed, err := store.Cleanup(q, h); err != nil || removed {
		t.Fatal("selected unsettled data removed", err)
	}
	h.State = "settled"
	if removed, err := store.Cleanup(q, h); err != nil || !removed {
		t.Fatal("terminal data retained", err)
	}
	q.Put("hold", []byte(`{}`))
	h.State = "dispute"
	h.Unaudited = true
	if removed, _ := store.Cleanup(q, h); removed {
		t.Fatal("unsigned/unfinalized lottery deleted data")
	}
	h.Sampling = &chain.SamplingRecord{}
	if removed, err := store.Cleanup(q, h); err != nil || !removed {
		t.Fatal("final unselected retained", err)
	}
	r := &SpotDelivery{Kind: "opening", RequestID: "hold", ChallengeHeight: 10, Delivered: map[string]bool{}}
	if store.Delivered(r, "auditor") {
		t.Fatal("premature delivered")
	}
	if err := store.MarkDelivered(r, "auditor"); err != nil {
		t.Fatal(err)
	}
	if !store.Delivered(r, "auditor") || store.Delivered(r, "replacement") {
		t.Fatal("delivery recipient isolation")
	}
}
