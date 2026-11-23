package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"ember/services/ledger"
	"ember/services/tunnel"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "ember/core-go"
	pb "ember/proto"
	chain "ember/services/chain"
	"ember/services/scheduler"
)

func imageModel() chain.Model {
	return chain.Model{ID: "ember-image", Kind: "text-to-image", ImageProfile: &core.ImageProfile{ExecutionProfile: "diffusers-sd1.5-fp32-v1", MaxSteps: 100, PriceTable: []core.ImagePrice{{Step: 560000000000, Text: 38000000000, VAE: 400000000000}}, Components: []core.ImageComponent{{Role: "unet", Params: 860000000}, {Role: "text_encoder", Params: 123000000}, {Role: "vae", Params: 83700000}}}}
}
func imageHold() *chain.Hold {
	return &chain.Hold{ID: "hold", ModelID: "ember-image", Node: "executor", Payer: "payer", Amount: 858, Actual: 858, State: "frozen", FreezeHeight: 1, ExpireHeight: 7001, Image: &chain.ImageHold{MultiplierPPM: 1000000, Request: chain.ImageRequest{Steps: 20}}}
}
func imageStore(t *testing.T) (*ImageStore, ed25519.PrivateKey) {
	t.Helper()
	_, key, e := ed25519.GenerateKey(nil)
	if e != nil {
		t.Fatal(e)
	}
	s, e := OpenImageStore(t.TempDir(), key)
	if e != nil {
		t.Fatal(e)
	}
	return s, key
}
func imageSub(data []byte) chain.ImageSubmission {
	sum := sha256.Sum256(data)
	pixel := sha256.Sum256([]byte("pixels"))
	final := sha256.Sum256([]byte("latent"))
	return chain.ImageSubmission{ImageSHA256: hex.EncodeToString(sum[:]), PixelSHA256: hex.EncodeToString(pixel[:]), FinalLatentHash: hex.EncodeToString(final[:]), ImageBytes: uint64(len(data))}
}
func TestImageWholeRoutingNeutralAffinity(t *testing.T) {
	m := imageModel()
	manifest := []byte("manifest")
	candidate := func(id string) scheduler.Candidate {
		return scheduler.Candidate{Online: true, Stake: 1000, Reputation: 1, Prefix: "session", Profile: &pb.ResourceProfile{NodeId: id, ChainId: m.ID, ExecutionProfileVer: m.ImageProfile.ExecutionProfile, ManifestRoot: manifest}}
	}
	whole := candidate("whole")
	shard := candidate("shard")
	shard.Profile.ShardTotalLayers = 28 // even malformed/empty shards cannot sneak in
	text := candidate("text")
	text.Profile.ExecutionProfileVer = "text-v1"
	other := candidate("other")
	other.Profile.ChainId = "ember-text"
	moe := candidate("moe")
	moe.Profile.MoeUnitKind = "expert"
	g := &Gateway{Manifest: manifest, Pipeline: &PipelineConfig{}, Moe: &MoeConfig{}, Candidates: func() []scheduler.Candidate { return []scheduler.Candidate{shard, text, other, moe, whole} }}
	for i := uint64(0); i < 20; i++ {
		n, e := g.ChooseImage(m, i)
		if e != nil || n.NodeID != "whole" || n.Affinity {
			t.Fatal(n, e)
		}
	}
	g.Candidates = func() []scheduler.Candidate { return []scheduler.Candidate{shard, text, other, moe} }
	if _, e := g.ChooseImage(m, 0); e == nil {
		t.Fatal("routed into non-image whole pool")
	}
	// Historical scheduler text affinity remains unchanged.
	n, e := scheduler.Choose(scheduler.Request{Chain: m.ID, Profile: m.ImageProfile.ExecutionProfile, Manifest: manifest, Prefix: "session"}, []scheduler.Candidate{whole})
	if e != nil || !n.Affinity {
		t.Fatal(n, e)
	}
}
func TestImageBudgetT1Formula(t *testing.T) {
	m, h := imageModel(), imageHold()
	for _, v := range []struct{ ppm, amount int64 }{{1000000, 858}, {2000000, 1716}, {800000, 686}} {
		h.Image.MultiplierPPM = v.ppm
		h.Amount = v.amount
		h.Actual = v.amount
		if e := ValidateImageBudget(m, h); e != nil {
			t.Fatal(e)
		}
		h.Actual++
		if ValidateImageBudget(m, h) == nil {
			t.Fatal("wrong actual accepted")
		}
		h.Actual--
		h.Amount++
		if ValidateImageBudget(m, h) == nil {
			t.Fatal("wrong freeze accepted")
		}
	}
}
func TestImageSpoolLargeRestartAndAbsence(t *testing.T) {
	for _, size := range []int{440000, 1100000} {
		t.Run(string(rune(size)), func(t *testing.T) {
			s, key := imageStore(t)
			data := bytes.Repeat([]byte{0, 1, 42, 255}, size/4)
			h := imageHold()
			sub := imageSub(data)
			d, e := s.Put(h, sub, bytes.NewReader(data), 2)
			if e != nil {
				t.Fatal(e)
			}
			if d.Receipt.Height != 2 || d.Receipt.ChallengeHeight != 1 || d.Receipt.RequestID != h.ID || !d.Receipt.Received || !bytes.Equal(d.Receipt.StepSetHash, chain.ImageDeliveryBinding(&sub)) || !ed25519.Verify(key.Public().(ed25519.PublicKey), chain.SpoolSigningBytes(d.Receipt), d.Receipt.Signature) {
				t.Fatal("incomplete receipt")
			}
			reopened := &ImageStore{queue: s.queue, key: key}
			got, e := reopened.Get(h.ID)
			if e != nil || !bytes.Equal(got.Image, data) {
				t.Fatal("restart image mismatch", e)
			}
			again, e := s.Put(h, sub, bytes.NewReader(data), 3)
			if e != nil || chain.SpoolReceiptHash(again.Receipt) != chain.SpoolReceiptHash(d.Receipt) {
				t.Fatal("retry changed receipt", e)
			}
			h.Image.Submission = &sub
			h.Image.Delivery = &chain.SpotChallenge{Deadline: 4}
			if _, e = s.Absence(h, 5); e == nil {
				t.Fatal("false absence")
			}
			h.ID = "missing"
			a, e := s.Absence(h, 5)
			if e != nil || a.Received || !bytes.Equal(a.StepSetHash, chain.ImageDeliveryBinding(&sub)) {
				t.Fatal(a, e)
			}
			h.ID = "bad"
			sub.ImageBytes++
			if _, e = s.Put(h, sub, bytes.NewReader(data), 2); e == nil {
				t.Fatal("bad length accepted")
			}
		})
	}
}

