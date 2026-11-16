import pytest
from ember_pb2 import RangeBandParams, RangeDeviceBase
from ember_core.range_band import range_thresholds


def test_range_thresholds():
    p = RangeBandParams(metric='relative-l2-v1', depth_coef=.5, hard_multiplier=3,
        base=[RangeDeviceBase(producer_device='cpu', reference_device='cuda', base=.02)],
        head_base=[RangeDeviceBase(producer_device='cpu', reference_device='cuda', base=.03)])
    for start, end, head, want in [(4, 8, False, .048), (0, 0, False, .02),
                                    (20, 20, True, .03), (0, 20, True, .03)]:
        assert range_thresholds(p, start, end, 20, 'cpu', 'cuda', head) == pytest.approx((want, 3*want))
    with pytest.raises(ValueError):
        range_thresholds(p, 0, 1, 20, 'cpu', 'cpu')
    p.base.add().CopyFrom(p.base[0])
    with pytest.raises(ValueError):
        range_thresholds(p, 0, 1, 20, 'cpu', 'cuda')
