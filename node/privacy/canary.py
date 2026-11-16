"""Consumer canaries and operator-side surfacing telemetry. No enforcement."""
import hashlib
import secrets
from pathlib import Path


def plant(prompt, participants):
    marker='EMBER_CANARY_'+secrets.token_hex(24)
    return prompt+'\n'+marker,dict(marker=marker,participants=sorted(set(participants)))


def scan(records, paths):
    findings=[]
    for path in paths:
        path=Path(path)
        raw=path.read_bytes()
        for record in records:
            if record['marker'].encode() in raw:
                findings.append(dict(marker_hash=hashlib.sha256(record['marker'].encode()).hexdigest(),
                    source=str(path),path_participants=record['participants'],telemetry_only=True))
    return findings


def main():
    import argparse,json
    parser=argparse.ArgumentParser(description='Canary surfacing telemetry; never enforcement or attribution proof')
    parser.add_argument('--records',required=True,help='consumer-authorized local marker/participant JSON')
    parser.add_argument('paths',nargs='+')
    args=parser.parse_args()
    print(json.dumps(scan(json.loads(Path(args.records).read_text()),args.paths),sort_keys=True))

if __name__=='__main__':
    main()
