#!/usr/bin/env bash
set -euo pipefail

role=
prefix=
python=${PYTHON:-python3.12}
profile_family=cpu
while (($#)); do
  case "$1" in
    --role) role=$2; shift 2 ;;
    --prefix) prefix=$2; shift 2 ;;
    --profile-family) profile_family=$2; shift 2 ;;
    --python) python=$2; shift 2 ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done
: "${role:?--role node|validator|gateway required}"
: "${prefix:?--prefix required}"
case "$role" in node|validator|gateway) ;; *) exit 2 ;; esac

if [[ "$role" == node ]]; then
  case "$profile_family" in
    cpu)
      if command -v nvidia-smi >/dev/null 2>&1 && nvidia-smi -L >/dev/null 2>&1; then
        printf 'NVIDIA GPU detected; CPU family remains selected. For CUDA use --profile-family cuda on x86_64 with a supported driver.\n' >&2
      fi
      ;;
    cuda)
      if [[ "$(uname -m)" != x86_64 ]]; then
        printf 'CUDA family supports x86_64 only; choose a supported architecture/runtime explicitly.\n' >&2
        exit 2
      fi
      if ! command -v nvidia-smi >/dev/null 2>&1 || ! nvidia-smi -L >/dev/null 2>&1; then
        printf 'CUDA family requires a working NVIDIA GPU and driver (nvidia-smi). Install the driver before retrying.\n' >&2
        exit 2
      fi
      drivers=$(nvidia-smi --query-gpu=driver_version --format=csv,noheader)
      while IFS= read -r driver; do
        if [[ ! "$driver" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] ||
           [[ "$(printf '%s\n' 550.54.14 "$driver" | sort -V | head -n1)" != 550.54.14 ]]; then
          printf 'CUDA 12.4 requires NVIDIA Linux driver >=550.54.14; found %s. Upgrade the driver before retrying.\n' "$driver" >&2
          exit 2
        fi
      done <<<"$drivers"
      ;;
    *) printf 'Unsupported runtime family: %s. Choose cpu or cuda explicitly.\n' "$profile_family" >&2; exit 2 ;;
  esac
  if ! command -v "$python" >/dev/null 2>&1; then
    printf 'Python 3.12 is required. Install python3.12 and its venv package, then pass --python python3.12.\n' >&2
    exit 2
  fi
  "$python" - <<'PREFLIGHT'
import sys
import subprocess
import tempfile
from pathlib import Path
if sys.version_info[:2] != (3, 12):
    raise SystemExit('Python 3.12 is required (found %s). Install python3.12 + python3.12-venv, then pass --python python3.12.' % sys.version.split()[0])
try:
    import venv
    import ensurepip
    ensurepip.version()
    # Importability does not establish that distro ensurepip can seed pip.
    # Probe the target interpreter before any prefix creation or copying.
    with tempfile.TemporaryDirectory(prefix='ember-venv-probe-') as probe:
        venv.EnvBuilder(with_pip=True).create(probe)
        subprocess.run([str(Path(probe) / 'bin/python'), '-m', 'pip', '--version'],
                       check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
except Exception as exc:
    raise SystemExit('Python 3.12 venv/pip support is missing. On Ubuntu run: sudo apt-get install -y python3.12-venv; then retry. Details: %s' % exc)
PREFLIGHT
fi

source_root=$(cd "$(dirname "$0")" && pwd)
install_root=$(cd "$(dirname "$prefix")" && pwd)/$(basename "$prefix")
mkdir -p "$install_root/bin" "$install_root/config" "$install_root/var"
chmod 700 "$install_root/config" "$install_root/var"
cp -a "$source_root/bin/." "$install_root/bin/"
cp -a "$source_root/lib" "$source_root/profiles" "$source_root/docs" "$install_root/"
install -m 0644 "$source_root/network.json" "$install_root/network.json"
install -m 0755 "$source_root/ember-role" "$install_root/bin/ember-role"
install -m 0600 "$source_root/config/$role.env.example" "$install_root/config/$role.env"
# Every role's installation must carry the local profile trust policy
# example: `ember-node configure` points generated configs at
# config/trust-policy.json inside the installation, and the node and
# gateway launchers both honour EMBER_PROFILE_TRUST_POLICY.  v0.9.2
# shipped the file only in the unpacked bundle, so a standard
# install-then-configure route silently lost the pointer and nodes
# refused the current profile's publisher.
install -m 0644 "$source_root/config/trust-policy.json" "$install_root/config/trust-policy.json"
if [[ "$role" == node ]]; then
  "$python" -m venv "$install_root/venv"
  "$install_root/venv/bin/python" -m pip install --disable-pip-version-check \
    -r "$install_root/lib/ember/requirements-$profile_family.txt"
  "$install_root/venv/bin/python" -m pip install --disable-pip-version-check \
    -r "$install_root/lib/ember/requirements-spi.txt"
  if [[ "$profile_family" == cuda ]]; then
    "$install_root/venv/bin/python" - <<'CUDA_CHECK'
import torch
if torch.__version__ != '2.5.1+cu124' or torch.version.cuda != '12.4' or not torch.cuda.is_available():
    raise SystemExit('CUDA installation failed runtime verification: expected torch 2.5.1+cu124 / CUDA 12.4 and an available GPU. Check the NVIDIA driver.')
torch.empty(1, device='cuda')
torch.cuda.synchronize()
print('CUDA runtime verified: torch=%s cuda=%s GPU=%s' % (torch.__version__, torch.version.cuda, torch.cuda.get_device_name(0)))
CUDA_CHECK
  fi
fi
printf 'installed role=%s prefix=%s config=%s/config/%s.env\n' \
  "$role" "$install_root" "$install_root" "$role"
