package tracker

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"time"

	core "ember/core-go"
	pb "ember/proto"
	"google.golang.org/protobuf/proto"
)

var Questions = []string{"ember benchmark: echo alpha", "ember benchmark: echo beta"}

type Runner func(context.Context, []string) ([]string, error)
type Tracker struct {
	mu       sync.RWMutex
	profiles map[string]*pb.ResourceProfile
	scores   map[string]int
}

func New() *Tracker {
	return &Tracker{profiles: make(map[string]*pb.ResourceProfile), scores: make(map[string]int)}
}
func NodeID(public []byte) string { return core.NodeID(public) }
func Benchmark(ctx context.Context, run Runner) ([]byte, int, error) {
	answers, err := run(ctx, Questions)
	if err != nil {
		return nil, 0, err
	}
	score := 0
	for i, q := range Questions {
		if i < len(answers) && answers[i] == q {
			score++
		}
	}
	h := sha256.New()
	for _, a := range answers {
		h.Write([]byte(a))
		h.Write([]byte{0})
	}
	return h.Sum(nil), score, nil
}
func (t *Tracker) Register(p *pb.ResourceProfile, observed []byte, score int, now time.Time) error {
	if err := core.Verify(p); err != nil {
		return err
	}
	if p.NodeId != NodeID(p.Signer) || p.ChainId == "" || p.ExpiresUnix <= now.Unix() || p.ExpiresUnix > now.Add(5*time.Minute).Unix() || p.EngineVer == "" || p.MemoryBytes == 0 || len(p.ManifestRoot) != 32 || p.ExecutionProfileVer == "" {
		return errors.New("invalid resource profile")
	}
	isShard := p.ShardTotalLayers > 0 && (p.ShardEndLayer > p.ShardStartLayer || p.ShardHasEmbedding || p.ShardHasLmHead)
	if strings.HasPrefix(p.EngineVer, "transformers-") {
		if (p.RequestedDevice != "auto" && p.RequestedDevice != "cpu" && p.RequestedDevice != "cuda") ||
			(p.ActualDevice != "cpu" && p.ActualDevice != "cuda") || p.Gpu != p.ActualDevice ||
			p.DeviceReason == "" || p.TorchVersion == "" || p.TransformersVersion == "" || p.AvailableMemoryBytes == 0 {
			return errors.New("incomplete transformer runtime profile")
		}
		if p.ActualDevice == "cuda" && (p.CudaVersion == "" || p.AcceleratorName == "") {
			return errors.New("incomplete CUDA runtime profile")
		}
		if isShard && (p.ShardTotalLayers == 0 || p.ShardEndLayer > p.ShardTotalLayers ||
			len(p.ShardParametersHash) != 32 || p.ShardEndpoint == "" || len(p.ShardTlsFingerprint) != 32) {
			return errors.New("incomplete shard stage profile")
		}
	} else if strings.HasPrefix(p.EngineVer, "pipeline-") {
		if p.Gpu != "pipeline" || p.RequestedDevice != "pipeline" || p.ActualDevice != "pipeline" ||
			p.DeviceReason == "" || p.TorchVersion == "" || p.TransformersVersion == "" ||
			len(p.ShardParametersHash) != 32 {
			return errors.New("incomplete pipeline runtime profile")
		}
	}
	if err := ValidateCapabilities(p); err != nil {
		return err
	}
	if score < len(Questions) || !bytesEqual(observed, p.BenchmarkResultRef) {
		return errors.New("benchmark did not pass")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.profiles[p.NodeId] = proto.Clone(p).(*pb.ResourceProfile)
	t.scores[p.NodeId] = score
	return nil
}

// Heartbeat renews only an already benchmarked identity with the same observed result.
func (t *Tracker) Heartbeat(p *pb.ResourceProfile, now time.Time) error {
	t.mu.RLock()
	old, ok := t.profiles[p.NodeId]
	score := t.scores[p.NodeId]
	t.mu.RUnlock()
	if !ok || old.ExpiresUnix <= now.Unix() || !bytesEqual(old.BenchmarkResultRef, p.BenchmarkResultRef) {
		return errors.New("benchmark registration required")
	}
	return t.Register(p, old.BenchmarkResultRef, score, now)
}
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var x byte
	for i := range a {
		x |= a[i] ^ b[i]
	}
	return x == 0
}
func (t *Tracker) Alive(now time.Time) []*pb.ResourceProfile {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []*pb.ResourceProfile{}
	for id, p := range t.profiles {
		if p.ExpiresUnix <= now.Unix() {
			delete(t.profiles, id)
			delete(t.scores, id)
		} else {
			out = append(out, proto.Clone(p).(*pb.ResourceProfile))
		}
	}
	return out
}

