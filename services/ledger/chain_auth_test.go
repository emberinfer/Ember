package ledger

import (
	"crypto/ed25519"
	"crypto/rand"
	core "ember/core-go"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChainAuthBindings(t *testing.T) {
	dir := t.TempDir()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	kp := filepath.Join(dir, "account.key")
	if e := os.WriteFile(kp, key, 0600); e != nil {
		t.Fatal(e)
	}
	id := core.NodeID(pub)
	token := strings.Repeat("k", 32)
	addressToken := strings.Repeat("a", 32)
	config := ChainConfig{RPC: "http://127.0.0.1:1", ChainID: "auth-test", CoordinatorKey: kp, AuditorKey: kp, AuthFile: filepath.Join(dir, "auth.json")}
	open := func(entries []chainAuthEntry) (*ChainLedger, error) {
		raw, _ := json.Marshal(entries)
		if e := os.WriteFile(config.AuthFile, raw, 0600); e != nil {
			t.Fatal(e)
		}
		return OpenChain(config)
	}
	l, e := open([]chainAuthEntry{{Token: token, Key: kp}, {Token: addressToken, Address: strings.ToUpper(id)}})
	if e != nil {
		t.Fatal(e)
	}
	for _, tok := range []string{token, addressToken} {
		got, e := l.TokenAccount(tok)
		if e != nil || got != id {
			t.Fatalf("binding mismatch %v", e)
		}
	}
	if !l.TokenCanSign(token) || l.TokenCanSign(addressToken) {
		t.Fatal("credential capability leaked across same-address tokens")
	}
	l, e = open([]chainAuthEntry{{Token: addressToken, Address: id}})
	if e != nil {
		t.Fatal(e)
	}
	if len(l.accounts) != 0 {
		t.Fatal("address binding acquired a key")
	}
	if e = l.Freeze("hold", id, id, 1); e == nil {
		t.Fatal("address binding signed freeze")
	}
	if e = l.Stake(id, 1); e == nil {
		t.Fatal("address binding signed stake")
	}
	for name, entries := range map[string][]chainAuthEntry{
		"neither": {{Token: token}}, "both": {{Token: token, Key: kp, Address: id}},
		"short": {{Token: token, Address: id[:62]}}, "nonhex": {{Token: token, Address: strings.Repeat("z", 64)}},
		"duplicate":   {{Token: token, Key: kp}, {Token: token, Address: id}},
		"short-token": {{Token: "short", Address: id}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, e := open(entries); e == nil {
				t.Fatal("invalid auth accepted")
			}
		})
	}
}
