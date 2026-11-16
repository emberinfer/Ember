package core

import pb "ember/proto"

// RangeCompute returns per-token work for a validated manifest and range.
// Without a table it is exactly StageWeight: attached boundary blocks do not
// add legacy work. With a table boundary atoms are additive; the lookup-only
// embedding has a one-unit floor, including when attached to decoder layers.
func RangeCompute(m *pb.ModelManifest, start, end uint32, embedding, head bool) uint64 {
	if len(m.GetLayerFlops()) == 0 {
		return uint64(StageWeight(start, end))
	}
	var work uint64
	for _, value := range m.LayerFlops[start:end] {
		work += value
	}
	if embedding {
		value := m.EmbeddingFlops
		if value == 0 {
			value = 1
		}
		work += value
	}
	if head {
		work += m.HeadFlops
	}
	return work
}

// TotalCompute preserves the legacy freeze denominator L without a table.
// Published compute tables are validated to fit uint64, including boundary atoms.
func TotalCompute(m *pb.ModelManifest) uint64 {
	if len(m.GetLayerFlops()) == 0 {
		return uint64(m.GetTotalLayers())
	}
	return RangeCompute(m, 0, m.TotalLayers, true, true)
}