type imageBook struct {
	h         *chain.Hold
	m         chain.Model
	height    int64
	actions   []string
	submitted int
	fate      string
	fail      bool
}

func (b *imageBook) Model(context.Context, string) (chain.Model, error) { return b.m, nil }
func (b *imageBook) Hold(context.Context, string) (*chain.Hold, int64, error) {
	return b.h, b.height, nil
}
func (b *imageBook) Quote(context.Context, string, chain.ImageRequest) (int64, error) {
	return 858, nil
}
func (b *imageBook) Freeze(_ context.Context, a chain.Action) error {
	b.h.ID = a.HoldID
	b.h.ModelID = a.ModelID
	b.h.Node = a.Node
	b.h.Payer = a.Payer
	b.h.Amount = a.Amount
	b.h.Actual = a.Amount
	b.h.Image.Request = *a.ImageRequest
	return nil
}
func (b *imageBook) Submit(context.Context, []byte) error {
	b.submitted++
	b.h.State = "dispute"
	b.h.SubmitHeight = b.height
	b.h.Image.Delivery = &chain.SpotChallenge{}
	return nil
}
func (b *imageBook) Apply(_ context.Context, a chain.Action) error {
	if b.fail {
		return errors.New("send failed")
	}
	b.actions = append(b.actions, a.Type)
	switch a.Type {
	case "image_received":
		b.h.Image.Delivery.Received = true
	case "assign_audit":
		if b.fate == "unaudited" || b.fate == "noauditor" {
			b.h.Unaudited = true
			b.h.NoAuditor = b.fate == "noauditor"
		} else {
			b.h.Auditor = "auditor"
		}
	case "settle":
		b.h.State = "settled"
	}
	return nil
}
func TestImageSubmitBudgetReceiptAndImmediateFate(t *testing.T) {
	for _, fate := range []string{"unaudited", "noauditor", "accept", "reject"} {
		t.Run(fate, func(t *testing.T) {
			s, key := imageStore(t)
			h := imageHold()
			h.Node = core.NodeID(key.Public().(ed25519.PublicKey))
			sub := imageSub([]byte("image"))
			d, e := s.Put(h, sub, bytes.NewReader([]byte("image")), 2)
			if e != nil {
				t.Fatal(e)
			}
			sub.SpoolReceiptHash = chain.SpoolReceiptHash(d.Receipt)
			raw, e := chain.Sign("domain", key, 1, chain.Action{Type: "submit_image", HoldID: h.ID, ImageSubmission: &sub})
			if e != nil {
				t.Fatal(e)
			}
			b := &imageBook{h: h, m: imageModel(), height: 3, fate: fate}
			service := &ImageService{Backend: b, Store: s, ChainID: "domain"}
			h.Amount++
			if service.SubmitImage(context.Background(), h.ID, raw) == nil || b.submitted != 0 {
				t.Fatal("bad budget forwarded")
			}
			h.Amount--
			good := sub.SpoolReceiptHash
			sub.SpoolReceiptHash = "wrong"
			bad, _ := chain.Sign("domain", key, 2, chain.Action{Type: "submit_image", HoldID: h.ID, ImageSubmission: &sub})
			if service.SubmitImage(context.Background(), h.ID, bad) == nil || b.submitted != 0 {
				t.Fatal("wrong receipt forwarded")
			}
			sub.SpoolReceiptHash = good
			if e = service.SubmitImage(context.Background(), h.ID, raw); e != nil {
				t.Fatal(e)
			}
			if len(b.actions) != 1 || b.actions[0] != "image_received" {
				t.Fatal("assignment before SubmitHeight+1", b.actions)
			}
			b.height = 4
			if e = service.FinalizedBlock(context.Background()); e != nil {
				t.Fatal(e)
			}
			if fate == "accept" || fate == "reject" {
				if h.State != "dispute" {
					t.Fatal("premature settlement")
				}
				h.AuditVerdict = fate
				b.height = 9
				if e = service.FinalizedBlock(context.Background()); e != nil {
					t.Fatal(e)
				}
			}
			if h.State != "settled" || b.actions[len(b.actions)-1] != "settle" {
				t.Fatal("fate did not settle immediately", b.actions)
			}
		})
	}
}
func TestImageStepLatencySeparateFromDelivery(t *testing.T) {
	s := &SpeedStats{}
	now := time.Now()
	for i := 0; i < 10; i++ {
		s.RecordImageStep("fast", "c", time.Second, now)
		s.RecordImageStep("slow", "c", 4*time.Second, now)
		s.RecordImageDelivery("fast", "c", time.Hour, now)
	}
	fast, _, _, _ := s.ImageScore("fast", "c", now)
	slow, _, _, _ := s.ImageScore("slow", "c", now)
	if fast <= slow {
		t.Fatal(fast, slow)
	}
	if len(s.Snapshot(now)) != 0 {
		t.Fatal("image timing leaked into text")
	}
	if len(s.ImageTimings(now)) != 30 {
		t.Fatal("lost delivery measurements")
	}
}
func TestImageReceiptT2Python(t *testing.T) {
	python := os.Getenv("EMBER_AUDIT_TEST_PYTHON")
	if python == "" {
		t.Fatal("EMBER_AUDIT_TEST_PYTHON required")
	}
	s, key := imageStore(t)
	h := imageHold()
	sub := imageSub([]byte("t2-image"))
	d, e := s.Put(h, sub, bytes.NewReader([]byte("t2-image")), 2)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(d.Receipt)
	file := filepath.Join(t.TempDir(), "receipt.json")
	if e = os.WriteFile(file, raw, 0600); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(python, "-c", `import sys,hashlib; from image_client import verify_spool_receipt; raw=open(sys.argv[1],'rb').read(); assert verify_spool_receipt(raw,bytes.fromhex(sys.argv[2]),'hold',1,sys.argv[3],2)==raw; assert hashlib.sha256(raw).hexdigest()==sys.argv[4]`, file, hex.EncodeToString(key.Public().(ed25519.PublicKey)), hex.EncodeToString(chain.ImageDeliveryBinding(&sub)), chain.SpoolReceiptHash(d.Receipt))
	root, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	cmd.Env = append(os.Environ(), "PYTHONPATH="+root+"/node:"+root+"/core-py:"+root+"/proto")
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("T2 rejected gateway receipt: %v %s", e, out)
	}
}

