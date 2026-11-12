"""Pinned-TLS gRPC transport for LAN Qwen pipeline stages."""
import argparse
import base64
import hashlib
import json
import os
from concurrent import futures
from pathlib import Path
import threading
import time

import grpc
import torch
from transformers import AutoTokenizer

from ember_core import digest, verify, sign, node_id
from ember_pb2 import (ChainGenesis, ModelManifest, ShardForwardRequest,
                       ShardForwardResponse, ShardStepReceipt, ShardSpec)
from ember_pb2_grpc import (ShardPipelineServicer, ShardPipelineStub,
                            add_ShardPipelineServicer_to_server)
from engine.shard import Qwen2ShardEngine
from fetcher import verify_local
from sharding import stage_span, activation_hash, range_parameters, execution_span
from pinned_tls import pinned_channel

MAX_TENSOR_BYTES = 64 * 1024 * 1024


def tensor_bytes(tensor):
    tensor = tensor.detach().to('cpu', dtype=torch.bfloat16).contiguous()
    return tensor.view(torch.uint16).numpy().tobytes(), list(tensor.shape)


def bytes_tensor(raw, shape, device, hidden_size=None, max_sequence=None):
    if not raw or len(raw) > MAX_TENSOR_BYTES or len(shape) != 3:
        raise ValueError('invalid hidden tensor')
    count = 1
    for dimension in shape:
        if dimension < 1:
            raise ValueError('invalid hidden shape')
        count *= dimension
    if count * 2 != len(raw):
        raise ValueError('hidden tensor size mismatch')
    if shape[0] != 1 or (hidden_size is not None and shape[2] != hidden_size):
        raise ValueError('hidden tensor model shape mismatch')
    if max_sequence is not None and shape[1] > max_sequence:
        raise ValueError('hidden tensor sequence is too long')
    return torch.frombuffer(bytearray(raw), dtype=torch.uint16).view(torch.bfloat16).reshape(tuple(shape)).to(device)


def load_shard_anchor(genesis_path, manifest_path, index, model_path, range_start=None, range_end=None):
    genesis = ChainGenesis.FromString(Path(genesis_path).read_bytes())
    manifest = ModelManifest.FromString(Path(manifest_path).read_bytes())
    verify(genesis)
    verify(manifest)
    if (genesis.chain_id != manifest.chain_id or
            genesis.model_manifest_root != digest(manifest) or
            (not manifest.shards and not manifest.dynamic_ranges) or not manifest.total_layers):
        raise ValueError('invalid shard chain anchor')
    if manifest.dynamic_ranges:
        config=json.loads(Path(model_path,'config.json').read_text())
        if config.get('model_type')!='qwen2' or config.get('num_hidden_layers')!=manifest.total_layers:
            raise ValueError('dynamic layout differs from reference model configuration')
        if manifest.min_stage_layers < 1 or range_start is None or range_end is None or not 0 <= range_start < range_end <= manifest.total_layers:
            raise ValueError('dynamic profile needs a valid capability range')
        for artifact in manifest.artifacts:
            verify_local(artifact, model_path)
        spec = ShardSpec(index=0,start_layer=range_start,end_layer=range_end,
            has_embedding=range_start==0,has_lm_head=range_end==manifest.total_layers,
            unit_kind='range',parameters_hash=range_parameters(digest(manifest),range_start,range_end),
            artifacts=[a.name for a in manifest.artifacts])
        return genesis,manifest,spec
    validate_topology(manifest)
    matches = [spec for spec in manifest.shards if spec.index == index]
    if len(matches) != 1:
        raise ValueError('shard index missing or duplicated')
    spec = matches[0]
    if (spec.end_layer < spec.start_layer or
            spec.end_layer > manifest.total_layers or not spec.parameters_hash):
        raise ValueError('invalid shard specification')
    artifacts = {artifact.name: artifact for artifact in manifest.artifacts}
    for name in spec.artifacts:
        if name not in artifacts:
            raise ValueError('shard artifact missing from manifest')
        verify_local(artifacts[name], model_path)
    config_path = os.path.join(model_path, 'config.json')
    if not os.path.isfile(config_path) or hashlib.sha256(Path(config_path).read_bytes()).digest() != manifest.config_hash:
        raise ValueError('shard config hash mismatch')
    return genesis, manifest, spec


