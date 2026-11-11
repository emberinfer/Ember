package tracker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	core "ember/core-go"
	pb "ember/proto"
)

func TestRegister(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	now := time.Now()
	ctx := context.Background()
	observed, score, err := Benchmark(ctx, func(_ context.Context, q []string) ([]string, error) { return append([]string(nil), q...), nil })
	if err != nil {
		t.Fatal(err)
	}
	p := &pb.ResourceProfile{NodeId: NodeID(key.Public().(ed25519.PublicKey)), ChainId: "ember-test", Gpu: "mock", MemoryBytes: 1024, EngineVer: "mock-v1", ExpiresUnix: now.Add(time.Minute).Unix(), ManifestRoot: bytes.Repeat([]byte{1}, 32), ExecutionProfileVer: "v1", BenchmarkResultRef: observed}
	if err = core.Sign(p, key); err != nil {
		t.Fatal(err)
	}
	tr := New()
	if err = tr.Register(p, observed, score, now); err != nil {
		t.Fatal(err)
	}
	p.ExpiresUnix = now.Add(2 * time.Minute).Unix()
	if err = core.Sign(p, key); err != nil {
		t.Fatal(err)
	}
	if err = tr.Heartbeat(p, now); err != nil {
		t.Fatal(err)
	}
	if len(tr.Alive(now)) != 1 || len(tr.Alive(now.Add(3*time.Minute))) != 0 {
		t.Fatal("ttl invalid")
	}
	if err = tr.Register(p, observed, score-1, now); err == nil {
		t.Fatal("unmeasured benchmark accepted")
	}
	p.Gpu = "fake"
	if err = tr.Register(p, observed, score, now); err == nil {
		t.Fatal("unsigned alteration accepted")
	}
}

func TestPipelineRuntimeProfile(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, 32))
	now := time.Now()
	observed, score, err := Benchmark(context.Background(), func(_ context.Context, q []string) ([]string, error) { return q, nil })
	if err != nil {
		t.Fatal(err)
	}
	p := &pb.ResourceProfile{NodeId: NodeID(key.Public().(ed25519.PublicKey)), ChainId: "ember-pipeline",
		Gpu: "pipeline", MemoryBytes: 1, EngineVer: "pipeline-v1", BenchmarkResultRef: observed,
		ExpiresUnix: now.Add(time.Minute).Unix(), ManifestRoot: bytes.Repeat([]byte{1}, 32),
		ExecutionProfileVer: "pipeline-v1", RequestedDevice: "pipeline", ActualDevice: "pipeline",
		DeviceReason: "LAN pipeline stages", TorchVersion: "2", TransformersVersion: "4",
		ShardParametersHash: bytes.Repeat([]byte{3}, 32)}
	if err = core.Sign(p, key); err != nil {
		t.Fatal(err)
	}
	if err = New().Register(p, observed, score, now); err != nil {
		t.Fatal(err)
	}
	p.ActualDevice = "cpu"
	if err = core.Sign(p, key); err != nil {
		t.Fatal(err)
	}
	if err = New().Register(p, observed, score, now); err == nil {
		t.Fatal("invalid pipeline metadata accepted")
	}
}

func TestShardStageRuntimeProfile(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{4}, 32))
	now := time.Now()
	observed, score, err := Benchmark(context.Background(), func(_ context.Context, q []string) ([]string, error) { return q, nil })
	if err != nil {
		t.Fatal(err)
	}
	p := &pb.ResourceProfile{NodeId: NodeID(key.Public().(ed25519.PublicKey)), ChainId: "ember-pipeline",
		Gpu: "cpu", MemoryBytes: 16 << 30, EngineVer: "transformers-shard-v1",
		BenchmarkResultRef: observed, ExpiresUnix: now.Add(time.Minute).Unix(),
		ManifestRoot: bytes.Repeat([]byte{2}, 32), ExecutionProfileVer: "transformers-qwen2.5-pipeline3-v1",
		RequestedDevice: "cpu", ActualDevice: "cpu", DeviceReason: "explicit CPU",
		TorchVersion: "2.5.1", TransformersVersion: "4.45.2", AvailableMemoryBytes: 16 << 30,
		ShardIndex: 1, ShardStartLayer: 9, ShardEndLayer: 18, ShardTotalLayers: 28,
		ShardParametersHash: bytes.Repeat([]byte{3}, 32), ShardEndpoint: "127.0.0.1:1",
		ShardTlsFingerprint: bytes.Repeat([]byte{4}, 32), EngineCapabilities: &pb.EngineCapabilities{
			SpiVersion: "ember-engine-spi-v1", EngineName: "transformers-shard", EngineVersion: "4.45.2",
			NumericalFormat: "bfloat16", DeviceClass: "cpu", EvidenceLevels: []string{"shard-layer"},
			ScoreKind: "raw-logits", MaxTopK: 16, Roles: []string{"shard-stage"}}}
	if err = core.Sign(p, key); err != nil {
		t.Fatal(err)
	}
	if err = New().Register(p, observed, score, now); err != nil {
		t.Fatal(err)
	}
	p.ShardTlsFingerprint = nil
	_ = core.Sign(p, key)
	if err = New().Register(p, observed, score, now); err == nil {
		t.Fatal("incomplete shard endpoint accepted")
	}
}

