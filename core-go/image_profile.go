package core

// Consensus image accounting v1. Non-matmul operations (normalization,
// activation, interpolation and biases) uniformly cost zero. Weight parameters
// still include these tensors. Integer FLOPs are never estimated from bytes.
import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

type ImageResolution struct {
	Width  uint32 `json:"width"`
	Height uint32 `json:"height"`
}
type ImagePrice struct {
	Step uint64 `json:"f_step"`
	Text uint64 `json:"f_textenc"`
	VAE  uint64 `json:"f_vae"`
}
type ImageComponent struct {
	Role   string            `json:"role"`
	Files  map[string]string `json:"files"`
	Params uint64            `json:"params"`
	Dtype  string            `json:"dtype"`
}
type ImageLayer struct {
	Name      string   `json:"name"`
	Shape     []uint64 `json:"shape"`
	Op        string   `json:"op"`
	Scale     uint64   `json:"scale"`
	Positions string   `json:"positions"`
}
type ImageStepBand struct {
	Metric          string  `json:"metric"`
	Base            float64 `json:"base"`
	StepCoef        float64 `json:"step_coef"`
	HardMultiplier  float64 `json:"hard_multiplier"`
	PrecisionMin    float64 `json:"precision_min"`
	PrecisionMax    float64 `json:"precision_max"`
	CalibrationHash string  `json:"calibration_hash"`
}
type ImageProfile struct {
	Components []ImageComponent `json:"components"`
	LatentSpec struct {
		Channels       uint32 `json:"channels"`
		VAEScaleFactor uint32 `json:"vae_scale_factor"`
		Distribution   string `json:"distribution"`
	} `json:"latent_spec"`
	AllowedResolutions []ImageResolution       `json:"allowed_resolutions"`
	AllowedSamplers    []string                `json:"allowed_samplers"`
	MaxSteps           uint32                  `json:"max_steps"`
	GuidanceRange      [2]uint32               `json:"guidance_range"`
	RNGSpecID          string                  `json:"rng_spec_id"`
	ExecutionProfile   string                  `json:"execution_profile"`
	RangeBand          ImageStepBand           `json:"range_band"`
	ArchManifest       map[string][]ImageLayer `json:"arch_manifest"`
	PriceTable         []ImagePrice            `json:"price_table"`
}

func (p ImageStepBand) Validate() error {
	if raw, e := hex.DecodeString(p.CalibrationHash); e != nil || len(raw) != 32 {
		return errors.New("invalid calibration hash")
	}
	for _, x := range []float64{p.Base, p.StepCoef, p.HardMultiplier, p.PrecisionMin, p.PrecisionMax} {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return errors.New("nonfinite step band")
		}
	}
	if p.Metric != "relative-l2-v1" || p.Base <= 0 || p.StepCoef < 0 || p.HardMultiplier <= 1 || p.PrecisionMin < .005 || p.PrecisionMax > .06 || p.PrecisionMin >= p.PrecisionMax || len(p.CalibrationHash) != 64 {
		return errors.New("invalid step band")
	}
	return nil
}
func (p ImageStepBand) Thresholds(step uint32) (float64, float64, error) {
	if err := p.Validate(); err != nil {
		return 0, 0, err
	}
	soft := p.Base + p.StepCoef*float64(step)
	hard := soft * p.HardMultiplier
	if math.IsInf(hard, 0) {
		return 0, 0, errors.New("step band overflow")
	}
	return soft, hard, nil
}
func ImageSpotK(n uint64, qPPM uint32) uint64 {
	if n == 0 || qPPM == 0 {
		return 0
	}
	k := (4800*n + uint64(qPPM)/2) / uint64(qPPM)
	if k < 1 {
		k = 1
	}
	if k > n {
		k = n
	}
	return k
}

