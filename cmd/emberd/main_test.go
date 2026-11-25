package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "ember/proto"
)

func TestPolicyLayerBounds(t *testing.T) {
	manifest := &pb.ModelManifest{TotalLayers: 24}
	policy := &pb.NumericAuditPolicy{Layers: []string{"0", "11", "23"}}
	if err := validatePolicyLayerBounds(policy, manifest); err != nil {
		t.Fatal(err)
	}
	for _, layer := range []string{"24", "999999999999999999999"} {
		policy.Layers[2] = layer
		if err := validatePolicyLayerBounds(policy, manifest); err == nil || !strings.Contains(err.Error(), layer) || !strings.Contains(err.Error(), "24 total layers") {
			t.Fatalf("layer %s: %v", layer, err)
		}
	}
	if err := validatePolicyLayerBounds(policy, &pb.ModelManifest{}); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentAuditKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "audit.key")
	first, err := loadAuditKey(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadAuditKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("audit identity changed")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = loadAuditKey(path); err == nil {
		t.Fatal("insecure audit key accepted")
	}
}

func TestReplaceEnvRemovesSourceCheckoutPythonPath(t *testing.T) {
	got := replaceEnv([]string{"HOME=/tmp", "PYTHONPATH=core-py:proto:node", "PATH=/bin"}, "PYTHONPATH", "/release/core:/release/proto:/release/node")
	want := []string{"HOME=/tmp", "PATH=/bin", "PYTHONPATH=/release/core:/release/proto:/release/node"}
	if len(got) != len(want) {
		t.Fatalf("environment=%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("environment=%v", got)
		}
	}
}
