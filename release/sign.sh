#!/usr/bin/env bash
set -euo pipefail

dist=${1:?usage: sign.sh DIST PRIVATE_KEY}
key=${2:?usage: sign.sh DIST PRIVATE_KEY}
openssl pkey -in "$key" -pubout -out "$dist/release-signing.pub"
openssl pkeyutl -sign -rawin -inkey "$key" -in "$dist/SHA256SUMS" \
  -out "$dist/SHA256SUMS.sig"
openssl pkeyutl -verify -pubin -inkey "$dist/release-signing.pub" -rawin \
  -in "$dist/SHA256SUMS" -sigfile "$dist/SHA256SUMS.sig"