func TestImageHTTPSpoolReadAndAuthorization(t *testing.T) {
	s, key := imageStore(t)
	h := imageHold()
	b := &imageBook{h: h, m: imageModel(), height: 2}
	book, e := ledger.Open(filepath.Join(t.TempDir(), "auth.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer book.Close()
	for _, who := range []string{h.Node, h.Payer, "other"} {
		if e = book.IssueToken(who, 100, strings.Repeat("x", 32)+who); e != nil {
			t.Fatal(e)
		}
	}
	g := &Gateway{Ledger: book, Image: &ImageService{Backend: b, Store: s, ChainID: "domain"}}
	data := bytes.Repeat([]byte("768-image"), 125000)
	sub := imageSub(data)
	call := func(method, path, actor string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 32)+actor)
		r.Header.Set("X-Image-Binding", hex.EncodeToString(chain.ImageDeliveryBinding(&sub)))
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		return w
	}
	if w := call("PUT", "/v1/images/hold/spool", "other", data); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	w := call("PUT", "/v1/images/hold/spool", h.Node, data)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var receipt chain.SpoolAttestation
	if e = json.Unmarshal(w.Body.Bytes(), &receipt); e != nil {
		t.Fatal(e)
	}
	canonical, _ := json.Marshal(receipt)
	if !bytes.Equal(canonical, w.Body.Bytes()) || !ed25519.Verify(key.Public().(ed25519.PublicKey), chain.SpoolSigningBytes(receipt), receipt.Signature) {
		t.Fatal("noncanonical HTTP receipt")
	}
	if w = call("GET", "/v1/images/hold/content", h.Payer, nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) || w.Header().Get("X-Image-SHA256") != sub.ImageSHA256 {
		t.Fatal("fetch integrity", w.Code)
	}
	if w = call("GET", "/v1/images/hold/content", "other", nil); w.Code != 403 {
		t.Fatal("private image leaked")
	}
	if w = call("POST", "/v1/images/quote", h.Payer, []byte(`{"kind":"text","model":"ember-image"}`)); w.Code != 400 {
		t.Fatal("text in image path")
	}
	if w = call("POST", "/v1/chat/completions", h.Payer, []byte(`{"kind":"text-to-image","model":"ember-image","messages":[{"role":"user","content":"anything"}]}`)); w.Code != 400 {
		t.Fatal("t2i reached text dispatch")
	}
}
func TestImageT1T2FixtureCommitment(t *testing.T) {
	raw, e := os.ReadFile("../../node/testdata/image-t1.json")
	if e != nil {
		t.Fatal(e)
	}
	var fixture struct {
		WireLeaves []chain.ImageLeaf `json:"wire_leaves"`
		Root       string            `json:"root"`
		Hashes     []string          `json:"leaf_hashes"`
	}
	if e = json.Unmarshal(raw, &fixture); e != nil {
		t.Fatal(e)
	}
	hs := make([][32]byte, len(fixture.WireLeaves))
	for i, l := range fixture.WireLeaves {
		hs[i] = chain.ImageLeafHash(l)
		if hex.EncodeToString(hs[i][:]) != fixture.Hashes[i] {
			t.Fatal("T1/T2 leaf fixture mismatch")
		}
	}
	root, e := chain.SpotMerkleRoot(hs)
	if e != nil || hex.EncodeToString(root[:]) != fixture.Root {
		t.Fatal("T1/T2 root mismatch", e)
	}
}