def validate_topology(manifest):
    specs = sorted(manifest.shards, key=lambda item: item.index)
    cursor = 0
    for index, spec in enumerate(specs):
        if spec.unit_kind not in ('', 'embedding', 'layers', 'lm_head'):
            raise ValueError('unsupported stage unit kind')
        if ((spec.unit_kind == 'embedding' and not (spec.start_layer == spec.end_layer == 0 and spec.has_embedding and not spec.has_lm_head)) or (spec.unit_kind == 'lm_head' and not (spec.start_layer == spec.end_layer == manifest.total_layers and spec.has_lm_head and not spec.has_embedding)) or (spec.unit_kind == 'layers' and (spec.end_layer <= spec.start_layer or spec.has_embedding or spec.has_lm_head))):
            raise ValueError('unit kind and boundaries disagree')
        start, end = stage_span(spec.start_layer, spec.end_layer, manifest.total_layers,
                                spec.has_embedding, spec.has_lm_head)
        if (spec.index != index or start != cursor or not spec.artifacts or not spec.parameters_hash):
            raise ValueError('invalid or non-contiguous shard topology')
        cursor = end
    if len(specs) < 2 or cursor != manifest.total_layers + 2:
        raise ValueError('shard topology does not cover the model')
    return specs


def invocation_hash(message):
    return hashlib.sha256(message.SerializeToString(deterministic=True)).digest()


def response_hash(response):
    unsigned = ShardForwardResponse()
    unsigned.CopyFrom(response)
    unsigned.ClearField('receipt')
    return invocation_hash(unsigned)


def sign_stage_response(request, response, key, genesis, manifest, spec):
    receipt = ShardStepReceipt(chain_id=genesis.chain_id,
        manifest_root=digest(manifest),
        execution_profile_ver=genesis.execution_profile_ver,
        request_id=request.request_id, step=request.step,
        shard_index=spec.index, start_layer=spec.start_layer,
        end_layer=spec.end_layer, parameters_hash=spec.parameters_hash, unit_kind=spec.unit_kind,
        input_activation_hash=activation_hash(digest(manifest),spec.start_layer,request.hidden_bf16,request.hidden_shape) if spec.unit_kind and request.hidden_bf16 else b"",
        output_activation_hash=activation_hash(digest(manifest),spec.end_layer,response.hidden_bf16,response.hidden_shape) if spec.unit_kind and response.hidden_bf16 else b"",
        request_hash=invocation_hash(request), response_hash=response_hash(response))
    from cryptography.hazmat.primitives import serialization
    receipt.node_id = node_id(key.public_key().public_bytes(
        serialization.Encoding.Raw, serialization.PublicFormat.Raw))
    sign(receipt, key)
    response.receipt.CopyFrom(receipt)
    return response


def verify_stage_response(request, response, profile):
    # The caller supplies the independently authenticated registered profile.
    verify(profile)
    receipt = response.receipt
    verify(receipt)
    start,end = ((request.executed_start_layer,request.executed_end_layer) if profile.shard_dynamic_range else (profile.shard_start_layer,profile.shard_end_layer))
    if profile.shard_dynamic_range or request.dynamic_range:
        execution_span(request,profile)
    if (profile.node_id != node_id(profile.signer) or
            receipt.signer != profile.signer or receipt.node_id != profile.node_id or
            receipt.chain_id != profile.chain_id or
            receipt.manifest_root != profile.manifest_root or
            receipt.execution_profile_ver != profile.execution_profile_ver or
            receipt.shard_index != profile.shard_index or
            receipt.start_layer != start or
            receipt.end_layer != end or
            receipt.parameters_hash != profile.shard_parameters_hash or
            receipt.unit_kind != profile.shard_unit_kind or
            response.shard_index != profile.shard_index or
            response.shard_parameters_hash != profile.shard_parameters_hash or
            receipt.request_id != request.request_id or receipt.step != request.step or
            receipt.request_hash != invocation_hash(request) or
            receipt.response_hash != response_hash(response)):
        raise ValueError('stage receipt binding mismatch')
    if receipt.unit_kind:
        expected_input = activation_hash(profile.manifest_root,receipt.start_layer,request.hidden_bf16,request.hidden_shape) if request.hidden_bf16 else b''
        expected_output = activation_hash(profile.manifest_root,receipt.end_layer,response.hidden_bf16,response.hidden_shape) if response.hidden_bf16 else b''
        if receipt.input_activation_hash != expected_input or receipt.output_activation_hash != expected_output:
            raise ValueError('activation boundary commitment mismatch')
    return receipt


