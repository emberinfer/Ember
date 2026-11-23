package core

import (
	"encoding/binary"
	"fmt"
	"math"
	"testing"
)

func TestImagePriceDesignEstimate(t *testing.T) {
	// §4.2 estimates only: not published SD1.5 accounting results.
	p := &ImageProfile{MaxSteps: 100, PriceTable: []ImagePrice{{560000000000, 38000000000, 400000000000}}, Components: []ImageComponent{{Role: "unet", Params: 860000000}, {Role: "text_encoder", Params: 123000000}, {Role: "vae", Params: 83700000}}}
	actual, e := ImageActual(p, 0, 20, 1000000)
	if e != nil || actual != 858 {
		t.Fatal(actual, e)
	}
	doubled, e := ImageActual(p, 0, 20, 2000000)
	if e != nil || doubled != 1716 {
		t.Fatal(doubled, e)
	}
	p.PriceTable[0].Step = math.MaxUint64
	p.MaxSteps = math.MaxUint32
	if _, e := ImageActual(p, 0, math.MaxUint32, math.MaxInt64); e == nil {
		t.Fatal("overflow accepted")
	}
}
func TestImageSpotKDesignTable(t *testing.T) {
	for i, n := range []uint64{20, 50, 100} {
		for j, q := range []uint32{300000, 50000, 20000} {
			want := [][3]uint64{{1, 2, 5}, {1, 5, 12}, {2, 10, 24}}[i][j]
			if got := ImageSpotK(n, q); got != want {
				t.Fatalf("%d/%d got %d want %d", n, q, got, want)
			}
		}
	}
	if ImageSpotK(1, 1) != 1 || ImageSpotK(0, 300000) != 0 {
		t.Fatal("clamp")
	}
}
func TestImageAccountingRules(t *testing.T) {
	r := ImageResolution{Width: 512, Height: 512}
	for _, tc := range []struct {
		layer ImageLayer
		want  uint64
	}{
		{ImageLayer{Op: "conv", Shape: []uint64{8, 4, 3, 3}, Scale: 8, Positions: "spatial"}, 2 * 8 * 4 * 3 * 3 * 64 * 64},
		{ImageLayer{Op: "linear", Shape: []uint64{8, 4}, Scale: 8, Positions: "text"}, 2 * 8 * 4 * 77},
		{ImageLayer{Op: "linear", Shape: []uint64{8, 4}, Scale: 8, Positions: "one"}, 2 * 8 * 4},
		{ImageLayer{Op: "ignored", Shape: []uint64{8}, Scale: 8}, 0},
	} {
		got, e := imageLayerFLOPs([]ImageLayer{tc.layer}, r)
		if e != nil || got != tc.want {
			t.Fatal(got, tc.want, e)
		}
	}
	var layers []ImageLayer
	for _, name := range []string{"to_q", "to_k", "to_v", "to_out.0"} {
		layers = append(layers, ImageLayer{Name: "attn1." + name + ".weight", Op: "linear", Shape: []uint64{8, 8}, Scale: 8, Positions: "spatial"})
	}
	got, e := imageLayerFLOPs(layers, r)
	want := uint64(4*4096*4096*8 + 8*4096*8*8)
	if e != nil || got != want {
		t.Fatal("attention", got, want, e)
	}
}
func TestSafeTensorHeadersAndStepBand(t *testing.T) {
	header := func(s string) []byte {
		return append(binary.LittleEndian.AppendUint64(nil, uint64(len(s))), []byte(s)...)
	}
	good := `{"a.weight":{"dtype":"F32","shape":[2,3],"data_offsets":[0,24]}}`
	if ts, e := ParseSafeTensorHeader(header(good)); e != nil || len(ts) != 1 {
		t.Fatal(ts, e)
	}
	for _, bad := range []string{`{}`, `{"x":{"dtype":"F32","shape":[2],"data_offsets":[1,9]}}`, `{"x":{},"x":{}}`, `{"x":{"dtype":"F32","shape":[18446744073709551615,2],"data_offsets":[0,8]}}`} {
		if _, e := ParseSafeTensorHeader(header(bad)); e == nil {
			t.Fatal("invalid header", bad)
		}
	}
	b := ImageStepBand{Metric: "relative-l2-v1", Base: 3e-6, StepCoef: 1e-6, HardMultiplier: 100, PrecisionMin: .005, PrecisionMax: .06, CalibrationHash: fmt.Sprintf("%064d", 1)}
	soft, hard, e := b.Thresholds(20)
	if e != nil || math.Abs(soft-23e-6) > 1e-18 || math.Abs(hard-.0023) > 1e-18 {
		t.Fatal(soft, hard, e)
	}
	b.StepCoef = math.NaN()
	if b.Validate() == nil {
		t.Fatal("NaN")
	}
}
func TestImageCalibrationAndMultiplierRounding(t *testing.T) {
	p := &ImageProfile{MaxSteps: 100, RangeBand: ImageStepBand{Metric: "relative-l2-v1", Base: 3e-6, StepCoef: 1e-6, HardMultiplier: 100, PrecisionMin: .005, PrecisionMax: .06, CalibrationHash: fmt.Sprintf("%064d", 1)}}
	if e := ValidateImageCalibration(p, []ImageCalibrationObservation{{Step: 1, RelativeL2: 3e-6}, {Step: 100, RelativeL2: 1e-4}}); e != nil {
		t.Fatal(e)
	}
	for _, obs := range [][]ImageCalibrationObservation{nil, {{Step: 101, RelativeL2: 0}}, {{Step: 1, RelativeL2: .01}}, {{Step: 1, RelativeL2: math.NaN()}}} {
		if ValidateImageCalibration(p, obs) == nil {
			t.Fatal("invalid calibration")
		}
	}
	p.PriceTable = []ImagePrice{{Step: 10000000000}}
	p.Components = []ImageComponent{{Role: "unet", Params: 1}}
	base, e := ImageActual(p, 0, 20, 1000000)
	if e != nil {
		t.Fatal(e)
	}
	want, _ := ScaleFee(base, 800000)
	got, e := ImageActual(p, 0, 20, 800000)
	if e != nil || got != want {
		t.Fatal("multiplier differs from text", got, want, e)
	}
}