// ParseSafeTensorHeader accepts the 8-byte LE length and JSON header, without
// tensor payload. Offsets, dtype and all names must be unique and contiguous.
func ParseSafeTensorHeader(raw []byte) ([]TensorShape, error) {
	if len(raw) < 8 || binary.LittleEndian.Uint64(raw[:8]) != uint64(len(raw)-8) || len(raw) > 16<<20 {
		return nil, errors.New("invalid safetensors header size")
	}
	// Decoder's token stream rejects duplicate tensor/metadata keys.
	dec := json.NewDecoder(strings.NewReader(string(raw[8:])))
	if t, e := dec.Token(); e != nil || t != json.Delim('{') {
		return nil, errors.New("invalid header")
	}
	seen := map[string]bool{}
	type span struct{ start, end uint64 }
	var spans []span
	var out []TensorShape
	for dec.More() {
		tok, e := dec.Token()
		if e != nil {
			return nil, e
		}
		name, ok := tok.(string)
		if !ok || seen[name] {
			return nil, errors.New("duplicate tensor")
		}
		seen[name] = true
		var value json.RawMessage
		if e = dec.Decode(&value); e != nil {
			return nil, e
		}
		if name == "__metadata__" {
			continue
		}
		var t struct {
			Dtype   string   `json:"dtype"`
			Shape   []uint64 `json:"shape"`
			Offsets []uint64 `json:"data_offsets"`
		}
		if json.Unmarshal(value, &t) != nil || len(t.Offsets) != 2 || len(t.Shape) == 0 {
			return nil, errors.New("invalid tensor")
		}
		size := uint64(0)
		switch t.Dtype {
		case "F32":
			size = 4
		case "F16", "BF16":
			size = 2
		default:
			return nil, errors.New("unsupported weight dtype")
		}
		for _, d := range t.Shape {
			if d == 0 || d > math.MaxUint64/size {
				return nil, errors.New("tensor overflow")
			}
			size *= d
		}
		if t.Offsets[1] < t.Offsets[0] || t.Offsets[1]-t.Offsets[0] != size {
			return nil, errors.New("tensor offsets")
		}
		spans = append(spans, span{t.Offsets[0], t.Offsets[1]})
		out = append(out, TensorShape{Name: name, Shape: t.Shape, Dtype: t.Dtype})
	}
	if _, e := dec.Token(); e != nil {
		return nil, e
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	end := uint64(0)
	for _, s := range spans {
		if s.start != end {
			return nil, errors.New("tensor gaps/overlap")
		}
		end = s.end
	}
	if len(out) == 0 {
		return nil, errors.New("empty weights")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ReconstructImageLayers derives the supported diffusers SD1.x hierarchy from
// tensor names/shapes. Publishers cannot choose positions, scales or operations.
func ReconstructImageLayers(role string, tensors []TensorShape) ([]ImageLayer, error) {
	var out []ImageLayer
	for _, t := range tensors {
		l := ImageLayer{Name: t.Name, Shape: t.Shape, Op: "ignored", Scale: 1, Positions: "spatial"}
		if role == "unet" {
			l.Scale = 8
			parts := strings.Split(t.Name, ".")
			if len(parts) > 2 && (parts[0] == "down_blocks" || parts[0] == "up_blocks") {
				i, e := strconv.Atoi(parts[1])
				if e != nil || i < 0 || i > 3 {
					return nil, errors.New("unsupported UNet stage")
				}
				if parts[0] == "up_blocks" {
					i = 3 - i
				}
				l.Scale = 8 << i
				if strings.Contains(t.Name, "upsamplers") {
					l.Scale /= 2
				}
				if strings.Contains(t.Name, "downsamplers") {
					l.Scale *= 2
				}
			}
		} else if role == "vae" {
			l.Scale = 8
			if strings.HasPrefix(t.Name, "decoder.up_blocks.") {
				parts := strings.Split(t.Name, ".")
				i, e := strconv.Atoi(parts[2])
				if e != nil || i < 0 || i > 3 {
					return nil, errors.New("unsupported VAE stage")
				}
				l.Scale = 8 >> i
				if strings.Contains(t.Name, "upsamplers") {
					l.Scale /= 2
					if l.Scale == 0 {
						return nil, errors.New("invalid upsample")
					}
				}
			}
			if strings.HasPrefix(t.Name, "decoder.conv_out") || strings.HasPrefix(t.Name, "decoder.conv_norm_out") {
				l.Scale = 1
			}
		}
		if role == "unet" || (role == "vae" && (strings.HasPrefix(t.Name, "decoder.") || strings.HasPrefix(t.Name, "post_quant_conv."))) {
			if strings.HasSuffix(t.Name, ".weight") {
				if len(t.Shape) != 1 && len(t.Shape) != 2 && len(t.Shape) != 4 {
					return nil, errors.New("unsupported image weight rank")
				}
				switch len(t.Shape) {
				case 4:
					l.Op = "conv"
				case 2:
					l.Op = "linear"
					if strings.Contains(t.Name, "time_embedding") || strings.Contains(t.Name, "time_emb_proj") {
						l.Positions = "one"
					}
					if strings.Contains(t.Name, "attn2.to_k") || strings.Contains(t.Name, "attn2.to_v") {
						l.Positions = "text"
					}
				}
			}
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
func imageLayerFLOPs(layers []ImageLayer, r ImageResolution) (uint64, error) {
	sum := new(big.Int)
	for _, l := range layers {
		if l.Op == "ignored" {
			continue
		}
		if l.Scale == 0 || uint64(r.Width)%l.Scale != 0 || uint64(r.Height)%l.Scale != 0 {
			return 0, errors.New("nonintegral layer resolution")
		}
		pos := uint64(r.Width) / l.Scale * (uint64(r.Height) / l.Scale)
		if l.Positions == "one" {
			pos = 1
		}
		if l.Positions == "text" {
			pos = 77
		}
		v := new(big.Int).SetUint64(2)
		for _, d := range l.Shape {
			v.Mul(v, new(big.Int).SetUint64(d))
		}
		v.Mul(v, new(big.Int).SetUint64(pos))
		sum.Add(sum, v)
		// Projection terms supply 8 L d² for self attention; add the two
		// score/value products exactly once at to_q. Cross attention Lk=77.
		if (strings.Contains(l.Name, "attn") || strings.Contains(l.Name, "attentions")) && strings.HasSuffix(l.Name, "to_q.weight") {
			keys := uint64(r.Width) / l.Scale * (uint64(r.Height) / l.Scale)
			if strings.Contains(l.Name, "attn2") {
				keys = 77
			}
			v.SetUint64(4)
			for _, d := range []uint64{pos, keys, l.Shape[0]} {
				v.Mul(v, new(big.Int).SetUint64(d))
			}
			sum.Add(sum, v)
		}
	}
	if !sum.IsUint64() {
		return 0, errors.New("FLOPs overflow")
	}
	return sum.Uint64(), nil
}
func ValidateImageProfile(p *ImageProfile, shapes map[string][]TensorShape) error {
	if p == nil || p.RNGSpecID != "philox-gauss-v1" || p.ExecutionProfile != "diffusers-sd1.5-fp32-v1" || p.LatentSpec.Channels != 4 || p.LatentSpec.VAEScaleFactor != 8 || p.LatentSpec.Distribution != "standard-normal" || p.MaxSteps < 1 || p.MaxSteps > 100 || p.GuidanceRange[0] < 1000 || p.GuidanceRange[1] > 30000 || p.GuidanceRange[0] > p.GuidanceRange[1] || p.RangeBand.Validate() != nil {
		return errors.New("invalid image profile")
	}
	if len(p.AllowedResolutions) == 0 || len(p.AllowedResolutions) != len(p.PriceTable) || len(p.ArchManifest) != 3 {
		return errors.New("incomplete image table")
	}
	seenRes := map[ImageResolution]bool{}
	for _, r := range p.AllowedResolutions {
		if !((r.Width == 512 && r.Height == 512) || (r.Width == 512 && r.Height == 768) || (r.Width == 768 && r.Height == 512)) || seenRes[r] {
			return errors.New("unsupported resolution")
		}
		seenRes[r] = true
	}
	seenS := map[string]bool{}
	for _, v := range p.AllowedSamplers {
		if (v != "ddim" && v != "euler") || seenS[v] {
			return errors.New("unsupported sampler")
		}
		seenS[v] = true
	}
	if len(seenS) == 0 {
		return errors.New("empty samplers")
	}
	seen := map[string]bool{}
	params := map[string]uint64{}
	for _, c := range p.Components {
		if seen[c.Role] || len(c.Files) == 0 {
			return errors.New("duplicate/empty component")
		}
		seen[c.Role] = true
		switch c.Role {
		case "tokenizer", "scheduler_cfg", "arch_manifest":
			if c.Params != 0 {
				return errors.New("nonweight params")
			}
			continue
		case "unet", "text_encoder", "vae":
		default:
			return errors.New("unsupported component")
		}
		if c.Dtype != "F32" {
			return errors.New("uncertified dtype")
		}
		ts := shapes[c.Role]
		if len(ts) == 0 {
			return errors.New("missing component headers")
		}
		names := map[string]bool{}
		var total uint64
		for _, t := range ts {
			if names[t.Name] || t.Dtype != c.Dtype {
				return errors.New("duplicate/dtype tensor")
			}
			names[t.Name] = true
			n := uint64(1)
			for _, d := range t.Shape {
				if d == 0 || d > math.MaxUint64/n {
					return errors.New("parameter overflow")
				}
				n *= d
			}
			if n > math.MaxUint64-total {
				return errors.New("parameter overflow")
			}
			total += n
		}
		if total != c.Params {
			return errors.New("declared parameters mismatch")
		}
		params[c.Role] = total
		layers, e := ReconstructImageLayers(c.Role, ts)
		if e != nil {
			return e
		}
		if !reflect.DeepEqual(layers, p.ArchManifest[c.Role]) {
			return fmt.Errorf("arch_manifest shape/hierarchy mismatch: %s", c.Role)
		}
	}
	for _, role := range []string{"unet", "text_encoder", "vae", "tokenizer", "scheduler_cfg", "arch_manifest"} {
		if !seen[role] {
			return errors.New("missing component")
		}
	}
	enc := new(big.Int).Mul(new(big.Int).SetUint64(params["text_encoder"]), big.NewInt(2*77*2))
	if !enc.IsUint64() {
		return errors.New("encoder overflow")
	}
	for i, r := range p.AllowedResolutions {
		u, e := imageLayerFLOPs(p.ArchManifest["unet"], r)
		if e != nil || u > math.MaxUint64/2 {
			return errors.New("UNet accounting overflow")
		}
		v, e := imageLayerFLOPs(p.ArchManifest["vae"], r)
		if e != nil {
			return e
		}
		want := ImagePrice{2 * u, enc.Uint64(), v}
		if want.Step == 0 || want.VAE == 0 || want != p.PriceTable[i] {
			return errors.New("declared FLOPs mismatch")
		}
	}
	return nil
}

// ImageActual evaluates §4.1 in integer FLOPs/parameters, with exactly one
// base ceiling to whole subunits, then the existing ScaleFee multiplier rule.
// Both encoder forwards are in F_textenc.
func ImageActual(p *ImageProfile, res, steps uint32, ppm int64) (int64, error) {
	if p == nil || uint64(res) >= uint64(len(p.PriceTable)) || steps == 0 || steps > p.MaxSteps || ppm <= 0 {
		return 0, errors.New("invalid image quote")
	}
	f := p.PriceTable[res]
	var u, e, v uint64
	for _, c := range p.Components {
		switch c.Role {
		case "unet":
			u = c.Params
		case "text_encoder":
			e = c.Params
		case "vae":
			v = c.Params
		}
	}
	compute := new(big.Int).Mul(new(big.Int).SetUint64(f.Step), new(big.Int).SetUint64(uint64(steps)))
	compute.Add(compute, new(big.Int).SetUint64(f.Text))
	compute.Add(compute, new(big.Int).SetUint64(f.VAE))
	compute.Mul(compute, big.NewInt(7370))
	storage := new(big.Int).Mul(new(big.Int).SetUint64(u), new(big.Int).SetUint64(uint64(steps)))
	storage.Add(storage, new(big.Int).Mul(new(big.Int).SetUint64(e), big.NewInt(154)))
	storage.Add(storage, new(big.Int).SetUint64(v))
	storage.Mul(storage, big.NewInt(368))
	compute.Add(compute, storage)
	den := big.NewInt(100000000000000)
	compute.Add(compute, new(big.Int).Sub(den, big.NewInt(1)))
	compute.Quo(compute, den)
	if !compute.IsInt64() || compute.Sign() <= 0 {
		return 0, errors.New("image price overflow/zero")
	}
	actual, err := ScaleFee(compute.Int64(), ppm)
	if err != nil {
		return 0, err
	}
	if actual <= 0 {
		return 0, errors.New("scaled image price is zero")
	}
	return actual, nil
}

// ImageCalibrationObservation is the Ledger/T4 rehearsal hook: the caller
// obtains signed paired-device measurements; this pure verifier never mutates
// the published band and rejects missing or out-of-band observations.
type ImageCalibrationObservation struct {
	Step       uint32
	RelativeL2 float64
}

func ValidateImageCalibration(p *ImageProfile, observations []ImageCalibrationObservation) error {
	if p == nil || len(observations) == 0 {
		return errors.New("missing image calibration")
	}
	for _, o := range observations {
		soft, _, e := p.RangeBand.Thresholds(o.Step)
		if e != nil {
			return e
		}
		if o.Step == 0 || o.Step > p.MaxSteps || math.IsNaN(o.RelativeL2) || o.RelativeL2 < 0 || o.RelativeL2 > soft {
			return errors.New("image calibration does not reproduce")
		}
	}
	return nil
}
