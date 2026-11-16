"""Count exactly the tokenizer input used by pipeline_client, without inference."""
import json
import sys

from transformers import AutoTokenizer


def main():
    tokenizer = AutoTokenizer.from_pretrained(
        sys.argv[1], local_files_only=True, trust_remote_code=False)
    from tokenization import artifact_version
    version = artifact_version(sys.argv[1])
    value = len(tokenizer(sys.stdin.read()).input_ids)
    if artifact_version(sys.argv[1]) != version:
        raise ValueError('tokenizer changed during tokenization')
    print(json.dumps(dict(input_tokens=value, tokenizer_version=version)))


if __name__ == '__main__':
    main()
