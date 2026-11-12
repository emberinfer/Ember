package verifier

import (
	"errors"
	"math"
	"regexp"
	"strings"

	core "ember/core-go"
	pb "ember/proto"
)

const GPUProfile = "transformers-qwen2.5-gpu-v1"

var gpuFingerprint = regexp.MustCompile(`^transformers/[^|\s]+\|torch=[^|\s]+\|cuda=[0-9]+\.[0-9]+\|gpu=(gb10|thor|orin)$`)

// ValidateNumericPolicy only opts a new profile into the new metric. Legacy
// genesis bytes retain their exact-ID comparator and numeric tolerance.
func ValidateNumericPolicy(g *pb.ChainGenesis) error {
	p := g.NumericAuditPolicy
	gpuProfile := core.IsGPUExecutionProfile(g.ExecutionProfileVer)
	numericProfile := g.ExecutionProfileVer == "transformers-qwen2.5-union-v1" ||
		g.ExecutionProfileVer == "transformers-qwen2.5-cpu-strict-v1" ||
		g.ExecutionProfileVer == "transformers-qwen2.5-cpu-preview-v1" ||
		g.ExecutionProfileVer == "transformers-qwen2.5-cpu-v1"
	if p == nil {
		if numericProfile || gpuProfile {
			return errors.New("union profile requires signed numeric policy")
		}
		return nil
	}
	pipelineProfile := strings.HasPrefix(g.ExecutionProfileVer, "transformers-qwen2.5-pipeline")
	metric := "union-interval-v1"
	if gpuProfile {
		metric = "rank-probability-v1"
	}
	if g.ChainId == "ember-qwen2.5-1.5b" || g.ExecutionProfileVer == "" || p.Metric != metric || p.TopK != 16 || len(p.Layers) != 3 || p.Layers[0] != "0" || p.Layers[1] != "14" || p.Layers[2] != "27" || len(p.CalibrationRef) != 32 || len(p.Bands) == 0 {
		return errors.New("invalid or unsupported signed numeric policy")
	}
	seen := map[[4]string]bool{}
	for _, b := range p.Bands {
		pair := [4]string{b.ProducerEngine, b.ProducerDevice, b.ReferenceEngine, b.ReferenceDevice}
		if b.ProducerEngine == "" || b.ProducerDevice == "" || b.ReferenceEngine == "" || b.ReferenceDevice == "" || seen[pair] || math.IsNaN(b.Tolerance) || math.IsInf(b.Tolerance, 0) || b.Tolerance <= 0 || (b.Mode != "enforce" && b.Mode != "observe") {
			return errors.New("invalid tolerance band")
		}
		pipelinePair := pipelineProfile && strings.HasPrefix(b.ProducerEngine, "pipeline[") &&
			strings.HasPrefix(b.ReferenceEngine, "transformers/") && b.ProducerDevice == b.ReferenceDevice
		if gpuProfile && (b.ProducerDevice != "cuda" || b.ReferenceDevice != "cuda" ||
			!gpuFingerprint.MatchString(b.ProducerEngine) || !gpuFingerprint.MatchString(b.ReferenceEngine) || b.Tolerance >= 1) {
			return errors.New("GPU bands require pinned CUDA/class fingerprints and tolerance < 1")
		}
		if b.Mode == "enforce" && (b.ProducerEngine != b.ReferenceEngine || b.ProducerDevice != b.ReferenceDevice) && !pipelinePair && !gpuProfile {
			return errors.New("cross-runtime slashing is forbidden in union v1")
		}
		seen[pair] = true
	}
	return nil
}
