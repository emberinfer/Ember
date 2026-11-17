"""MOE-8 staging drill (in-process fleet, real gRPC + pinned TLS):

  assembler -> attention stage A [0,1) -> expert nodes (layer 0)
            -> attention stage B [1,2) -> expert nodes (layer 1)

on a tiny random-weight OLMoE. Verifies: token-identical output vs the
whole-model baseline, segment auditor acceptance of the honest run,
attribution of a tampered expert (weights perturbed, committed params
hash still claimed), objective grading on corrupted evidence, and exports
the execution artifacts (manifest/genesis/receipts/profiles/audits) that
the Go chain ingest test settles on a fresh state (MOE_DRILL_DIR).
"""
import json
import os
import socket
import sys
import tempfile
import threading
import unittest
from concurrent import futures
from pathlib import Path

sys.path[:0] = [str(Path(__file__).resolve().parents[1] / 'proto'),
                str(Path(__file__).resolve().parents[1] / 'core-py'),
                str(Path(__file__).resolve().parent),
                str(Path(__file__).resolve().parents[1] / 'chain-genesis')]

import grpc
import torch
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.asymmetric import rsa
from cryptography.x509.oid import NameOID
import datetime

from ember_core import digest, node_id, sign
from ember_pb2 import (EngineCapabilities, MoeUnitRegistration, ResourceProfile)
from ember_pb2_grpc import add_ShardPipelineServicer_to_server
from engine.moe import AttentionStage, ExpertUnit, MoeWeights
from moe_client import MoeAssembler
from moe_rpc import (MoeAttentionClient, MoeAttentionService, MoeExpertClient,
                     MoeExpertService, ReceiptSigner)
from auditor.moe import MoeAuditor
import build_moe
from test_moe_engine import tiny_model_dir

ARTIFACTS = Path(os.environ.get('MOE_DRILL_DIR', '/tmp/moe-drill-artifacts'))


def make_cert(directory, name):
    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    subject = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, name)])
    import ipaddress
    cert = (x509.CertificateBuilder().subject_name(subject).issuer_name(subject)
            .public_key(key.public_key()).serial_number(x509.random_serial_number())
            .not_valid_before(datetime.datetime.now(datetime.timezone.utc))
            .not_valid_after(datetime.datetime.now(datetime.timezone.utc) +
                             datetime.timedelta(days=30))
            .add_extension(x509.SubjectAlternativeName([
                x509.IPAddress(ipaddress.IPv4Address('127.0.0.1')),
                x509.DNSName('localhost')]), critical=False)
            .sign(key, hashes.SHA256()))
    cert_path = directory / f'{name}.crt'
    key_path = directory / f'{name}.key'
    cert_path.write_bytes(cert.public_bytes(serialization.Encoding.PEM))
    key_path.write_bytes(key.private_bytes(
        serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8,
        serialization.NoEncryption()))
    import hashlib
    fingerprint = hashlib.sha256(cert.public_bytes(serialization.Encoding.DER)).hexdigest()
    return cert_path, key_path, fingerprint


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def serve(service, cert_path, key_path):
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=4), options=(
        ('grpc.max_receive_message_length', 64 * 1024 * 1024),
        ('grpc.max_send_message_length', 64 * 1024 * 1024)))
    add_ShardPipelineServicer_to_server(service, server)
    credentials = grpc.ssl_server_credentials(
        ((Path(key_path).read_bytes(), Path(cert_path).read_bytes()),))
    port = free_port()
    if server.add_secure_port(f'127.0.0.1:{port}', credentials) == 0:
        raise RuntimeError('bind failed')
    server.start()
    return server, port


def pub(key):
    return key.public_key().public_bytes(serialization.Encoding.Raw,
                                         serialization.PublicFormat.Raw)


class SimpleTokenizer:
    eos_token_id = -1  # never emitted; the baseline generate uses the same rule

    def __call__(self, text):
        ids = [(ord(c) % 100) + 3 for c in text]

        class _Out:
            pass
        out = _Out()
        out.input_ids = ids
        return out


class MoeDrillTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = Path(tempfile.mkdtemp(prefix='moe-drill-'))
        cls.model_dir, cls.cfg = tiny_model_dir()
        out = cls.tmp / 'profile'
        cls.facts = build_moe.build(cls.model_dir, out, 'ember-olmoe-tiny-moe-v1',
                                    [(0, 1), (1, 2)], price_output=0,
                                    key=Ed25519PrivateKey.generate())
        from ember_pb2 import ChainGenesis, ModelManifest
        cls.manifest = ModelManifest.FromString((out / 'manifest.pb').read_bytes())
        cls.genesis = ChainGenesis.FromString((out / 'genesis.pb').read_bytes())
        cls.weights = MoeWeights(cls.model_dir)
        cls.layout = cls.manifest.moe_layout
        cls.keys = {name: Ed25519PrivateKey.generate() for name in
                    ('assembler', 'stageA', 'stageB', 'expB', 'expC', 'expB2', 'expC2')}
        cls.ids = {name: node_id(pub(key)) for name, key in cls.keys.items()}
        cls.profiles = {}
        cls._build_profiles()
        cls.servers = []
        cls.targets = {}
        cls._start_fleet(tamper=False)
        ARTIFACTS.mkdir(parents=True, exist_ok=True)
        (ARTIFACTS / 'manifest.pb').write_bytes((out / 'manifest.pb').read_bytes())
        (ARTIFACTS / 'genesis.pb').write_bytes((out / 'genesis.pb').read_bytes())
        profiles = {p.node_id: p.SerializeToString(deterministic=True).hex()
                    for p in cls.profiles.values()}
        (ARTIFACTS / 'profiles.json').write_text(json.dumps(profiles))
        # Drill-only test keys so the Go chain ingest test can sign chain
        # actions for these nodes. Never production material.
        seeds = {cls.ids[name]: key.private_bytes(
            serialization.Encoding.Raw, serialization.PrivateFormat.Raw,
            serialization.NoEncryption()).hex()
            for name, key in cls.keys.items()}
        (ARTIFACTS / 'keys.json').write_text(json.dumps(seeds))

    @classmethod
    def tearDownClass(cls):
        for server in cls.servers:
            server.stop(0)

    @classmethod
    def _build_profiles(cls):
        root = digest(cls.manifest)
        facts = cls.facts
        def attn_profile(name, stage_idx):
            stage = facts['stages'][stage_idx]
            profile = ResourceProfile(
                node_id=cls.ids[name], chain_id=cls.manifest.chain_id,
                manifest_root=root,
                execution_profile_ver=cls.genesis.execution_profile_ver,
                requested_device='cpu', actual_device='cpu',
                shard_index=stage_idx, shard_start_layer=stage['start_layer'],
                shard_end_layer=stage['end_layer'], shard_total_layers=2,
                shard_has_embedding=stage['start_layer'] == 0,
                shard_has_lm_head=stage['end_layer'] == 2,
                shard_parameters_hash=bytes.fromhex(stage['params_hash']),
                shard_unit_kind='attention', moe_unit_kind='attention',
                engine_capabilities=EngineCapabilities(
                    engine_name='olmoe-moe', engine_version='v1',
                    numerical_format='bfloat16', score_kind='raw-logits',
                    max_top_k=16, forced_prefix_replay=True,
                    evidence_levels=['full'], roles=['moe-attention']))
            sign(profile, cls.keys[name])
            cls.profiles[name] = profile
        def expert_profile(name, units):
            profile = ResourceProfile(
                node_id=cls.ids[name], chain_id=cls.manifest.chain_id,
                manifest_root=root,
                execution_profile_ver=cls.genesis.execution_profile_ver,
                requested_device='cpu', actual_device='cpu',
                moe_unit_kind='expert',
                engine_capabilities=EngineCapabilities(
                    engine_name='olmoe-moe', engine_version='v1',
                    numerical_format='bfloat16', score_kind='raw-logits',
                    max_top_k=16, forced_prefix_replay=True,
                    evidence_levels=['full'], roles=['moe-expert']))
            for layer, expert in units:
                idx = layer * 4 + expert
                profile.moe_units.append(MoeUnitRegistration(
                    layer=layer, expert=expert,
                    params_hash=bytes.fromhex(facts['expert_params_hashes'][idx]),
                    merkle_proof=[bytes.fromhex(p) for p in
                                  facts['expert_proofs'][f'{layer}:{expert}']],
                    leaf_index=idx))
            sign(profile, cls.keys[name])
            cls.profiles[name] = profile
        attn_profile('stageA', 0)
        attn_profile('stageB', 1)
        expert_profile('expB', [(0, 0), (0, 1), (1, 0), (1, 1)])
        expert_profile('expC', [(0, 2), (0, 3), (1, 2), (1, 3)])
        expert_profile('expB2', [(0, 0), (0, 1), (1, 0), (1, 1)])
        expert_profile('expC2', [(0, 2), (0, 3), (1, 2), (1, 3)])

    @classmethod
    def _start_fleet(cls, tamper):
        for server in cls.servers:
            server.stop(0)
        cls.servers = []
        cert_dir = cls.tmp / 'certs'
        cert_dir.mkdir(exist_ok=True)

        def expert_service(name, units, lie):
            expert_units = {}
            for layer, expert in units:
                unit = ExpertUnit(cls.weights, layer, expert)
                if lie:
                    honest = unit.params_hash
                    with torch.no_grad():
                        unit.gate = unit.gate + 0.75
                        unit.up = unit.up + 0.5
                    unit.params_hash = honest  # claim the committed hash
                expert_units[(layer, expert)] = unit
            signer = ReceiptSigner(cls.keys[name], cls.genesis, cls.manifest)
            service = MoeExpertService(expert_units, signer,
                                       [cls.ids['stageA'], cls.ids['stageB'],
                                        cls.ids['assembler']])
            cert, key, fp = make_cert(cert_dir, name)
            server, port = serve(service, cert, key)
            cls.servers.append(server)
            cls.targets[name] = f'127.0.0.1:{port}@{fp}'

        expert_service('expB', [(0, 0), (0, 1), (1, 0), (1, 1)], tamper)
        expert_service('expC', [(0, 2), (0, 3), (1, 2), (1, 3)], tamper)

        def attention_service(name, stage_idx, start, end, layer):
            signer = ReceiptSigner(cls.keys[name], cls.genesis, cls.manifest)
            stage = AttentionStage(cls.weights, cls.layout, start, end)
            clients = {}
            for expert in range(4):
                holder = 'expB' if expert < 2 else 'expC'
                clients[(layer, expert)] = MoeExpertClient(
                    cls.targets[holder], cls.keys[name],
                    profile=cls.profiles[holder])
            service = MoeAttentionService(stage, signer, clients,
                                          [cls.ids['assembler']])
            cert, key, fp = make_cert(cert_dir, name)
            server, port = serve(service, cert, key)
            cls.servers.append(server)
            cls.targets[name] = f'127.0.0.1:{port}@{fp}'

        attention_service('stageA', 0, 0, 1, 0)
        attention_service('stageB', 1, 1, 2, 1)

    def assembler(self):
        stages = []
        for name in ('stageA', 'stageB'):
            client = MoeAttentionClient(self.targets[name], self.keys['assembler'],
                                        profile=self.profiles[name])
            stages.append((client, self.profiles[name]))
        expert_clients = {}
        for name in ('expB', 'expC'):
            expert_clients[self.ids[name]] = MoeExpertClient(
                self.targets[name], self.keys['assembler'],
                profile=self.profiles[name])
        return MoeAssembler(stages, SimpleTokenizer(), self.manifest, self.genesis,
                            expert_node_clients=expert_clients)

    def run_generation(self, request_id, evidence_name):
        assembler = self.assembler()
        receipt, evidence = assembler.generate(request_id, 'hello moe world',
                                               max_tokens=5)
        directory = ARTIFACTS / evidence_name
        assembler.save_evidence(directory, receipt, evidence, 'hello moe world',
                                [(ord(c) % 100) + 3 for c in 'hello moe world'])
        return receipt, directory

    def auditor(self):
        return MoeAuditor(self.model_dir, self.manifest, self.genesis)

    def audit_job(self, receipt, directory, drawn):
        index = json.loads((directory / 'index.json').read_text())
        return {'receipt': receipt.SerializeToString(deterministic=True).hex(),
                'profiles': self.profiles, 'evidence_dir': str(directory),
                'drawn': drawn, 'input_ids': index['input_ids'],
                'tokens': index['tokens'],
                'stage_index': {(0, 1): 0, (1, 2): 1}}

    def test_1_honest_generation_matches_baseline(self):
        receipt, directory = self.run_generation('drill-honest', 'evidence-honest')
        (ARTIFACTS / 'receipt-honest.pb').write_bytes(
            receipt.SerializeToString(deterministic=True))
        from transformers import OlmoeForCausalLM
        model = OlmoeForCausalLM.from_pretrained(self.model_dir,
                                                 torch_dtype=torch.bfloat16)
        ids = [(ord(c) % 100) + 3 for c in 'hello moe world']
        with torch.no_grad():
            ref = model.generate(torch.tensor([ids]), max_new_tokens=5,
                                 do_sample=False)
        index = json.loads((directory / 'index.json').read_text())
        self.assertEqual(index['tokens'], ref[0, len(ids):].tolist(),
                         'MoE assembly output differs from whole-model baseline')
        self.assertEqual(receipt.output_tokens, len(index['tokens']))
        # Audit the honest run: draw every frame-0 segment + two later ones.
        drawn = []
        for seg in receipt.moe.segment_receipts:
            if seg.frame == 0:
                from ember_pb2 import SegmentRef
                drawn.append(SegmentRef(kind=seg.kind, layer=seg.layer,
                                        expert=seg.expert, frame=seg.frame,
                                        end_layer=seg.end_layer))
        result = self.auditor().audit(self.audit_job(receipt, directory, drawn))
        (ARTIFACTS / 'audit-honest.json').write_text(json.dumps({
            'verdict': result['verdict'],
            'distances': [{'kind': d.ref.kind, 'frame': d.ref.frame,
                           'layer': d.ref.layer, 'expert': d.ref.expert,
                           'end': d.ref.end_layer, 'distance': d.distance,
                           'band_key': d.band_key} for d in result['distances']],
            'fault_node_ids': result['fault_node_ids']}))
        self.assertEqual(result['verdict'], 'accept',
                         f"honest run rejected: {result['distances']}")
        for d in result['distances']:
            self.assertLess(d.distance, 0.006, f'segment distance {d}')

    def test_2_tampered_expert_is_attributed(self):
        self._start_fleet(tamper=True)
        receipt, directory = self.run_generation('drill-cheat', 'evidence-cheat')
        (ARTIFACTS / 'receipt-cheat.pb').write_bytes(
            receipt.SerializeToString(deterministic=True))
        from ember_pb2 import SegmentRef
        drawn = [SegmentRef(kind=seg.kind, layer=seg.layer, expert=seg.expert,
                            frame=seg.frame, end_layer=seg.end_layer)
                 for seg in receipt.moe.segment_receipts if seg.frame == 0]
        result = self.auditor().audit(self.audit_job(receipt, directory, drawn))
        (ARTIFACTS / 'audit-cheat.json').write_text(json.dumps({
            'verdict': result['verdict'],
            'distances': [{'kind': d.ref.kind, 'frame': d.ref.frame,
                           'layer': d.ref.layer, 'expert': d.ref.expert,
                           'end': d.ref.end_layer, 'distance': d.distance,
                           'band_key': d.band_key} for d in result['distances']],
            'fault_node_ids': result['fault_node_ids'],
            'offense': ({'class': getattr(result['offense'], 'class'),
                         'band': result['offense'].band} if result['offense'] else None)}))
        self.assertEqual(result['verdict'], 'reject')
        self.assertIn(self.ids['expB'], result['fault_node_ids'])
        self.assertIn(self.ids['expC'], result['fault_node_ids'])
        expert_distances = [d.distance for d in result['distances']
                            if d.ref.kind == 'expert']
        self.assertTrue(any(d > 0.008 for d in expert_distances),
                        'tampered expert segments not detected')
        self._start_fleet(tamper=False)

    def test_3_corrupt_evidence_is_objective(self):
        receipt, directory = self.run_generation('drill-honest2', 'evidence-corrupt')
        # Flip bytes in one expert output evidence file.
        index = json.loads((directory / 'index.json').read_text())
        key = sorted(index['expout'])[0]
        path = directory / index['expout'][key]
        raw = bytearray(path.read_bytes())
        raw[10] ^= 0xFF
        raw[11] ^= 0xFF
        path.write_bytes(bytes(raw))
        frame, layer, expert = (int(p) for p in key.split(','))
        from ember_pb2 import SegmentRef
        drawn = [SegmentRef(kind='expert', layer=layer, expert=expert, frame=frame,
                            end_layer=layer + 1)]
        result = self.auditor().audit(self.audit_job(receipt, directory, drawn))
        self.assertEqual(result['verdict'], 'reject')
        self.assertIsNotNone(result['offense'])
        self.assertEqual(getattr(result['offense'], 'class'), 'objective')


if __name__ == '__main__':
    unittest.main()
