package ledger

import (
	"bytes"
	"crypto/sha256"
	core "ember/core-go"
	chain "ember/services/chain"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
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

// sd15-clip.header is the unmodified length prefix + JSON header from the
// T2-pinned d008943c... model.safetensors (header SHA256 38103b1a5c6b6d274a7e3c325a70c21a60a649780915a71c8b20110f59fc2383).
// Payload is streamed as zeros here; real artifact authentication is separately
// exercised by the T1F three-file replay.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestRealCLIPHeaderMaterialsAndBufferPolicy(t *testing.T) {
	clip, err := os.ReadFile("testdata/sd15-clip.header")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(clip)); got != "38103b1a5c6b6d274a7e3c325a70c21a60a649780915a71c8b20110f59fc2383" {
		t.Fatal("real header fixture changed", got)
	}
	for _, tc := range []struct {
		name, old, replacement string
		want                   string
	}{
		{name: "real"},
		{"unlisted", "position_ids", "position_ids2", "unapproved image buffer"},
		{"shape", `"shape":[1,77]`, `"shape":[7,11]`, "unapproved image buffer"},
		{"dtype", `"dtype":"I64"`, `"dtype":"U64"`, "unapproved image buffer"},
		{"unknown", `"dtype":"I64"`, `"dtype":"XYZ"`, "unknown safetensors dtype"},
		{"integer-weight", `"dtype":"F32"`, `"dtype":"I32"`, "unapproved image buffer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch := append([]byte(nil), clip...)
			if tc.old != "" {
				body := strings.ReplaceAll(string(ch[8:]), tc.old, tc.replacement)
				if body == string(ch[8:]) {
					t.Fatal("mutation did not apply")
				}
				ch = append(binary.LittleEndian.AppendUint64(nil, uint64(len(body))), []byte(body)...)
			}
			p := &core.ImageProfile{ExecutionProfile: "diffusers-sd1.5-fp32-v1", RNGSpecID: "philox-gauss-v1", MaxSteps: 100, GuidanceRange: [2]uint32{1000, 30000}, AllowedResolutions: []core.ImageResolution{{Width: 512, Height: 512}}, AllowedSamplers: []string{"ddim"}, RangeBand: core.ImageStepBand{Metric: "relative-l2-v1", Base: 3e-6, StepCoef: 1e-6, HardMultiplier: 100, PrecisionMin: .005, PrecisionMax: .06, CalibrationHash: strings.Repeat("1", 64)}, ArchManifest: map[string][]core.ImageLayer{}}
			p.LatentSpec.Channels = 4
			p.LatentSpec.VAEScaleFactor = 8
			p.LatentSpec.Distribution = "standard-normal"
			readers := map[string]io.Reader{}
			headers := map[string][]byte{"text_encoder": ch}
			for role, name := range map[string]string{"unet": "conv.weight", "vae": "decoder.conv_out.weight"} {
				body := []byte(fmt.Sprintf(`{"%s":{"dtype":"F32","shape":[1,1,1,1],"data_offsets":[0,4]}}`, name))
				headers[role] = append(binary.LittleEndian.AppendUint64(nil, uint64(len(body))), body...)
			}
			for _, role := range []string{"unet", "text_encoder", "vae"} {
				h := headers[role]
				ts, e := core.ParseSafeTensorHeader(h)
				if e != nil {
					if tc.want != "" && strings.Contains(e.Error(), tc.want) {
						return
					}
					t.Fatal(e)
				}
				var payload, params uint64
				for _, tensor := range ts {
					size, _ := core.SafeTensorDtypeSize(tensor.Dtype)
					n := uint64(1)
					for _, d := range tensor.Shape {
						n *= d
					}
					payload += n * size
					if tensor.Dtype == "F32" {
						params += n
					}
				}
				if role == "text_encoder" && tc.name == "real" {
					if params != 123060480 || payload != 492242536 {
						t.Fatalf("float T=%d payload=%d", params, payload)
					}
				}
				layers, e := core.ReconstructImageLayers(role, ts)
				// Invalid buffer errors must also propagate through Ledger, rather than
				// accepting a publisher-supplied manifest that omits the offending tensor.
				p.ArchManifest[role] = layers
				digest := sha256.New()
				newReader := func() io.Reader {
					return io.MultiReader(bytes.NewReader(h), io.LimitReader(zeroReader{}, int64(payload)))
				}
				if _, e = io.Copy(digest, newReader()); e != nil {
					t.Fatal(e)
				}
				p.Components = append(p.Components, core.ImageComponent{Role: role, Files: map[string]string{role: hex.EncodeToString(digest.Sum(nil))}, Params: params, Dtype: "F32"})
				readers[role] = newReader()
			}
			for _, role := range []string{"tokenizer", "scheduler_cfg", "arch_manifest"} {
				sum := sha256.Sum256(nil)
				p.Components = append(p.Components, core.ImageComponent{Role: role, Files: map[string]string{role: hex.EncodeToString(sum[:])}})
				readers[role] = bytes.NewReader(nil)
			}
			p.PriceTable = []core.ImagePrice{{Step: 4 * 64 * 64, Text: 2 * 123060480 * 77 * 2, VAE: 2 * 512 * 512}}
			_, e := VerifyImageMaterials(p, readers)
			if tc.want != "" {
				if e == nil || !strings.Contains(e.Error(), tc.want) {
					t.Fatalf("want %s got %v", tc.want, e)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestI64PayloadUsesEightBytes(t *testing.T) {
	body := []byte(`{"text_model.embeddings.position_ids":{"dtype":"I64","shape":[1,77],"data_offsets":[0,616]}}`)
	header := append(binary.LittleEndian.AppendUint64(nil, uint64(len(body))), body...)
	for _, size := range []int{77 * 8, 77 * 2} {
		file := append(append([]byte(nil), header...), make([]byte, size)...)
		hash := sha256.Sum256(file)
		p := &core.ImageProfile{Components: []core.ImageComponent{{Role: "text_encoder", Files: map[string]string{"clip": hex.EncodeToString(hash[:])}}}}
		_, e := VerifyImageMaterials(p, map[string]io.Reader{"clip": bytes.NewReader(file)})
		if size == 77*8 {
			if e == nil || e.Error() != "invalid image profile" {
				t.Fatal(e)
			}
		} else if e == nil || e.Error() != "safetensors payload size mismatch" {
			t.Fatal(e)
		}
	}
}