class StageService(ShardPipelineServicer):
    def __init__(self, engine, authorized_schedulers, spec, key=None, genesis=None,
                 manifest=None, nonce_path=None):
        self.engine = engine
        self.authorized_schedulers = set(authorized_schedulers)
        self.spec = spec
        self.key, self.genesis, self.manifest = key, genesis, manifest
        self.auth_lock = threading.Lock()
        self.nonce_path = Path(nonce_path) if nonce_path else None
        self.auth_nonces = self._load_nonces()

    def _load_nonces(self):
        if self.nonce_path is None or not self.nonce_path.exists():
            return {}
        if self.nonce_path.stat().st_mode & 0o077:
            raise ValueError('stage scheduler nonce state must have mode 0600')
        values = json.loads(self.nonce_path.read_text(encoding='ascii'))
        if not isinstance(values, dict) or any(not isinstance(k, str) or
                not isinstance(v, int) or v < 0 for k, v in values.items()):
            raise ValueError('invalid stage scheduler nonce state')
        return values

    def _save_nonces(self):
        if self.nonce_path is None:
            return
        self.nonce_path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        temporary = self.nonce_path.with_suffix('.tmp')
        fd = os.open(temporary, os.O_CREAT | os.O_TRUNC | os.O_WRONLY, 0o600)
        with os.fdopen(fd, 'w', encoding='ascii') as stream:
            json.dump(self.auth_nonces, stream, sort_keys=True,
                      separators=(',', ':'))
            stream.flush(); os.fsync(stream.fileno())
        os.replace(temporary, self.nonce_path)
        self.nonce_path.chmod(0o600)

    def execution_spec(self, request):
        if not self.manifest or not self.manifest.dynamic_ranges:
            if request.dynamic_range:
                raise ValueError('dynamic request for pinned stage')
            return self.spec
        a,b=request.executed_start_layer,request.executed_end_layer
        m=self.manifest
        if (not request.dynamic_range or not self.spec.start_layer<=a<b<=self.spec.end_layer or b-a<m.min_stage_layers or
                (m.allowed_layer_cuts and (a not in m.allowed_layer_cuts or b not in m.allowed_layer_cuts)) or
                (not m.allowed_layer_cuts and (a % m.min_stage_layers or (b % m.min_stage_layers and b!=m.total_layers)))):
            raise ValueError('execution violates capability or signed boundary rules')
        value=ShardSpec();value.CopyFrom(self.spec);value.start_layer=a;value.end_layer=b
        value.has_embedding=a==0 and self.spec.has_embedding
        value.has_lm_head=b==m.total_layers and self.spec.has_lm_head
        return value

    def signed_response(self, request, **kwargs):
        response = self.response(**kwargs)
        if self.key is not None:
            sign_stage_response(request, response, self.key, self.genesis,
                                self.manifest, self.execution_spec(request))
        return response

    def response(self, **kwargs):
        return ShardForwardResponse(shard_index=self.spec.index,
            shard_parameters_hash=self.spec.parameters_hash, **kwargs)

    def Forward(self, request, context):
        started = time.perf_counter()
        try:
            verify(request)
            scheduler = node_id(request.signer)
            with self.auth_lock:
                previous = self.auth_nonces.get(scheduler, 0)
                if (scheduler not in self.authorized_schedulers or
                        request.nonce <= previous):
                    raise PermissionError('unauthorized or replayed scheduler request')
                self.auth_nonces[scheduler] = request.nonce
                self._save_nonces()
            if not request.request_id or len(request.request_id) > 128:
                raise ValueError('invalid request id')
            spec = self.execution_spec(request)
            delay = float(os.environ.get('EMBER_STAGE_TEST_DELAY_MS','0')) / 1000
            if delay > 0:
                time.sleep(delay)
            if spec.has_embedding:
                if not request.input_ids or request.hidden_bf16:
                    raise ValueError('first stage requires token ids')
                if len(request.input_ids) > self.engine.config.max_position_embeddings:
                    raise ValueError('input sequence is too long')
                ids = torch.tensor([request.input_ids], dtype=torch.long,
                                   device=self.engine.device)
                output = self.engine.forward(input_ids=ids,start_layer=spec.start_layer,end_layer=spec.end_layer,has_embedding=spec.has_embedding,has_lm_head=spec.has_lm_head)
            else:
                if request.input_ids or not request.hidden_bf16:
                    raise ValueError('later stage requires hidden tensor')
                hidden = bytes_tensor(request.hidden_bf16, request.hidden_shape,
                                      self.engine.device, self.engine.config.hidden_size,
                                      self.engine.config.max_position_embeddings)
                output = self.engine.forward(hidden_states=hidden,start_layer=spec.start_layer,end_layer=spec.end_layer,has_embedding=spec.has_embedding,has_lm_head=spec.has_lm_head)
            if spec.has_lm_head:
                top = torch.topk(output[0, -1], 16)
                response = self.signed_response(request, token_id=int(top.indices[0]),
                    topk_ids=[int(x) for x in top.indices.tolist()],
                    topk_logits=[float(x) for x in top.values.tolist()],
                    activations=self.engine.last_activations,
                    server_elapsed_us=int((time.perf_counter() - started) * 1_000_000))
                print(f'SHARD FORWARD request={request.request_id} step={request.step} '
                      f'index={self.spec.index} layers={spec.start_layer}:{spec.end_layer}', flush=True)
                return response
            raw, shape = tensor_bytes(output)
            response = self.signed_response(request, hidden_bf16=raw, hidden_shape=shape,
                                            activations=self.engine.last_activations,
                                            server_elapsed_us=int((time.perf_counter() - started) * 1_000_000))
            print(f'SHARD FORWARD request={request.request_id} step={request.step} '
                  f'index={self.spec.index} layers={spec.start_layer}:{spec.end_layer}', flush=True)
            return response
        except PermissionError as exc:
            context.abort(grpc.StatusCode.UNAUTHENTICATED, str(exc))
        except (ValueError, RuntimeError) as exc:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, str(exc))


