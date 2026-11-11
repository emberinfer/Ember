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
	runtime := "transformers/9.9.8|torch=9.9.7|cuda=99.9|gpu=thor"
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

func TestCPUStrictNumericPolicy(t *testing.T) {
	g := &pb.ChainGenesis{ChainId: "ember-qwen2.5-1.5b-cpu-strict-v1",
		ExecutionProfileVer: "transformers-qwen2.5-cpu-strict-v1",
		NumericAuditPolicy: &pb.NumericAuditPolicy{Metric: "union-interval-v1", TopK: 16,
			Layers: []string{"0", "14", "27"}, CalibrationRef: make([]byte, 32),
			Bands: []*pb.ToleranceBand{{ProducerEngine: "transformers/9.9.8|torch=9.9.9+cu999", ProducerDevice: "cpu",
				ReferenceEngine: "transformers/9.9.8|torch=9.9.9+cu999", ReferenceDevice: "cpu", Tolerance: .03, Mode: "enforce"},
				{ProducerEngine: "transformers/9.9.8|torch=9.9.9+cu999", ProducerDevice: "cpu",
					ReferenceEngine: "transformers/9.9.8|torch=9.9.9", ReferenceDevice: "cpu", Tolerance: .3, Mode: "observe"}}}}
	if err := ValidateNumericPolicy(g); err != nil {
		t.Fatal(err)
	}
	g.NumericAuditPolicy.Bands[1].Mode = "enforce"
	if ValidateNumericPolicy(g) == nil {
		t.Fatal("CPU strict cross-runtime enforcement accepted")
	}
}

func TestCPUPreviewNumericPolicy(t *testing.T) {
	g := &pb.ChainGenesis{ChainId: "ember-qwen2.5-1.5b-cpu-preview-v1",
		ExecutionProfileVer: "transformers-qwen2.5-cpu-preview-v1",
		NumericAuditPolicy: &pb.NumericAuditPolicy{Metric: "union-interval-v1", TopK: 16,
			Layers: []string{"0", "14", "27"}, CalibrationRef: make([]byte, 32),
			Bands: []*pb.ToleranceBand{{ProducerEngine: "transformers/9.9.8|torch=9.9.9", ProducerDevice: "cpu",
				ReferenceEngine: "transformers/9.9.8|torch=9.9.9", ReferenceDevice: "cpu", Tolerance: .001, Mode: "enforce"},
				{ProducerEngine: "transformers/9.9.8|torch=9.9.9+cu999", ProducerDevice: "cpu",
					ReferenceEngine: "transformers/9.9.8|torch=9.9.9", ReferenceDevice: "cpu", Tolerance: .3, Mode: "observe"}}}}
	if err := ValidateNumericPolicy(g); err != nil {
		t.Fatal(err)
	}
	g.NumericAuditPolicy.Bands[1].Mode = "enforce"
	if ValidateNumericPolicy(g) == nil {
		t.Fatal("CPU preview cross-runtime enforcement accepted")
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
	ident := "transformers/9.9.8|torch=9.9.9+cu999|cuda=99.9|gpu=gb10"
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
