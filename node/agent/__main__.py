import argparse
import os
import sys
from pathlib import Path
from . import Agent
from engine import load_engine


def main():
    commands = {
        'id': 'Show or create the node identity',
        'configure': 'Write a node configuration',
        'join': 'Register and join a profile',
        'profile': 'Prepare, verify or publish profile artifacts',
        'exit': 'Inspect registration and unbond stake',
        'request-exec-exit': 'Request executor exit on the chain',
        'conformance': 'Check model execution conformance',
        'image-conformance': 'Check image execution conformance',
        'shard': 'Run a shard service',
        'serve': 'Run the standalone engine service',
        'version': 'Print the release version',
    }
    parser = argparse.ArgumentParser(prog='ember-node')
    subcommands = parser.add_subparsers(dest='command', metavar='COMMAND')
    for name, description in commands.items():
        subcommands.add_parser(name, help=description)
    if len(sys.argv) < 2 or sys.argv[1] in ('-h', '--help'):
        parser.print_help()
        return
    if sys.argv[1] not in commands:
        parser.print_help(sys.stderr)
        parser.exit(2, f'ember-node: error: unknown command {sys.argv[1]!r}\n')
    if len(sys.argv) > 1 and sys.argv[1] == 'image-conformance':
        from image_conformance import main as image_main
        image_main(sys.argv[2:])
        return
    if len(sys.argv) > 1 and sys.argv[1] == 'version':
        print((Path(__file__).resolve().parents[2] / 'VERSION').read_text().strip())
        return
    if len(sys.argv) > 1 and sys.argv[1] == 'profile':
        from .publish import main as publish_main
        publish_main(sys.argv[2:])
        return
    if len(sys.argv) > 1 and sys.argv[1] == 'request-exec-exit':
        from .lifecycle import request_exec_exit_main
        request_exec_exit_main(sys.argv[2:])
        return
    if len(sys.argv) > 1 and sys.argv[1] == 'exit':
        from .lifecycle import main as exit_main
        exit_main(sys.argv[2:])
        return
    if len(sys.argv) > 1 and sys.argv[1] == 'configure':
        from .network import main as configure
        configure(sys.argv[2:])
        return
    if len(sys.argv) > 1 and sys.argv[1] == 'conformance':
        from conformance import main as conformance_main
        conformance_main(sys.argv[2:])
        return
    if len(sys.argv) > 1 and sys.argv[1] == 'shard':
        from shard_rpc import main as shard_main
        sys.argv = [sys.argv[0] + ' shard', *sys.argv[2:]]
        shard_main()
        return
    if len(sys.argv) > 1 and sys.argv[1] in ('join', 'id'):
        from .join import identity, join
        command = sys.argv[1]
        cli = argparse.ArgumentParser(prog='ember-node ' + command)
        cli.add_argument('--state-dir', default='node/.state')
        if command == 'join':
            cli.add_argument('--coordinator', required=True)
            cli.add_argument('--tls-fingerprint')
            cli.add_argument('--insecure-localhost', action='store_true')
            cli.add_argument('--chain', required=True)
            cli.add_argument('--engine', choices=('mock', 'transformers', 'pipeline', 'llamacpp', 'vllm', 'diffusers-t2i'), required=True)
            cli.add_argument('--model-path')
            cli.add_argument('--image-pixel-policy', help='pinned T4 pixel audit policy JSON')
            cli.add_argument('--image-retention-seconds', type=int, default=3600)
            cli.add_argument('--image-calibration', help='operator-pinned image reference trajectory and signed profile limits JSON')
            cli.add_argument('--scheme-hash', help='pinned layout scheme hash (64 hex)')
            cli.add_argument('--generation-timeout', type=float, default=300,
                             help='SPI generation deadline in seconds (default 300)')
            cli.add_argument('--contribution', choices=('low', 'balanced', 'full'))
            cli.add_argument('--contribution-config', help='ordered profile catalogue, capacity and custom overrides JSON')
            cli.add_argument('--contribution-concurrency', type=int)
            cli.add_argument('--vram-budget-bytes', type=int)
            cli.add_argument('--contribution-context-tokens', type=int, default=512)
            cli.add_argument('--spot-spool-dir', help='durable gateway spool mirror for v14 audits')
            cli.add_argument('--spot-gateway-url', help='operator-pinned gateway query URL for signed absence attestations')
            cli.add_argument('--spot-context-dir', help='locally pinned per-request replay contexts (default STATE/spot-context)')
            cli.add_argument('--spot-profile-config', help='local model/tokenizer pins and deployment gateway key for context derivation')
            cli.add_argument('--no-audit', action='store_true', help='opt out of chain-drawn review until registration expiry')
            cli.add_argument('--no-exec', action='store_true', help='opt out of gateway execution until registration expiry')
            cli.add_argument('--audit-role', action='store_true', help='legacy: add review eligibility to engine roles')
            cli.add_argument('--tamper', action='store_true', help='test-only committed logit corruption')
            cli.add_argument('--engine-endpoint')
            cli.add_argument('--engine-model')
            cli.add_argument('--engine-version', default='unknown')
            cli.add_argument('--device', choices=('auto', 'cpu', 'cuda'), default='auto')
            cli.add_argument('--stage', action='append')
            cli.add_argument('--scheduler-key-file')
            cli.add_argument('--scheduler-id', action='append', default=[],
                             help='authorized on-chain scheduler account ID')
            cli.add_argument('--trust-policy', help='local publisher/endorsement policy JSON')
            cli.add_argument('--chain-rpc', help='CometBFT RPC exposed by the selected seed')
            cli.add_argument('--liveness-enabled', action=argparse.BooleanOptionalAction, default=True)
            cli.add_argument('--liveness-threshold', type=int, default=300)
            cli.add_argument('--liveness-poll', type=float, default=10)
            cli.add_argument('--chain-cli', default='emberchain')
            cli.add_argument('--ledger-chain-id')
            cli.add_argument('--stake-target', type=int, default=os.environ.get('EMBER_STAKE_TARGET') or None,
                             help='target total stake in subunits (default: EMBER_STAKE_TARGET or chain minimum)')
            cli.add_argument('--registration-ttl', type=int, default=200000)
            cli.add_argument('--genesis-sha256')
            cli.add_argument('--manifest-sha256')
            cli.add_argument('--health-listen', default='127.0.0.1:19192')
        args = cli.parse_args(sys.argv[2:])
        if command == 'id':
            print(identity(args.state_dir)[1])
        else:
            try:
                join(args)
            except ValueError as exc:
                cli.error(str(exc))
        return
    parser = argparse.ArgumentParser(prog='ember-node serve')
    parser.add_argument('--address', default='127.0.0.1:19091')
    parser.add_argument('--id', required=True)
    parser.add_argument('--engine', choices=('mock', 'transformers'), default='mock')
    parser.add_argument('--model-path')
    parser.add_argument('--device', choices=('auto', 'cpu', 'cuda'), default='auto')
    parser.add_argument('--cheat', action='store_true')
    parser.add_argument('--tamper', action='store_true')
    parser.add_argument('--delay', type=float, default=0)
    args = parser.parse_args(sys.argv[2:])
    engine = load_engine(args.engine, model_path=args.model_path,
                         device=args.device, tamper=args.tamper)
    print(f'NODE {args.id} DEVICE {engine.device}', flush=True)
    Agent(args.id, engine, cheat=args.cheat, delay=args.delay).serve(args.address)


if __name__ == '__main__':
    main()
