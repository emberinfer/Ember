package core

import (
	"crypto/sha256"
	pb "ember/proto"
	"encoding/binary"
	"errors"
)

// StageSpan maps the embedding, decoder layers and head onto a strictly
// increasing execution order without pretending embedding/head are layers.
// Layer coordinates remain [a,b); embedding=[0,0), head=[L,L).
func StageSpan(start, end, total uint32, embedding, head bool) (uint32, uint32, error) {
	if total == 0 || total > ^uint32(0)-2 || start > end || end > total || (embedding && start != 0) || (head && end != total) ||
		(start == end && !((start == 0 && embedding && !head) || (start == total && head && !embedding))) {
		return 0, 0, errors.New("invalid stage unit boundaries")
	}
	a, b := start+1, end+1
	if embedding {
		a = 0
	}
	if head {
		b = total + 2
	}
	if b <= a {
		return 0, 0, errors.New("empty stage")
	}
	return a, b, nil
}

// StageWeight preserves legacy layer attribution. A standalone boundary unit
// has weight one; profiles with explicit embedding/head therefore total L+2.
func StageWeight(start, end uint32) uint32 {
	if end > start {
		return end - start
	}
	return 1
}

func ValidUnitKind(kind string, start, end, total uint32, embedding, head bool) bool {
	switch kind {
	case "":
		return start < end
	case "embedding":
		return start == 0 && end == 0 && embedding && !head
	case "lm_head":
		return start == total && end == total && head && !embedding
	case "layers":
		return start < end && !embedding && !head
	case "range":
		return start < end && (!embedding || start == 0) && (!head || end == total)
	default:
		return false
	}
}

func RangeParameters(root []byte, start, end uint32) []byte {
	h := sha256.New()
	h.Write([]byte("ember/capability-range/v1\x00"))
	h.Write(root)
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], start)
	h.Write(b[:])
	binary.BigEndian.PutUint32(b[:], end)
	h.Write(b[:])
	return h.Sum(nil)
}

func ExecutedSpan(p *pb.ResourceProfile, start, end uint32) (uint32, uint32, error) {
	if p.ShardDynamicRange {
		if start < p.ShardStartLayer || end > p.ShardEndLayer || end <= start {
			return 0, 0, errors.New("execution exceeds capability range")
		}
		return StageSpan(start, end, p.ShardTotalLayers, start == 0 && p.ShardHasEmbedding, end == p.ShardTotalLayers && p.ShardHasLmHead)
	}
	if start != p.ShardStartLayer || end != p.ShardEndLayer {
		return 0, 0, errors.New("executed range differs from pinned stage")
	}
	return StageSpan(start, end, p.ShardTotalLayers, p.ShardHasEmbedding, p.ShardHasLmHead)
}

func AllowedRange(m *pb.ModelManifest, start, end uint32) bool {
	if !m.DynamicRanges || m.MinStageLayers == 0 || end <= start || end > m.TotalLayers || end-start < m.MinStageLayers {
		return false
	}
	if len(m.AllowedLayerCuts) > 0 {
		a, b := false, false
		for _, cut := range m.AllowedLayerCuts {
			if cut == start {
				a = true
			}
			if cut == end {
				b = true
			}
		}
		return a && b
	}
	return start%m.MinStageLayers == 0 && (end%m.MinStageLayers == 0 || end == m.TotalLayers)
}

func ActivationHash(manifest []byte, layer uint32, raw []byte, shape []uint32) []byte {
	h := sha256.New()
	h.Write([]byte("ember/activation-boundary/v1\x00"))
	h.Write(manifest)
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], layer)
	h.Write(b[:])
	binary.BigEndian.PutUint32(b[:], uint32(len(shape)))
	h.Write(b[:])
	for _, n := range shape {
		binary.BigEndian.PutUint32(b[:], n)
		h.Write(b[:])
	}
	h.Write(raw)
	return h.Sum(nil)
}