func TestSafeTensorDtypeSizesAndBufferValidation(t *testing.T) {
	for dtype, want := range map[string]uint64{"F64": 8, "F32": 4, "F16": 2, "BF16": 2, "I64": 8, "U64": 8, "I32": 4, "U32": 4, "I16": 2, "U16": 2, "I8": 1, "U8": 1, "BOOL": 1, "F8_E4M3": 1, "F8_E5M2": 1, "F8_E8M0": 1} {
		size, e := SafeTensorDtypeSize(dtype)
		if e != nil || size != want {
			t.Fatal(dtype, size, e)
		}
		body := fmt.Sprintf(`{"x":{"dtype":%q,"shape":[2],"data_offsets":[0,%d]}}`, dtype, 2*want)
		raw := append(binary.LittleEndian.AppendUint64(nil, uint64(len(body))), []byte(body)...)
		if _, e := ParseSafeTensorHeader(raw); e != nil {
			t.Fatal(dtype, e)
		}
	}
	for _, body := range []string{
		`{"x":{"dtype":"UNKNOWN","shape":[1],"data_offsets":[0,1]}}`,
		`{"x":{"dtype":"I64","shape":[1,77],"data_offsets":[0,154]}}`,
		`{"x":{"dtype":"I64","shape":[18446744073709551615],"data_offsets":[0,8]}}`,
		`{"x":{"dtype":"I64","shape":[1],"data_offsets":[0,8]},"y":{"dtype":"F32","shape":[1],"data_offsets":[4,8]}}`,
	} {
		raw := append(binary.LittleEndian.AppendUint64(nil, uint64(len(body))), []byte(body)...)
		if _, e := ParseSafeTensorHeader(raw); e == nil {
			t.Fatal(body)
		}
	}
	good := TensorShape{Name: "text_model.embeddings.position_ids", Dtype: "I64", Shape: []uint64{1, 77}}
	if layers, e := ReconstructImageLayers("text_encoder", []TensorShape{good}); e != nil || len(layers) != 0 {
		t.Fatal(layers, e)
	}
	for _, role := range []string{"unet", "vae"} {
		if _, e := ReconstructImageLayers(role, []TensorShape{good}); e == nil {
			t.Fatal("buffer outside text_encoder")
		}
	}
	for _, shape := range [][]uint64{{77}, {1, 76}, {1, 77, 1}, {0, 77}} {
		bad := good
		bad.Shape = shape
		if _, e := ReconstructImageLayers("text_encoder", []TensorShape{bad}); e == nil {
			t.Fatal(shape)
		}
	}
}
