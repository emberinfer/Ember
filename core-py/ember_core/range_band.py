"""Signed pipeline-range-v2 coefficients, identical to core-go/range_band.go."""
import math

BAND = 'pipeline-range-v2'


def validate_range_band(params):
    if (params.metric != 'relative-l2-v1' or not math.isfinite(params.depth_coef)
            or params.depth_coef < 0 or not math.isfinite(params.hard_multiplier)
            or params.hard_multiplier <= 1 or not params.base):
        raise ValueError('invalid range band parameters')
    for entries in (params.base, params.head_base):
        seen = set()
        for entry in entries:
            pair = entry.producer_device, entry.reference_device
            if (not all(pair) or pair in seen or not math.isfinite(entry.base)
                    or entry.base <= 0):
                raise ValueError('invalid range device base')
            seen.add(pair)


def range_thresholds(params, start, end, total, producer, reference, head=False):
    validate_range_band(params)
    if not 0 <= start <= end <= total or total < 1:
        raise ValueError('invalid range bounds')
    for entry in (params.head_base if head else params.base):
        if (entry.producer_device, entry.reference_device) == (producer, reference):
            soft = entry.base
            if not head:
                soft *= math.sqrt(max(1, end-start)) * (1+params.depth_coef*end/total)
            hard = soft * params.hard_multiplier
            if not math.isfinite(soft) or not math.isfinite(hard):
                raise ValueError('range band overflow')
            return soft, hard
    raise ValueError('uncalibrated range device pair')
