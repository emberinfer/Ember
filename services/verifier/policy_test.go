package verifier

import (
	pb "ember/proto"
	"strings"
	"testing"
)

func TestSmolLMNumericPolicy(t *testing.T) {
	g := &pb.ChainGenesis{ChainId: "ember-smollm2-360m-gpu-v1", ExecutionProfileVer: "transformers-smollm2-gpu-v1"}
	if ValidateNumericPolicy(g) == nil {
		t.Fatal("second model accepted without calibrated policy")
	}
	runtime := "transformers/4.45.2|torch=2.11.0|cuda=13.0|gpu=thor"
	g.NumericAuditPolicy = &pb.NumericAuditPolicy{Metric: "rank-probability-v1", TopK: 16,
		Layers: []string{"0", "14", "27"}, CalibrationRef: make([]byte, 32),
		Bands: []*pb.ToleranceBand{{ProducerEngine: runtime, ProducerDevice: "cuda", ReferenceEngine: runtime, ReferenceDevice: "cuda", Tolerance: .001, Mode: "enforce"}}}
	if err := ValidateNumericPolicy(g); err != nil {
		t.Fatal(err)
	}
	g.NumericAuditPolicy.Bands[0].ReferenceDevice = "cpu"
	if ValidateNumericPolicy(g) == nil {
		t.Fatal("second model inherited a CPU enforcement band")
	}
}

func TestNumericPolicyGuards(t *testing.T) {
	legacy := &pb.ChainGenesis{ChainId: "ember-qwen2.5-1.5b", ExecutionProfileVer: "transformers-qwen2.5-v1"}
	if err := ValidateNumericPolicy(legacy); err != nil {
		t.Fatal(err)
	}
	g := &pb.ChainGenesis{ChainId: "ember-qwen2.5-1.5b-union-v1", ExecutionProfileVer: "transformers-qwen2.5-union-v1"}
	if ValidateNumericPolicy(g) == nil {
		t.Fatal("missing signed policy accepted")
	}
	g.NumericAuditPolicy = &pb.NumericAuditPolicy{Metric: "union-interval-v1", TopK: 16,
		Layers: []string{"0", "14", "27"}, CalibrationRef: make([]byte, 32),
		Bands: []*pb.ToleranceBand{{ProducerEngine: "cpu-runtime", ProducerDevice: "cpu", ReferenceEngine: "cpu-runtime", ReferenceDevice: "cpu", Tolerance: .001, Mode: "enforce"}}}
	if err := ValidateNumericPolicy(g); err != nil {
		t.Fatal(err)
	}
	g.NumericAuditPolicy.Bands[0].ProducerDevice = "cuda"
	if ValidateNumericPolicy(g) == nil {
		t.Fatal("cross-device enforcement accepted")
	}
	g.NumericAuditPolicy.Bands[0].Mode = "observe"
	if err := ValidateNumericPolicy(g); err != nil {
		t.Fatal(err)
	}
	g.ChainId = legacy.ChainId
	if ValidateNumericPolicy(g) == nil {
		t.Fatal("old chain metric replaced")
	}
}

func TestNumericPolicySampleLayers(t *testing.T) {
	genesis := &pb.ChainGenesis{ChainId: "ember-qwen2.5-0.5b-gate-v1", ExecutionProfileVer: "transformers-qwen2.5-union-v1",
		NumericAuditPolicy: &pb.NumericAuditPolicy{Metric: "union-interval-v1", TopK: 16, CalibrationRef: make([]byte, 32),
			Bands: []*pb.ToleranceBand{{ProducerEngine: "cpu-runtime", ProducerDevice: "cpu", ReferenceEngine: "cpu-runtime", ReferenceDevice: "cpu", Tolerance: .001, Mode: "enforce"}}}}
	for _, layers := range [][]string{{"0", "11", "23"}, {"0", "14", "27"}, {"23"}, {"0", "1", "9", "10", "100000000000000000000"}} {
		genesis.NumericAuditPolicy.Layers = layers
		if err := ValidateNumericPolicy(genesis); err != nil {
			t.Fatalf("layers %q: %v", layers, err)
		}
	}
	for _, layers := range [][]string{nil, {""}, {"11", "0"}, {"9", "9"}, {"10", "9"}, {"01"}, {"+1"}, {"-1"}, {" 1"}, {"１"}} {
		genesis.NumericAuditPolicy.Layers = layers
		if err := ValidateNumericPolicy(genesis); err == nil {
			t.Fatalf("accepted layers %q", layers)
		}
	}
}