def read_scheduler_key(path):
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
    path = Path(path)
    if path.stat().st_mode & 0o077:
        raise ValueError('scheduler key must have mode 0600')
    raw = path.read_bytes()
    if len(raw) == 64:
        raw = raw[:32]
    if len(raw) != 32:
        raise ValueError('scheduler key must be a raw Ed25519 seed or private key')
    return Ed25519PrivateKey.from_private_bytes(raw)


class StageClient:
    def __init__(self, target, scheduler_key, spec=None, profile=None, executed_range=None):
        address, fingerprint = target.rsplit('@', 1)
        options = (('grpc.max_send_message_length', MAX_TENSOR_BYTES),
                   ('grpc.max_receive_message_length', MAX_TENSOR_BYTES))
        self.address,self.fingerprint,self.options = address,fingerprint,options
        self.channel = None
        self.stub = None
        self.scheduler_key = scheduler_key
        self.nonce_lock = threading.Lock()
        self.last_nonce = 0
        self.spec = spec
        self.profile = profile
        self.executed_range = executed_range

    def forward(self, request_id, step, input_ids=None, hidden=None, timeout=120):
        if input_ids is not None:
            request = ShardForwardRequest(request_id=request_id, step=step,
                                          input_ids=input_ids)
        else:
            raw, shape = tensor_bytes(hidden)
            request = ShardForwardRequest(request_id=request_id, step=step,
                                          hidden_bf16=raw, hidden_shape=shape)
        if self.profile is not None and self.profile.shard_dynamic_range:
            if self.executed_range is None:
                raise ValueError('dynamic client needs an executed range')
            request.dynamic_range=True
            request.executed_start_layer,request.executed_end_layer=self.executed_range
        with self.nonce_lock:
            request.nonce = max(time.time_ns(), self.last_nonce + 1)
            self.last_nonce = request.nonce
        sign(request, self.scheduler_key)
        if self.stub is None:
            self.channel = pinned_channel(self.address,self.fingerprint,self.options)
            self.stub = ShardPipelineStub(self.channel)
        response = self.stub.Forward(request, timeout=timeout)
        self.last_request = request
        if self.spec is not None and (response.shard_index != self.spec.index or
                response.shard_parameters_hash != self.spec.parameters_hash):
            raise ValueError('shard identity or order mismatch')
        if self.profile is not None:
            verify_stage_response(request, response, self.profile)
        return response

    def close(self):
        if self.channel is not None:
            self.channel.close()


