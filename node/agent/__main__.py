import argparse
from pathlib import Path
from . import Agent
from engine import load_engine


def main():
    import sys
    if len(sys.argv) > 1 and sys.argv[1] == 'version':
        print((Path(__file__).resolve().parents[2] / 'VERSION').read_text().strip())
        return
    if len(sys.argv) > 1 and sys.argv[1] == 'profile':
        from .publish import main as publish_main
        publish_main(sys.argv[2:])
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
            cli.add_argument('--engine', choices=('mock', 'transformers', 'pipeline', 'llamacpp', 'vllm'), required=True)
            cli.add_argument('--model-path')
            cli.add_argument('--generation-timeout', type=float, default=300,
                             help='SPI generation deadline in seconds (default 300)')
            cli.add_argument('--audit-role', action='store_true', help='offer stake-backed forced-prefix audit replay')
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
            cli.add_argument('--chain-cli', default='emberchain')
            cli.add_argument('--ledger-chain-id')
            cli.add_argument('--registration-ttl', type=int, default=200000)
            cli.add_argument('--genesis-sha256')
            cli.add_argument('--manifest-sha256')
            cli.add_argument('--health-listen', default='127.0.0.1:19192')
        args = cli.parse_args(sys.argv[2:])
        if command == 'id':
            print(identity(args.state_dir)[1])
        else:
            join(args)
        return
    parser = argparse.ArgumentParser()
    parser.add_argument('--address', default='127.0.0.1:19091')
    parser.add_argument('--id', required=True)
    parser.add_argument('--engine', choices=('mock', 'transformers'), default='mock')
    parser.add_argument('--model-path')
    parser.add_argument('--device', choices=('auto', 'cpu', 'cuda'), default='auto')
    parser.add_argument('--cheat', action='store_true')
    parser.add_argument('--tamper', action='store_true')
    parser.add_argument('--delay', type=float, default=0)
    args = parser.parse_args()
    engine = load_engine(args.engine, model_path=args.model_path,
                         device=args.device, tamper=args.tamper)
    print(f'NODE {args.id} DEVICE {engine.device}', flush=True)
    Agent(args.id, engine, cheat=args.cheat, delay=args.delay).serve(args.address)


if __name__ == '__main__':
    main()
