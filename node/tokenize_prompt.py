"""Count exactly the tokenizer input used by pipeline_client, without inference."""
import json
import sys

from transformers import AutoTokenizer


def main():
    tokenizer = AutoTokenizer.from_pretrained(
        sys.argv[1], local_files_only=True, trust_remote_code=False)
    print(json.dumps(len(tokenizer(sys.stdin.read()).input_ids)))


if __name__ == '__main__':
    main()