from engine.spi import EngineAdapter, EngineCapabilities


class PipelineEngine(EngineAdapter):
    device = 'pipeline'

    def __init__(self, model_path, targets, scheduler_key_file, timeout=120,
                 manifest=None):
        if len(targets) < 2:
            raise ValueError('pipeline requires at least two stages')
        self.tokenizer = AutoTokenizer.from_pretrained(
            model_path, local_files_only=True, trust_remote_code=False)
        scheduler_key = read_scheduler_key(scheduler_key_file)
        specs = validate_topology(manifest) if manifest else [None] * len(targets)
        if len(specs) != len(targets):
            raise ValueError('stage count does not match signed topology')
        self.stages = [StageClient(target, scheduler_key, spec)
                       for target, spec in zip(targets, specs)]
        self.timeout = timeout
        self.manifest = manifest

    def count_input_tokens(self, prompt):
        return len(self.tokenizer(prompt).input_ids)

    def _step(self, request_id, step, ids):
        response = self.stages[0].forward(request_id, step, input_ids=ids,
                                          timeout=self.timeout)
        activations = dict(response.activations)
        for index, stage in enumerate(self.stages[1:], 1):
            raw = response.hidden_bf16
            hidden = bytes_tensor(raw, response.hidden_shape, 'cpu')
            response = stage.forward(request_id, step, hidden=hidden,
                                     timeout=self.timeout)
            activations.update(response.activations)
        topk = list(zip(response.topk_ids, response.topk_logits))
        if not topk or response.token_id != topk[0][0]:
            raise ValueError('invalid terminal shard response')
        return response.token_id, topk, activations

    def _generate(self, prompt, seed, count):
        return self.generate_for_request(prompt, seed, count, os.urandom(16).hex())

    def generate_for_request(self, prompt, seed, count, request_id):
        from engine import Step
        ids = self.tokenizer(prompt, return_tensors='pt').input_ids[0].tolist()
        for index in range(count):
            token, topk, activations = self._step(request_id, index, ids)
            text = '' if token == self.tokenizer.eos_token_id else self.tokenizer.decode([token])
            yield Step(token, text, topk, activations)
            ids.append(token)
            if token == self.tokenizer.eos_token_id:
                break

    def replay(self, prompt, seed, tokens):
        from engine import Step
        request_id = os.urandom(16).hex()
        ids = self.tokenizer(prompt, return_tensors='pt').input_ids[0].tolist()
        result = []
        for index, committed in enumerate(tokens):
            _token, topk, activations = self._step(request_id, index, ids)
            result.append(Step(committed, self.tokenizer.decode([committed]), topk, activations))
            ids.append(committed)
        return result

    def capabilities(self):
        return EngineCapabilities('pipeline', __import__('transformers').__version__,
                                  'pipeline', 'bfloat16', ('full',), 'raw-logits',
                                  16, True)

    def close(self):
        super().close()
        for stage in self.stages:
            stage.close()

    def resource_metadata(self):
        return {'requested_device': 'pipeline', 'actual_device': 'pipeline',
                'device_reason': 'LAN pipeline stages',
                'torch_version': torch.__version__,
                'transformers_version': __import__('transformers').__version__,
                'available_memory_bytes': 1,
                'shard_parameters_hash': digest(self.manifest) if self.manifest else b''}


def certificate_fingerprint(path):
    from cryptography import x509
    from cryptography.hazmat.primitives import serialization
    cert = x509.load_pem_x509_certificate(Path(path).read_bytes())
    return hashlib.sha256(cert.public_bytes(serialization.Encoding.DER)).digest()


