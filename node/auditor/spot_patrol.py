"""Independent finalized-assignment patrol; lack of evidence is never evidence."""
import base64
import json
import logging
from pathlib import Path
import threading
from urllib.request import Request, urlopen

from .spot import DirectorySpool, durable_json
from .spot_worker import envelope_result, unopened_action

LOG = logging.getLogger(__name__)


def broadcast(agent, payload):
    """Commit the existing signed envelope directly, without gateway forwarding."""
    envelope = json.loads(payload)['envelope']
    body = json.dumps(dict(jsonrpc='2.0', id=1, method='broadcast_tx_commit',
                           params=dict(tx=envelope))).encode()
    request = Request(agent.chain_args.chain_rpc, data=body,
                      headers={'Content-Type': 'application/json'})
    with urlopen(request, timeout=60) as response:
        result = json.load(response)
    if 'error' in result:
        raise ValueError('spot broadcast RPC error')
    result = result['result']
    execution = result.get('tx_result', result.get('deliver_tx'))
    if result['check_tx'].get('code', 0) or execution is None or execution.get('code', 0):
        raise ValueError('spot transaction rejected')


def submit_received(agent, request_id):
    """Called after verified delivery is durable, before expensive replay."""
    if not getattr(agent.chain_args, 'chain_rpc', None):
        return  # Offline replay harnesses have no transaction transport.
    with agent.spot_tx_lock:
        broadcast(agent, envelope_result(agent, dict(type='spot_received', hold_id=request_id), {}))


class Patrol:
    def __init__(self, agent, query=None, submit=None):
        self.agent = agent
        if query is None:
            from agent.join import _chain_json
            query = lambda path: _chain_json(agent.chain_args.chain_cli, agent.chain_args.chain_rpc, path)
        self.query = query
        self.submit = submit or (lambda payload: broadcast(agent, payload))
        self.path = Path(agent.chain_args.state_dir) / 'spot-patrol.json'
        self.done = json.loads(self.path.read_text()) if self.path.exists() else []

    def poll(self):
        args = self.agent.chain_args
        if getattr(args, 'no_audit', False):
            return
        snapshot = self.query('/spot-assignments/' + self.agent.node_id)
        for request_id, hold in snapshot['holds'].items():
            current = hold['pipeline']['spot']
            for c in [*current.get('history', []), current]:
                identity = [request_id, c['challenge_height'], c['round']]
                if (c['auditor'] != self.agent.node_id or c.get('received')
                        or c.get('evidence_resolved') or c.get('accusation') or c.get('replace_after')
                        or snapshot['height'] <= c['deadline'] or identity in self.done
                        or snapshot['height'] >= hold['settle_height'] - 60):
                    continue
                try:
                    self.accuse(request_id, c, snapshot['height'])
                except Exception as exc:
                    # Do not expose configuration, evidence, prompts or URLs.
                    LOG.warning('spot evidence unavailable or submission unconfirmed request=%s error=%s',
                                request_id, type(exc).__name__)
                    continue
                self.done.append(identity)
                durable_json(self.path, self.done)

    def accuse(self, request_id, c, height):
        args = self.agent.chain_args
        if len(bytes.fromhex(request_id)) != 32 or Path(request_id).name != request_id:
            raise ValueError('invalid spot request identity')
        # Only the deployment pin or a locally provisioned request context may
        # supply the gateway key. Assignment/job data cannot override it.
        if getattr(args, 'spot_profile_config', None):
            pin = json.loads(Path(args.spot_profile_config).read_text())['gateway_public']
        else:
            root = Path(getattr(args, 'spot_context_dir', None) or Path(args.state_dir) / 'spot-context')
            pin = json.loads((root / (request_id + '.json')).read_text())['gateway_public']
        context = dict(request_id=request_id, challenge_height=c['challenge_height'],
                       steps=c['step_set'], round=c['round'], open_deadline=c['deadline'],
                       challenge_record=c)
        def attestor(*unused):
            with urlopen(args.spot_gateway_url.rstrip('/') + '/v1/spot/' + request_id + '/absence?challenge_height=' + str(c['challenge_height']),
                         timeout=30) as response:
                return json.load(response)
        spool = DirectorySpool(args.spot_spool_dir, attestor=attestor)
        attestation = spool.absence(context, height)
        # Reuse one immutable signed observation for the existing three-piece
        # proof builder; it also checks the real local spool for delivered data.
        spool.attestor = lambda *unused: attestation
        with self.agent.spot_tx_lock:
            payload = unopened_action(self.agent, context, spool, bytes.fromhex(pin), attestation, height)
            self.submit(payload)


def start(agent, stop=None, interval=10):
    stop = stop or threading.Event()
    def run():
        patrol = None
        while not stop.is_set():
            try:
                if patrol is None:
                    patrol = Patrol(agent)
                patrol.poll()
            except Exception as exc:
                LOG.warning('spot patrol unavailable error=%s', type(exc).__name__)
            stop.wait(interval)
    thread = threading.Thread(target=run, name='spot-patrol', daemon=True)
    thread.start()
    return thread
