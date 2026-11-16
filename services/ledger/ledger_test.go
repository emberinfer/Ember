package ledger

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

func sample(t *testing.T, id, node string) *pb.ExecutionReceipt {
	t.Helper()
	raw, e := os.ReadFile("../../testdata/vectors/signed.json")
	if e != nil {
		t.Fatal(e)
	}
	var vectors []struct{ Type, Wire string }
	if e = json.Unmarshal(raw, &vectors); e != nil {
		t.Fatal(e)
	}
	receipt := &pb.ExecutionReceipt{RequestId: id, NodeId: node, OutputTokens: 20}
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	key := ed25519.NewKeyFromSeed(seed)
	var prev []byte
	for _, v := range vectors {
		if v.Type != "ChunkRoot" {
			continue
		}
		data, _ := hex.DecodeString(v.Wire)
		c := &pb.ChunkRoot{}
		_ = proto.Unmarshal(data, c)
		c.RequestId = id
		c.PrevRoot = prev
		if e := core.Sign(c, key); e != nil {
			t.Fatal(e)
		}
		prev, e = core.Digest(c)
		if e != nil {
			t.Fatal(e)
		}
		receipt.ChunkRoots = append(receipt.ChunkRoots, c)
	}
	if e := core.Sign(receipt, key); e != nil {
		t.Fatal(e)
	}
	return receipt
}
func TestLedgerLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	l, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range []struct {
		id     string
		amount int64
	}{{"payer", 1000}, {"node", 5000}} {
		if e = l.Create(v.id, v.amount); e != nil {
			t.Fatal(e)
		}
	}
	if e = l.Stake("node", 5000); e != nil {
		t.Fatal(e)
	}
	if e = l.Freeze("r1", "payer", "node", 200); e != nil {
		t.Fatal(e)
	}
	if e = l.Freeze("r1", "payer", "node", 200); e == nil {
		t.Fatal("duplicate accepted")
	}
	if e = l.Submit("r1", sample(t, "r1", "node"), 100, time.Hour); e != nil {
		t.Fatal(e)
	}
	seed := make([]byte, 32)
	seed[0] = 9
	key := ed25519.NewKeyFromSeed(seed)
	ref, e := core.Digest(sample(t, "r1", "node"))
	if e != nil {
		t.Fatal(e)
	}
	challenge := &pb.AuditChallenge{ReceiptRef: ref, Positions: []uint32{0, 10, 19}}
	if e = core.Sign(challenge, key); e != nil {
		t.Fatal(e)
	}
	challengeRef, e := core.Digest(challenge)
	if e != nil {
		t.Fatal(e)
	}
	verdict := &pb.AuditVerdict{ChallengeRef: challengeRef, AuditorId: "local-reference", Verdict: "accept", Distances: []float64{0}}
	if e = core.Sign(verdict, key); e != nil {
		t.Fatal(e)
	}
	if e = l.RecordAudit("r1", challenge, verdict); e != nil {
		t.Fatal(e)
	}
	if e = l.RecordAudit("r1", challenge, verdict); e == nil {
		t.Fatal("duplicate audit accepted")
	}
	if _, e = l.DB().Exec(`UPDATE audits SET verdict=X'00' WHERE hold_id='r1'`); e == nil {
		t.Fatal("audit is mutable")
	}
	if e = l.Finish("r1", "accept", time.Now(), 20); e == nil {
		t.Fatal("early settlement")
	}
	if e = l.Finish("r1", "accept", time.Now().Add(2*time.Hour), 20); e != nil {
		t.Fatal(e)
	}
	b, s, e := l.Balance("payer")
	if e != nil || b != 900 || s != 0 {
		t.Fatalf("payer %d %d %v", b, s, e)
	}
	b, s, e = l.Balance("node")
	if e != nil || b != 100 || s != 5000 {
		t.Fatalf("node %d %d %v", b, s, e)
	}
	if e = l.Freeze("r2", "payer", "node", 200); e != nil {
		t.Fatal(e)
	}
	if e = l.Submit("r2", sample(t, "r2", "node"), 100, 0); e != nil {
		t.Fatal(e)
	}
	if e = l.Finish("r2", "reject", time.Now(), 20); e != nil {
		t.Fatal(e)
	}
	b, s, e = l.Balance("node")
	if e != nil || b != 100 || s != 3000 {
		t.Fatalf("slash %d %d %v", b, s, e)
	}
	if e = l.VerifyLog(); e != nil {
		t.Fatal(e)
	}
	if _, e = l.DB().Exec(`DELETE FROM receipts`); e == nil {
		t.Fatal("log is mutable")
	}
	if e = l.Close(); e != nil {
		t.Fatal(e)
	}
	l, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	if e = l.VerifyLog(); e != nil {
		t.Fatal(e)
	}
}

func TestBearerToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.db")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.IssueToken("visitor", 42, "secret-token-with-at-least-32-characters"); err != nil {
		t.Fatal(err)
	}
	if _, err = l.TokenAccount("visitor"); err == nil {
		t.Fatal("account name authenticated")
	}
	if _, err = l.TokenAccount("wrong-token-with-at-least-32-characters"); err == nil {
		t.Fatal("wrong token authenticated")
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	l, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if id, err := l.TokenAccount("secret-token-with-at-least-32-characters"); err != nil || id != "visitor" {
		t.Fatal(id, err)
	}
	var count int
	if err = l.DB().QueryRow("SELECT count(*) FROM tokens WHERE digest=?", []byte("secret-token-with-at-least-32-characters")).Scan(&count); err != nil || count != 0 {
		t.Fatal("raw token stored", err)
	}
}

func TestRecoverFrozenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recover.db")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err = l.Create("payer", 100); err != nil {
		t.Fatal(err)
	}
	if err = l.Create("node", 100); err != nil {
		t.Fatal(err)
	}
	if err = l.Stake("node", 50); err != nil {
		t.Fatal(err)
	}
	if err = l.Freeze("pending", "payer", "node", 10); err != nil {
		t.Fatal(err)
	}
	if got, e := l.RecoverFrozen(); e != nil || got != 1 {
		t.Fatalf("recover=%d %v", got, e)
	}
	if got, e := l.RecoverFrozen(); e != nil || got != 0 {
		t.Fatalf("second recover=%d %v", got, e)
	}
	if balance, _, e := l.Balance("payer"); e != nil || balance != 100 {
		t.Fatalf("balance=%d %v", balance, e)
	}
	if state, e := l.State("pending"); e != nil || state != "cancelled" {
		t.Fatalf("state=%q %v", state, e)
	}
}
