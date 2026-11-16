package chain

import (
	pb "ember/proto"
	"errors"
	"fmt"
	"google.golang.org/protobuf/proto"
	"math/big"
	"strings"
)

func (s *State) validateGPUBudget(p *pb.ResourceProfile) error {
	b := p.GpuBudget
	if b == nil {
		return nil
	}
	if b.MemoryPermille < 1 || b.MemoryPermille > 1000 || b.ComputePermille < 1 || b.ComputePermille > 1000 || b.AcceleratorMemoryBytes == 0 {
		return errors.New("gpu_budget requires memory/compute permille in 1..1000 and positive accelerator memory")
	}
	_, raw := modelArtifacts(s.Models[p.ChainId])
	m := &pb.ModelManifest{}
	if len(raw) == 0 || proto.Unmarshal(raw, m) != nil {
		return errors.New("gpu_budget requires anchored weight manifest")
	}
	selected, e := selectScheme(m, p.SchemeHash)
	if e != nil {
		return e
	}
	// Charge the bytes of all files needed by the claimed span. A monolithic file
	// cannot be safely fractioned by layer count; absent tensor byte proofs, use
	// the complete file as a conservative bound (never an invented fraction).
	names := map[string]bool{}
	partial := p.ShardTotalLayers > 0 && (p.ShardStartLayer > 0 || p.ShardEndLayer < p.ShardTotalLayers)
	if partial {
		for _, atom := range selected.Shards {
			if atom.EndLayer > p.ShardStartLayer && atom.StartLayer < p.ShardEndLayer || atom.HasEmbedding && p.ShardHasEmbedding || atom.HasLmHead && p.ShardHasLmHead {
				for _, name := range atom.Artifacts {
					names[name] = true
				}
			}
		}
	}
	need := new(big.Int)
	files := 0
	for _, a := range m.Artifacts {
		if strings.HasSuffix(a.Name, ".safetensors") || strings.HasSuffix(a.Name, ".bin") || strings.HasSuffix(a.Name, ".gguf") {
			if partial && len(names) > 0 && !names[a.Name] {
				continue
			}
			if a.Size <= 0 {
				return errors.New("invalid declared weight size")
			}
			need.Add(need, big.NewInt(a.Size))
			files++
		}
	}
	if files == 0 {
		return errors.New("gpu_budget has no verifiable weight bytes")
	}
	available := new(big.Int).Mul(new(big.Int).SetUint64(b.AcceleratorMemoryBytes), new(big.Int).SetUint64(uint64(b.MemoryPermille)))
	available.Quo(available, big.NewInt(1000))
	if available.Cmp(need) < 0 {
		return fmt.Errorf("gpu_budget memory %s bytes cannot fit claimed weights %s bytes", available, need)
	}
	return nil
}