// ValidateCapabilities preserves absent legacy declarations, but prevents HTTP
// logprobs or incomplete declarations from claiming a full-evidence profile.
func ValidateCapabilities(p *pb.ResourceProfile) error {
	c := p.EngineCapabilities
	if core.IsGPUExecutionProfile(p.ExecutionProfileVer) {
		if c == nil || c.EngineName != "transformers" || c.DeviceClass != "cuda" || c.NumericalFormat != "bfloat16" ||
			p.RequestedDevice != "cuda" || p.ActualDevice != "cuda" || p.Gpu != "cuda" ||
			p.TorchVersion == "" || p.CudaVersion == "" || p.AcceleratorName == "" || p.TransformersVersion != c.EngineVersion {
			return errors.New("GPU profile requires explicit CUDA and complete BF16 runtime facts")
		}
	}
	full := core.IsGPUExecutionProfile(p.ExecutionProfileVer) || strings.HasPrefix(p.ExecutionProfileVer, "transformers-qwen2.5-") || strings.HasPrefix(p.ExecutionProfileVer, "qwen2.5-")
	if c == nil {
		if strings.HasPrefix(p.EngineVer, "llamacpp-") || strings.HasPrefix(p.EngineVer, "vllm-") || strings.Contains(p.ExecutionProfileVer, "spi-v1") || p.ExecutionProfileVer == "transformers-qwen2.5-union-v1" || (full && !strings.HasPrefix(p.EngineVer, "transformers-") && !strings.HasPrefix(p.EngineVer, "pipeline-")) {
			return errors.New("engine capability declaration required")
		}
		return nil // existing legacy signed profiles and vectors
	}
	if c.SpiVersion != "ember-engine-spi-v1" || c.EngineName == "" || c.EngineVersion == "" || c.DeviceClass != p.Gpu || c.NumericalFormat == "" || len(c.EvidenceLevels) == 0 || len(c.Roles) == 0 || c.MaxTopK == 0 || c.MaxTopK > 1024 {
		return errors.New("invalid engine capabilities")
	}
	hasFull, hasShard := false, false
	for _, level := range c.EvidenceLevels {
		switch level {
		case "full":
			hasFull = true
		case "shard-layer":
			hasShard = true
		case "topk-logprobs", "none", "mock":
		default:
			return errors.New("unknown evidence level")
		}
	}
	if (c.ScoreKind != "raw-logits" && c.ScoreKind != "logprobs") || (hasFull && (c.ScoreKind != "raw-logits" || !c.ForcedPrefixReplay || c.MaxTopK < 16)) ||
		(hasShard && (c.ScoreKind != "raw-logits" || c.MaxTopK < 16 || c.ForcedPrefixReplay)) {
		return errors.New("inconsistent evidence capabilities")
	}
	if (c.EngineName == "llama.cpp" || c.EngineName == "vllm") && (hasFull || c.ScoreKind != "logprobs" || c.ForcedPrefixReplay || c.DeviceClass != "remote") {
		return errors.New("HTTP backend cannot claim full evidence or replay")
	}
	if full && !hasFull && !hasShard {
		return errors.New("execution profile requires full evidence")
	}
	if hasShard && (p.ShardEndLayer < p.ShardStartLayer || !contains(c.Roles, "shard-stage") || !core.ValidUnitKind(p.ShardUnitKind, p.ShardStartLayer, p.ShardEndLayer, p.ShardTotalLayers, p.ShardHasEmbedding, p.ShardHasLmHead)) {
		return errors.New("shard evidence requires a shard stage profile")
	}
	return nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