func TestPipelineUnionPolicy(t *testing.T) {
	g := &pb.ChainGenesis{ChainId: "ember-qwen2.5-1.5b-pipeline3",
		ExecutionProfileVer: "transformers-qwen2.5-pipeline3-v1",
		NumericAuditPolicy: &pb.NumericAuditPolicy{Metric: "union-interval-v1", TopK: 16,
			Layers: []string{"0", "14", "27"}, CalibrationRef: make([]byte, 32),
			Bands: []*pb.ToleranceBand{{ProducerEngine: "pipeline[transformers-shard/4|torch=2]", ProducerDevice: "cpu",
				ReferenceEngine: "transformers/4|torch=2", ReferenceDevice: "cpu", Tolerance: .02, Mode: "enforce"}}}}
	if err := ValidateNumericPolicy(g); err != nil {
		t.Fatal(err)
	}
	g.ExecutionProfileVer = strings.Replace(g.ExecutionProfileVer, "pipeline3", "other", 1)
	if ValidateNumericPolicy(g) == nil {
		t.Fatal("unsupported profile accepted")
	}
}

func TestS19X86NumericPolicy(t *testing.T) {
	g := &pb.ChainGenesis{ChainId: "ember-qwen2.5-1.5b-s19-x86-v1",
		ExecutionProfileVer: "transformers-qwen2.5-s19-x86-v1",
		NumericAuditPolicy: &pb.NumericAuditPolicy{Metric: "union-interval-v1", TopK: 16,
			Layers: []string{"0", "14", "27"}, CalibrationRef: make([]byte, 32),
			Bands: []*pb.ToleranceBand{{ProducerEngine: "transformers/4.45.2|torch=2.5.1+cu124", ProducerDevice: "cpu",
				ReferenceEngine: "transformers/4.45.2|torch=2.5.1+cu124", ReferenceDevice: "cpu", Tolerance: .03, Mode: "enforce"},
				{ProducerEngine: "transformers/4.45.2|torch=2.5.1+cu124", ProducerDevice: "cpu",
					ReferenceEngine: "transformers/4.45.2|torch=2.5.1", ReferenceDevice: "cpu", Tolerance: .3, Mode: "observe"}}}}
	if err := ValidateNumericPolicy(g); err != nil {
		t.Fatal(err)
	}
	g.NumericAuditPolicy.Bands[1].Mode = "enforce"
	if ValidateNumericPolicy(g) == nil {
		t.Fatal("S19 cross-runtime enforcement accepted")
	}
}

func TestS21PrepNumericPolicy(t *testing.T) {
	g := &pb.ChainGenesis{ChainId: "ember-qwen2.5-1.5b-s21-prep-v1",
		ExecutionProfileVer: "transformers-qwen2.5-s21-prep-v1",
		NumericAuditPolicy: &pb.NumericAuditPolicy{Metric: "union-interval-v1", TopK: 16,
			Layers: []string{"0", "14", "27"}, CalibrationRef: make([]byte, 32),
			Bands: []*pb.ToleranceBand{{ProducerEngine: "transformers/4.45.2|torch=2.5.1", ProducerDevice: "cpu",
				ReferenceEngine: "transformers/4.45.2|torch=2.5.1", ReferenceDevice: "cpu", Tolerance: .001, Mode: "enforce"},
				{ProducerEngine: "transformers/4.45.2|torch=2.5.1+cu124", ProducerDevice: "cpu",
					ReferenceEngine: "transformers/4.45.2|torch=2.5.1", ReferenceDevice: "cpu", Tolerance: .3, Mode: "observe"}}}}
	if err := ValidateNumericPolicy(g); err != nil {
		t.Fatal(err)
	}
	g.NumericAuditPolicy.Bands[1].Mode = "enforce"
	if ValidateNumericPolicy(g) == nil {
		t.Fatal("S21 cross-runtime enforcement accepted")
	}
}