def register_stage(args, key, ident, genesis, manifest, spec, engine, fingerprint):
    from agent import Agent
    from agent.join import register_on_chain
    from ember_core import join_challenge
    from ember_pb2 import ResourceProfile, TunnelFrame
    from engine import MockEngine
    from engine.spi import EngineCapabilities

    agent = Agent(ident, MockEngine(), key=key)
    genesis_raw = Path(args.genesis).read_bytes()
    manifest_raw = Path(args.manifest).read_bytes()

    def build_profile(observed):
        runtime = engine.resource_metadata()
        caps = EngineCapabilities('transformers' if genesis.execution_profile_ver.startswith('transformers-qwen2.5-pipeline-gpu') else 'transformers-shard', runtime['transformers_version'],
            engine.device, 'bfloat16', ('shard-layer',), 'raw-logits', 16, False,
            native_incremental=False, roles=('shard-stage',))
        value = ResourceProfile(node_id=ident, chain_id=genesis.chain_id,
            gpu=engine.device,
            memory_bytes=os.sysconf('SC_PHYS_PAGES') * os.sysconf('SC_PAGE_SIZE'),
            engine_ver='transformers-shard-v1', benchmark_result_ref=observed,
            expires_unix=int(time.time()) + 120, manifest_root=genesis.model_manifest_root,
            execution_profile_ver=genesis.execution_profile_ver,
            requested_device=runtime['requested_device'], actual_device=runtime['actual_device'],
            device_reason=runtime['device_reason'], torch_version=runtime['torch_version'],
            transformers_version=runtime['transformers_version'],
            cuda_version=runtime['cuda_version'], accelerator_name=runtime['accelerator_name'],
            available_memory_bytes=runtime['available_memory_bytes'], shard_index=spec.index,
            shard_start_layer=spec.start_layer, shard_end_layer=spec.end_layer,
            shard_total_layers=manifest.total_layers, shard_has_embedding=spec.has_embedding,
            shard_unit_kind=spec.unit_kind, shard_dynamic_range=manifest.dynamic_ranges,
            shard_has_lm_head=spec.has_lm_head, shard_parameters_hash=spec.parameters_hash,
            shard_endpoint=args.advertise, shard_tls_fingerprint=fingerprint,
            engine_capabilities=caps.to_proto())
        sign(value, key)
        return value

    def authenticate(frame):
        data = json.loads(frame.payload)
        if (base64.b64decode(data['genesis']) != genesis_raw or
                base64.b64decode(data['manifest']) != manifest_raw):
            raise ValueError('coordinator shard anchor mismatch')
        answers = data['questions']
        observed = hashlib.sha256(b''.join(a.encode() + b'\x00' for a in answers)).digest()
        agent.profile = lambda: build_profile(observed)
        signed_profile = build_profile(observed)
        if getattr(args, 'chain_rpc', None):
            register_on_chain(args, Path(args.state_dir), ident, signed_profile)
        proof = {'profile': base64.b64encode(signed_profile.SerializeToString(
                    deterministic=True)).decode(),
                 'signature': base64.b64encode(key.sign(join_challenge(
                    base64.b64decode(data['nonce'])))).decode(),
                 'answers': answers}
        return TunnelFrame(node_id=ident, kind='authenticate',
                           payload=json.dumps(proof).encode())

    agent.serve(args.coordinator,
        channel_factory=lambda target: pinned_channel(target, args.coordinator_tls_fingerprint),
        authenticate=authenticate)


