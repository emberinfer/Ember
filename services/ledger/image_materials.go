package ledger

import (
	"crypto/sha256"
	core "ember/core-go"
	chain "ember/services/chain"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// VerifyImageMaterials is the publication ingestion boundary. It streams the
// entire pinned artifacts through SHA256, retaining only safetensors headers;
// hashing a header alone cannot authenticate its membership in a weight file.
// The returned headers are suitable for Action.ImageHeaders after this check.
// Consensus independently repeats parameter/hierarchy/FLOP derivation from the
// submitted inventory; it does not perform network or filesystem I/O.
func VerifyImageMaterials(p *core.ImageProfile, files map[string]io.Reader) (map[string][]byte, error) {
	if p == nil {
		return nil, errors.New("missing image profile")
	}
	headers := map[string][]byte{}
	shapes := map[string][]core.TensorShape{}
	seen := map[string]bool{}
	for _, component := range p.Components {
		for name, want := range component.Files {
			if seen[name] || files[name] == nil {
				return nil, fmt.Errorf("missing/duplicate image artifact %s", name)
			}
			seen[name] = true
			hash := sha256.New()
			reader := io.TeeReader(files[name], hash)
			var header []byte
			var payloadBytes uint64
			if component.Role == "unet" || component.Role == "vae" || component.Role == "text_encoder" {
				prefix := make([]byte, 8)
				if _, e := io.ReadFull(reader, prefix); e != nil {
					return nil, e
				}
				n := binary.LittleEndian.Uint64(prefix)
				if n == 0 || n > 16<<20-8 {
					return nil, errors.New("image artifact header exceeds limit")
				}
				header = make([]byte, 8+int(n))
				copy(header, prefix)
				if _, e := io.ReadFull(reader, header[8:]); e != nil {
					return nil, e
				}
				tensors, e := core.ParseSafeTensorHeader(header)
				if e != nil {
					return nil, e
				}
				for _, tensor := range tensors {
					n := uint64(4)
					if tensor.Dtype != "F32" {
						n = 2
					}
					for _, d := range tensor.Shape {
						n *= d
					}
					payloadBytes += n
				}
				shapes[component.Role] = append(shapes[component.Role], tensors...)
			}
			count, e := io.Copy(io.Discard, reader)
			if e != nil {
				return nil, e
			}
			if header != nil && uint64(count) != payloadBytes {
				return nil, errors.New("safetensors payload size mismatch")
			}
			if hex.EncodeToString(hash.Sum(nil)) != want {
				return nil, fmt.Errorf("image artifact SHA256 mismatch: %s", name)
			}
			if header != nil {
				headers[name] = header
			}
		}
	}
	if e := core.ValidateImageProfile(p, shapes); e != nil {
		return nil, e
	}
	return headers, nil
}

// RecomputeImageCalibration is the T4 hook. The measurement callback is a
// Ledger-local paired-device replay, never an oracle in consensus execution.
// The caller must authenticate calibration source/device signatures before use.
func RecomputeImageCalibration(p *core.ImageProfile, replay func() ([]core.ImageCalibrationObservation, error)) error {
	if replay == nil {
		return errors.New("missing image calibration replay")
	}
	observations, e := replay()
	if e != nil {
		return e
	}
	return core.ValidateImageCalibration(p, observations)
}

// PrepareImageRequest runs the pinned tokenizer before signing/freezing. Both
// strings, including empty negative, always pass through the same T0 boundary;
// callers cannot supply a truncated token sequence to this ingestion API.
func PrepareImageRequest(p *core.ImageProfile, a chain.Action, ppm int64, tokenize core.T2ITokenizer) (chain.Action, error) {
	if a.Type != "image_request" || a.ImageRequest == nil || a.Actual != 0 {
		return chain.Action{}, errors.New("invalid image request action")
	}
	r := *a.ImageRequest
	tokens, e := core.T2IEncodePrompts(r.Prompt, r.NegativePrompt, tokenize)
	if e != nil {
		return chain.Action{}, e
	}
	r.PromptTokens = tokens[0]
	r.NegativeTokens = tokens[1]
	amount, e := core.ImageActual(p, r.Resolution, r.Steps, ppm)
	if e != nil {
		return chain.Action{}, e
	}
	a.ImageRequest = &r
	a.Amount = amount
	return a, nil
}