func TestPermissionlessProfileNumericPolicy(t *testing.T) {
	g := &pb.ChainGenesis{ChainId: "ember-publisher-cpu", ExecutionProfileVer: "publisher-cpu-v1",
		NumericAuditPolicy: &pb.NumericAuditPolicy{Metric: "union-interval-v1", TopK: 16, Layers: []string{"0", "14", "27"}, CalibrationRef: make([]byte, 32), Bands: []*pb.ToleranceBand{{ProducerEngine: "cpu-runtime", ProducerDevice: "cpu", ReferenceEngine: "cpu-runtime", ReferenceDevice: "cpu", Tolerance: .001, Mode: "enforce"}}}}
	if err := ValidateNumericPolicy(g); err != nil {
		t.Fatal(err)
	}
	g.NumericAuditPolicy.Bands[0].ProducerDevice = "cuda"
	if ValidateNumericPolicy(g) == nil {
		t.Fatal("publisher bypassed cross-device guard")
	}
}

func TestGPUNumericPolicy(t *testing.T) {
	ident := "transformers/5.20.2|torch=2.14.0+cu130|cuda=13.0|gpu=gb10"
	g := &pb.ChainGenesis{ChainId: "ember-gpu-unit", ExecutionProfileVer: GPUProfile,
		NumericAuditPolicy: &pb.NumericAuditPolicy{Metric: "rank-probability-v1", TopK: 16,
			Layers: []string{"0", "14", "27"}, CalibrationRef: make([]byte, 32),
			Bands: []*pb.ToleranceBand{{ProducerEngine: ident, ProducerDevice: "cuda",
				ReferenceEngine: ident, ReferenceDevice: "cuda", Tolerance: .001, Mode: "enforce"}}}}
	if err := ValidateNumericPolicy(g); err != nil {
		t.Fatal(err)
	}
	band := g.NumericAuditPolicy.Bands[0]
	band.ReferenceEngine = strings.Replace(ident, "gpu=gb10", "gpu=thor", 1)
	if err := ValidateNumericPolicy(g); err != nil {
		t.Fatal("explicit signed cross-class band rejected:", err)
	}
	for _, mutate := range []func(){
		func() { band.ReferenceDevice = "cpu" },
		func() { band.ReferenceEngine = "transformers/unpinned" },
		func() { band.Tolerance = 1 },
		func() { g.NumericAuditPolicy.Metric = "union-interval-v1" },
	} {
		band.ReferenceDevice, band.ReferenceEngine, band.Tolerance = "cuda", ident, .001
		g.NumericAuditPolicy.Metric = "rank-probability-v1"
		mutate()
		if ValidateNumericPolicy(g) == nil {
			t.Fatal("invalid GPU policy accepted")
		}
	}
}

func TestRangeNumericPolicy(t *testing.T) {
	g := &pb.ChainGenesis{ChainId: "ember-range", ExecutionProfileVer: "transformers-qwen2.5-pipeline-range-v2", NumericAuditPolicy: &pb.NumericAuditPolicy{TopK: 16, CalibrationRef: make([]byte, 32), RangeBand: &pb.RangeBandParams{Metric: "relative-l2-v1", HardMultiplier: 3, Base: []*pb.RangeDeviceBase{{ProducerDevice: "cpu", ReferenceDevice: "cuda", Base: .02}}, HeadBase: []*pb.RangeDeviceBase{{ProducerDevice: "cpu", ReferenceDevice: "cuda", Base: .03}}}}}
	if err := ValidateNumericPolicy(g); err != nil {
		t.Fatal(err)
	}
	g.NumericAuditPolicy.RangeBand.HeadBase[0].Base = -1
	if ValidateNumericPolicy(g) == nil {
		t.Fatal("invalid head base accepted")
	}
}