def serve(args):
    genesis, manifest, spec = load_shard_anchor(
        args.genesis, args.manifest, args.shard_index, args.model_path, args.range_start, args.range_end)
    if args.torch_threads:
        torch.set_num_threads(args.torch_threads)
    key = None
    if args.state_dir:
        from agent.join import identity
        key, stage_id = identity(args.state_dir)
        print(f'SHARD IDENTITY node_id={stage_id}', flush=True)
    if not args.authorized_scheduler:
        raise ValueError('at least one on-chain scheduler identity is required')
    engine = Qwen2ShardEngine(args.model_path, spec.start_layer, spec.end_layer,
                              spec.has_embedding, spec.has_lm_head, args.device)
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=1), options=(
        ('grpc.max_receive_message_length', MAX_TENSOR_BYTES),
        ('grpc.max_send_message_length', MAX_TENSOR_BYTES)))
    add_ShardPipelineServicer_to_server(
        StageService(engine, args.authorized_scheduler, spec, key, genesis, manifest,
                     Path(args.state_dir) / 'scheduler-nonces.json'
                     if args.state_dir else None), server)
    credentials = grpc.ssl_server_credentials(((Path(args.key).read_bytes(),
                                                 Path(args.cert).read_bytes()),))
    if server.add_secure_port(args.listen, credentials) == 0:
        raise RuntimeError('could not bind shard listener')
    server.start()
    print(f'SHARD READY index={spec.index} layers={spec.start_layer}:{spec.end_layer} device={engine.device}', flush=True)
    if args.coordinator:
        if not key or not args.coordinator_tls_fingerprint or not args.advertise:
            raise ValueError('registered shard requires state dir, coordinator pin and advertised endpoint')
        thread = threading.Thread(target=register_stage,
            args=(args, key, stage_id, genesis, manifest, spec, engine,
                  certificate_fingerprint(args.cert)), daemon=True)
        thread.start()
    server.wait_for_termination()


def run(args):
    genesis = ChainGenesis.FromString(Path(args.genesis).read_bytes())
    manifest = ModelManifest.FromString(Path(args.manifest).read_bytes())
    verify(genesis); verify(manifest)
    if genesis.model_manifest_root != digest(manifest):
        raise ValueError('invalid pipeline chain anchor')
    engine = PipelineEngine(args.model_path, args.stage, args.scheduler_key,
                            args.timeout, manifest)
    try:
        text = []
        for step in engine.generate_for_request(
                args.prompt, 0, args.max_tokens, args.request_id):
            text.append(step.text)
            print(step.text, end='', flush=True)
        print()
        return ''.join(text)
    finally:
        for stage in engine.stages:
            stage.close()


def main():
    parser = argparse.ArgumentParser(prog='ember-node shard')
    sub = parser.add_subparsers(dest='command', required=True)
    worker = sub.add_parser('serve')
    worker.add_argument('--listen', required=True)
    worker.add_argument('--state-dir', help='private stage identity directory; enables signed invocation receipts')
    worker.add_argument('--chain-rpc')
    worker.add_argument('--chain-cli', default='emberchain')
    worker.add_argument('--ledger-chain-id')
    worker.add_argument('--registration-ttl', type=int, default=1000)
    worker.add_argument('--coordinator', help='outbound registration tunnel address')
    worker.add_argument('--coordinator-tls-fingerprint')
    worker.add_argument('--advertise', help='coordinator-reachable host:port for this shard')
    worker.add_argument('--cert', required=True)
    worker.add_argument('--key', required=True)
    worker.add_argument('--authorized-scheduler', action='append', required=True)
    worker.add_argument('--model-path', required=True)
    worker.add_argument('--genesis', required=True)
    worker.add_argument('--manifest', required=True)
    worker.add_argument('--range-start', type=int)
    worker.add_argument('--range-end', type=int)
    worker.add_argument('--shard-index', type=int, required=True)
    worker.add_argument('--device', choices=('auto', 'cpu', 'cuda'), default='auto')
    worker.add_argument('--torch-threads', type=int, default=0)
    client = sub.add_parser('run')
    client.add_argument('--stage', action='append', required=True,
                        help='host:port@sha256-certificate-fingerprint, in layer order')
    client.add_argument('--scheduler-key', required=True)
    client.add_argument('--model-path', required=True)
    client.add_argument('--genesis', required=True)
    client.add_argument('--manifest', required=True)
    client.add_argument('--prompt', required=True)
    client.add_argument('--request-id', default=lambda: os.urandom(16).hex())
    client.add_argument('--max-tokens', type=int, default=8)
    client.add_argument('--timeout', type=float, default=120)
    args = parser.parse_args()
    if callable(getattr(args, 'request_id', None)):
        args.request_id = args.request_id()
    serve(args) if args.command == 'serve' else run(args)


if __name__ == '__main__':
    main()
