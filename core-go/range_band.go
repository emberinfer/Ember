package core

import (
	pb "ember/proto"
	"errors"
	"math"
)

const RangeBand = "pipeline-range-v2"

// ValidateRangeBand rejects incomplete/ambiguous calibration; coefficients are
// signed genesis data, never runtime defaults or inferred from old bands.
func ValidateRangeBand(p *pb.RangeBandParams) error {
	if p == nil || p.Metric != "relative-l2-v1" || math.IsNaN(p.DepthCoef) || math.IsInf(p.DepthCoef, 0) || p.DepthCoef < 0 || math.IsNaN(p.HardMultiplier) || math.IsInf(p.HardMultiplier, 0) || p.HardMultiplier <= 1 || len(p.Base) == 0 {
		return errors.New("invalid range band parameters")
	}
	for _, list := range [][]*pb.RangeDeviceBase{p.Base, p.HeadBase} {
		seen := map[[2]string]bool{}
		for _, b := range list {
			if b == nil {
				return errors.New("nil range device base")
			}
			key := [2]string{b.ProducerDevice, b.ReferenceDevice}
			if key[0] == "" || key[1] == "" || seen[key] || math.IsNaN(b.Base) || math.IsInf(b.Base, 0) || b.Base <= 0 {
				return errors.New("invalid range device base")
			}
			seen[key] = true
		}
	}
	return nil
}

// RangeThresholds shares the exact formula and validation with core-py.
// Embedding uses one unit; the head is an independent atom without depth scaling.
func RangeThresholds(p *pb.RangeBandParams, s, e, total uint32, producer, reference string, head bool) (float64, float64, error) {
	if err := ValidateRangeBand(p); err != nil {
		return 0, 0, err
	}
	if total == 0 || s > e || e > total {
		return 0, 0, errors.New("invalid range bounds")
	}
	list := p.Base
	if head {
		list = p.HeadBase
	}
	for _, b := range list {
		if b.ProducerDevice == producer && b.ReferenceDevice == reference {
			soft := b.Base
			if !head {
				length := e - s
				if length == 0 {
					length = 1
				}
				soft *= math.Sqrt(float64(length)) * (1 + p.DepthCoef*float64(e)/float64(total))
			}
			hard := soft * p.HardMultiplier
			if math.IsInf(soft, 0) || math.IsInf(hard, 0) {
				return 0, 0, errors.New("range band overflow")
			}
			return soft, hard, nil
		}
	}
	return 0, 0, errors.New("uncalibrated range device pair")
}