func TestImageReceiptHeightAfterFsyncAndInterruptedArrival(t *testing.T) {
	s, _ := imageStore(t)
	h := imageHold()
	data := []byte("durable image")
	sub := imageSub(data)
	binding := chain.ImageDeliveryBinding(&sub)
	_, e := s.PutBindingAt(h, binding, bytes.NewReader(data), func() (int64, error) {
		if _, e := s.queue.Get("arrival-" + h.ID); e != nil {
			t.Fatal("height read before durable arrival", e)
		}
		return 0, errors.New("height query failed")
	})
	if e == nil {
		t.Fatal("signed without finalized height")
	}
	h.Image.Submission = &sub
	h.Image.Delivery = &chain.SpotChallenge{Deadline: 5}
	if _, e = s.Absence(h, 6); e == nil {
		t.Fatal("false absence after interrupted receipt")
	}
	d, e := s.PutBindingAt(h, binding, bytes.NewReader(data), func() (int64, error) { return 7, nil })
	if e != nil || d.Receipt.Height != 7 {
		t.Fatal("receipt failed recovery", e)
	}
}
func TestImageFateFailedSendRetriesOnBlock(t *testing.T) {
	s, _ := imageStore(t)
	h := imageHold()
	sub := imageSub([]byte("x"))
	if _, e := s.Put(h, sub, bytes.NewReader([]byte("x")), 2); e != nil {
		t.Fatal(e)
	}
	h.State = "dispute"
	h.SubmitHeight = 3
	h.Image.Submission = &sub
	h.Image.Delivery = &chain.SpotChallenge{Received: true}
	b := &imageBook{h: h, m: imageModel(), height: 4, fate: "unaudited", fail: true}
	service := &ImageService{Backend: b, Store: s}
	if service.FinalizedBlock(context.Background()) == nil {
		t.Fatal("lost failed send")
	}
	b.fail = false
	if e := service.FinalizedBlock(context.Background()); e != nil || h.State != "settled" {
		t.Fatal("failed event replay", e)
	}
}

