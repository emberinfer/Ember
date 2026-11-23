package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	core "ember/core-go"
	chain "ember/services/chain"
	"ember/services/ledger"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageAddressAuthorization(t *testing.T) {
	dir := t.TempDir()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	kp := filepath.Join(dir, "payer.key")
	if e := os.WriteFile(kp, key, 0600); e != nil {
		t.Fatal(e)
	}
	payer := core.NodeID(pub)
	node := strings.Repeat("1", 64)
	other := strings.Repeat("2", 64)
	tokens := map[string]string{}
	entries := []map[string]string{}
	for _, address := range []string{node, payer, other} {
		token := strings.Repeat("t", 32) + address
		tokens[address] = token
		entries = append(entries, map[string]string{"token": token, "address": address})
	}
	// A custody credential for the same payer must not elevate its address token.
	entries = append(entries, map[string]string{"token": strings.Repeat("k", 32), "key": kp})
	raw, _ := json.Marshal(entries)
	ap := filepath.Join(dir, "auth.json")
	if e := os.WriteFile(ap, raw, 0600); e != nil {
		t.Fatal(e)
	}
	book, e := ledger.OpenChain(ledger.ChainConfig{RPC: "http://127.0.0.1:1", ChainID: "domain", CoordinatorKey: kp, AuditorKey: kp, AuthFile: ap})
	if e != nil {
		t.Fatal(e)
	}
	s, _ := imageStore(t)
	h := imageHold()
	h.Node = node
	h.Payer = payer
	b := &imageBook{h: h, m: imageModel(), height: 2}
	g := &Gateway{Ledger: book, Image: &ImageService{Backend: b, Store: s, ChainID: "domain"}}
	data := []byte("image address authorization")
	sub := imageSub(data)
	call := func(method, path, actor string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+tokens[actor])
		r.Header.Set("X-Image-Binding", hex.EncodeToString(chain.ImageDeliveryBinding(&sub)))
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		return w
	}
	for _, actor := range []string{payer, other} {
		if w := call("PUT", "/v1/images/hold/spool", actor); w.Code != 403 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := call("GET", "/v1/images/hold/absence", node); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("PUT", "/v1/images/hold/spool", node); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, path := range []string{"status", "receipt", "content", "absence"} {
		if w := call("GET", "/v1/images/hold/"+path, other); w.Code != 403 {
			t.Fatal(path, w.Code)
		}
		if path == "absence" {
			continue
		} // A received delivery correctly refuses absence.
		for _, actor := range []string{node, payer} {
			if w := call("GET", "/v1/images/hold/"+path, actor); w.Code != 200 {
				t.Fatal(path, w.Code, w.Body.String())
			}
		}
	}
	for _, path := range []string{"/v1/images/generations", "/v1/chat/completions"} {
		if w := call("POST", path, payer); w.Code != 403 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	// No executor key exists in the real backend's custody map; it must fail
	// before an RPC request even if invoked outside the HTTP capability gate.
	rpc, domain, coord, accounts, mu := book.ImageTransport()
	transport, e := NewImageRPCBackendOn(rpc, domain, coord, accounts, mu)
	if e != nil {
		t.Fatal(e)
	}
	if e = transport.Freeze(context.Background(), chain.Action{Type: "image_request", Payer: node}); e == nil || !strings.Contains(e.Error(), "signer unavailable") {
		t.Fatalf("missing key accepted: %v", e)
	}
}
