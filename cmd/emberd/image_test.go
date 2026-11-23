package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"ember/services/gateway"
	"ember/services/ledger"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestImageHostDisabled(t *testing.T) {
	root := t.TempDir()
	api := &gateway.Gateway{}
	if err := wireImages(api, nil, false, filepath.Join(root, "ledger"), "", ""); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mountImages(mux, api)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("POST", "/v1/images/generations", nil))
	if response.Code != 404 || api.Image != nil {
		t.Fatalf("disabled: %d, %v", response.Code, api.Image)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("disabled created state: %v %v", entries, err)
	}
}

func TestImageHostEnabled(t *testing.T) {
	root := t.TempDir()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(root, "authority")
	if err = os.WriteFile(keyFile, key, 0600); err != nil {
		t.Fatal(err)
	}
	authFile := filepath.Join(root, "auth.json")
	raw, _ := json.Marshal([]map[string]string{{"token": "01234567890123456789012345678901", "key": keyFile}})
	if err = os.WriteFile(authFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	book, err := ledger.OpenChain(ledger.ChainConfig{RPC: "http://127.0.0.1:1", ChainID: "host-test", CoordinatorKey: keyFile, AuditorKey: keyFile, AuthFile: authFile})
	if err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(root, "ledger")
	api := &gateway.Gateway{Ledger: book}
	if err = wireImages(api, book, true, database, "", ""); err != nil {
		t.Fatal(err)
	}
	if api.Image == nil || api.Image.Store == nil || api.Image.Backend == nil || api.Image.ChainID != "host-test" {
		t.Fatal("incomplete image host")
	}
	first, err := os.ReadFile(database + ".image.key")
	if err != nil || len(first) != 64 {
		t.Fatal("identity not persisted", err)
	}
	info, err := os.Stat(database + ".image.key")
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("identity permissions", err)
	}
	if info, err = os.Stat(database + ".image-spool"); err != nil || !info.IsDir() {
		t.Fatal("spool not initialized", err)
	}
	mux := http.NewServeMux()
	mountImages(mux, api)
	for _, path := range []string{"generations", "quote", "hold/spool", "hold/content", "hold/receipt", "hold/status", "hold/absence"} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest("GET", "/v1/images/"+path, nil))
		if response.Code != 401 {
			t.Fatalf("route %s: %d", path, response.Code)
		}
	}
	if err = wireImages(api, book, true, database, "", ""); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(database + ".image.key")
	if string(first) != string(second) {
		t.Fatal("identity changed on restart")
	}
}

func TestImageHostRequiresChain(t *testing.T) {
	if wireImages(&gateway.Gateway{}, nil, true, filepath.Join(t.TempDir(), "db"), "", "") == nil {
		t.Fatal("image enabled without chain")
	}
}
