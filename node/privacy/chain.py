"""Genesis-verified chain reads and signed transactions via emberchain CLI."""
import json
import subprocess
from .escrow import identity
from .crypto import public


class Chain:
    def __init__(self, cli, rpc, chain_id, genesis, signing_key, key_path):
        self.cli, self.rpc, self.chain_id, self.genesis = cli, rpc, chain_id, genesis
        self.signing_key, self.key_path = signing_key, str(key_path)
        self.actor = identity(public(signing_key))

    def state(self):
        run = subprocess.run([self.cli, 'query', '--rpc', self.rpc, '--path', '/state',
            '--verify-state', '--genesis', str(self.genesis)], check=True, capture_output=True, text=True)
        proof, separator, state_json = run.stdout.partition("\n")
        if not separator or not proof.startswith("state_height=") or "commit=genesis-verified" not in proof:
            raise ValueError("missing verified chain proof")
        state = json.loads(state_json)
        if state["parameters"]["chain_id"] != self.chain_id:
            raise ValueError("wrong chain id")
        return state

    def request(self, request_id):
        return (self.state().get('privacy') or {}).get('requests', {})[request_id]

    def epoch(self, epoch):
        return self.state()['privacy']['epochs'][epoch]

    def operator(self, actor):
        return self.state()['privacy']['operators'][actor]

    def submit(self, op, **fields):
        return self.submit_action(dict(type='privacy', privacy=dict(op=op, **fields)))

    def submit_action(self, action):
        state = self.state()
        run = subprocess.run([self.cli, 'tx', '--rpc', self.rpc, '--chain-id', self.chain_id,
            '--key', self.key_path, '--nonce', str(state['accounts'][self.actor]['nonce']+1),
            '--action', json.dumps(action, separators=(',', ':'))], check=True, capture_output=True, text=True)
        return run.stdout.strip()

    def event(self, kind, request_id='', epoch=None, reference=None, reason=''):
        if epoch is None:
            epoch = self.request(request_id)['epoch']
        return self.submit('event', kind=kind, request_id=request_id, epoch=epoch, reason=reason)

    def hold(self, request_id):
        return self.state()['holds'][request_id]

    def public_bundle(self, ref):
        for record in self.state().get('privacy', {}).get('requests', {}).values():
            refs = [record.get('bundle_ref')] + [s['bundle_ref'] for s in record.get('segments', [])]
            if ref in refs:
                return record['status'] != 'final' and not record.get('migrations')
        return False

    def model(self, model_id):
        return self.state()['models'][model_id]
