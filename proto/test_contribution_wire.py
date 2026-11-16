"""Contribution facts must survive serialization and be bound by node signatures."""
import unittest
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from ember_core import sign, verify
from ember_pb2 import ResourceProfile, ContributionSignal


class ContributionWireTest(unittest.TestCase):
    def test_signed_measurement_roundtrip_and_tampering(self):
        key = Ed25519PrivateKey.generate()
        profile = ResourceProfile(chain_id='context-profile', contribution=ContributionSignal(
            unavailable=True, weight_bytes=123, kv_bytes=45, peak_bytes=180,
            budget_bytes=100, context_tokens=512, max_concurrent=1,
            global_concurrent=1, reason='fixture only'))
        sign(profile,key)
        decoded = ResourceProfile.FromString(profile.SerializeToString(deterministic=True))
        verify(decoded)
        self.assertEqual(decoded.contribution, profile.contribution)
        decoded.contribution.unavailable=False
        with self.assertRaises(Exception): verify(decoded)

    def test_legacy_profile_has_no_signal(self):
        p=ResourceProfile.FromString(ResourceProfile(chain_id='legacy').SerializeToString())
        self.assertFalse(p.HasField('contribution'))
