package ledger

import (
	"bytes"
	"crypto/sha256"
	core "ember/core-go"
	chain "ember/services/chain"
	"encoding/binary"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

func TestImageMaterialsAuthenticateWholeFile(t *testing.T) {
	// A correct prefix with a changed payload must fail before profile derivation.
	header := []byte(`{"w.weight":{"dtype":"F32","shape":[1],"data_offsets":[0,4]}}`)
	file := append(binary.LittleEndian.AppendUint64(nil, uint64(len(header))), header...)
	file = append(file, 0, 0, 0, 0)
	hash := sha256.Sum256(file)
	p := &core.ImageProfile{Components: []core.ImageComponent{{Role: "unet", Files: map[string]string{"weights.safetensors": hex.EncodeToString(hash[:])}}}}
	file[len(file)-1] = 1
	if _, e := VerifyImageMaterials(p, map[string]io.Reader{"weights.safetensors": bytes.NewReader(file)}); e == nil || !strings.Contains(e.Error(), "SHA256 mismatch") {
		t.Fatal("unverified payload accepted or wrong validation failure", e)
	}
	if RecomputeImageCalibration(p, nil) == nil {
		t.Fatal("missing replay accepted")
	}
}
func TestPrepareImageRequestRejectsTruncationBeforeFreeze(t *testing.T) {
	p := &core.ImageProfile{MaxSteps: 100, PriceTable: []core.ImagePrice{{Step: 560000000000}}}
	a := chain.Action{Type: "image_request", ImageRequest: &chain.ImageRequest{Prompt: "positive", Steps: 20}}
	calls := []string{}
	tokenize := func(s string) ([]uint32, error) {
		calls = append(calls, s)
		if s == "positive" {
			return make([]uint32, 75), nil
		}
		return nil, nil
	}
	prepared, e := PrepareImageRequest(p, a, 1000000, tokenize)
	if e != nil || len(calls) != 2 || calls[1] != "" || len(prepared.ImageRequest.PromptTokens) != 77 || prepared.Actual != 0 || prepared.Amount <= 0 {
		t.Fatal(prepared, e, calls)
	}
	a.ImageRequest.PromptTokens = make([]uint32, 77) // cannot hide 76 content tokens
	tokenize = func(string) ([]uint32, error) { return make([]uint32, 76), nil }
	if _, e := PrepareImageRequest(p, a, 1000000, tokenize); e == nil {
		t.Fatal("overlong prompt accepted")
	}
}