func TestImageHTTPGenerationT2EnvelopeShape(t *testing.T) {
	s, key := imageStore(t)
	h := imageHold()
	h.Node = core.NodeID(key.Public().(ed25519.PublicKey))
	b := &imageBook{h: h, m: imageModel(), height: 2, fate: "unaudited"}
	book, e := ledger.Open(filepath.Join(t.TempDir(), "auth.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer book.Close()
	for _, who := range []string{h.Node, h.Payer} {
		if e = book.IssueToken(who, 100, strings.Repeat("x", 32)+who); e != nil {
			t.Fatal(e)
		}
	}
	manifest := []byte("manifest")
	g := &Gateway{Ledger: book, Manifest: manifest, SpeedStats: &SpeedStats{}, Candidates: func() []scheduler.Candidate {
		return []scheduler.Candidate{{Online: true, Stake: 10000, Profile: &pb.ResourceProfile{NodeId: h.Node, ChainId: b.m.ID, ManifestRoot: manifest, ExecutionProfileVer: b.m.ImageProfile.ExecutionProfile}}}
	}}
	service := &ImageService{Backend: b, Store: s, ChainID: "domain"}
	g.Image = service
	service.Run = func(ctx context.Context, node, hold string, job []byte) ([]byte, error) {
		data := []byte("local image payload")
		sub := imageSub(data)
		r := httptest.NewRequest("PUT", "/v1/images/"+hold+"/spool", bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 32)+node)
		r.Header.Set("X-Image-Binding", hex.EncodeToString(chain.ImageDeliveryBinding(&sub)))
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != 200 {
			return nil, errors.New(w.Body.String())
		}
		digest := sha256.Sum256(w.Body.Bytes())
		sub.SpoolReceiptHash = hex.EncodeToString(digest[:])
		raw, e := chain.Sign("domain", key, 1, chain.Action{Type: "submit_image", HoldID: hold, ImageSubmission: &sub})
		if e != nil {
			return nil, e
		}
		return json.Marshal(map[string]any{"kind": "image", "envelope": base64.StdEncoding.EncodeToString(raw)})
	}
	body := `{"kind":"text-to-image","model":"ember-image","request":{"prompt":"arbitrary raw prompt","steps":20}}`
	r := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 32)+h.Payer)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var response map[string]any
	if e = json.Unmarshal(w.Body.Bytes(), &response); e != nil {
		t.Fatal(e)
	}
	if response["hold_id"] != h.ID || response["url"] != "/v1/images/"+h.ID+"/content" || response["spool_receipt_hash"] == "" {
		t.Fatal(response)
	}
	if len(g.SpeedStats.ImageTimings(time.Now())) != 2 {
		t.Fatal("step and delivery timing not separately recorded")
	}
}

func TestImageTunnelResultTerminatesRun(t *testing.T) {
	relay := tunnel.New()
	relay.Legacy = true
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	server := grpc.NewServer()
	pb.RegisterNodeTunnelServer(server, relay)
	go server.Serve(listener)
	defer server.Stop()
	connection, e := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if e != nil {
		t.Fatal(e)
	}
	defer connection.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, e := pb.NewNodeTunnelClient(connection).Connect(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = stream.Send(&pb.TunnelFrame{NodeId: "image-node", Kind: "hello"}); e != nil {
		t.Fatal(e)
	}
	for !relay.Online("image-node") {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	go func() {
		f, e := stream.Recv()
		if e == nil {
			_ = stream.Send(&pb.TunnelFrame{NodeId: "image-node", RequestId: f.RequestId, AttemptId: f.AttemptId, Kind: "image_result", Payload: []byte("result")})
		}
	}()
	g := &Gateway{Tunnel: relay}
	raw, e := g.runImageTunnel(ctx, "image-node", "hold", []byte(`{"kind":"text-to-image"}`))
	if e != nil || string(raw) != "result" {
		t.Fatal("image result did not terminate run", e, string(raw))
	}
}