func TestEngineCapabilityDowngrade(t *testing.T) {
	p := &pb.ResourceProfile{Gpu: "remote", EngineVer: "vllm-v1", ExecutionProfileVer: "transformers-qwen2.5-v1"}
	if ValidateCapabilities(p) == nil {
		t.Fatal("missing HTTP declaration accepted")
	}
	p.EngineCapabilities = &pb.EngineCapabilities{
		SpiVersion: "ember-engine-spi-v1", EngineName: "vllm", EngineVersion: "fixture",
		NumericalFormat: "unknown", DeviceClass: "remote", EvidenceLevels: []string{"topk-logprobs"},
		ScoreKind: "logprobs", MaxTopK: 16, Roles: []string{"model"},
	}
	if ValidateCapabilities(p) == nil {
		t.Fatal("logprobs admitted to raw-logit profile")
	}
	p.EngineCapabilities.EvidenceLevels = []string{"full"}
	p.EngineCapabilities.ScoreKind = "raw-logits"
	p.EngineCapabilities.ForcedPrefixReplay = true
	if ValidateCapabilities(p) == nil {
		t.Fatal("HTTP full-evidence forgery accepted")
	}
	p.Gpu = "cpu"
	p.EngineVer = "transformers-v1"
	p.EngineCapabilities.EngineName = "transformers"
	p.EngineCapabilities.DeviceClass = "cpu"
	p.EngineCapabilities.NumericalFormat = "bfloat16"
	if err := ValidateCapabilities(p); err != nil {
		t.Fatal(err)
	}
	p.EngineCapabilities = nil
	if err := ValidateCapabilities(p); err != nil {
		t.Fatal("legacy profile broken:", err)
	}
}

func TestGPUProfileRequiresExplicitCUDA(t *testing.T) {
	profile := func() *pb.ResourceProfile {
		return &pb.ResourceProfile{ExecutionProfileVer: "transformers-qwen2.5-gpu-v1", Gpu: "cuda",
			RequestedDevice: "cuda", ActualDevice: "cuda", TorchVersion: "9.9.9+cu999",
			TransformersVersion: "9.9.8", CudaVersion: "99.9", AcceleratorName: "NVIDIA GB10",
			EngineCapabilities: &pb.EngineCapabilities{SpiVersion: "ember-engine-spi-v1",
				EngineName: "transformers", EngineVersion: "9.9.8", NumericalFormat: "bfloat16",
				DeviceClass: "cuda", EvidenceLevels: []string{"full"}, ScoreKind: "raw-logits",
				MaxTopK: 16, ForcedPrefixReplay: true, Roles: []string{"model"}}}
	}
	if err := ValidateCapabilities(profile()); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*pb.ResourceProfile){
		func(p *pb.ResourceProfile) { p.RequestedDevice = "auto" },
		func(p *pb.ResourceProfile) { p.ActualDevice = "cpu" },
		func(p *pb.ResourceProfile) { p.EngineCapabilities.NumericalFormat = "float16" },
		func(p *pb.ResourceProfile) { p.EngineCapabilities = nil },
		func(p *pb.ResourceProfile) { p.CudaVersion = "" },
		func(p *pb.ResourceProfile) { p.TransformersVersion = "another-build" },
	} {
		p := profile()
		mutate(p)
		if ValidateCapabilities(p) == nil {
			t.Fatal("incompatible GPU runtime admitted")
		}
	}
}
