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
  if [[ "$profile_family" != cpu ]]; then
    printf 'Unsupported runtime family: %s. This release ships measured CPU profiles only.\n' "$profile_family" >&2
    exit 2
  fi
  if ! command -v "$python" >/dev/null 2>&1; then
    printf 'Python 3.12 is required. Install python3.12 and its venv package, then pass --python python3.12.\n' >&2
    exit 2
  fi
  "$python" - <<'PREFLIGHT'
import sys
if sys.version_info[:2] != (3, 12):
    raise SystemExit('Python 3.12 is required (found %s). Install python3.12 + python3.12-venv, then pass --python python3.12.' % sys.version.split()[0])
try:
    import venv
    import ensurepip
    ensurepip.version()
except (ImportError, OSError) as exc:
    raise SystemExit('Python 3.12 venv/pip support is missing. Install the python3.12-venv package using your host package manager, then retry. Details: %s' % exc)
PREFLIGHT
fi

source_root=$(cd "$(dirname "$0")" && pwd)
install_root=$(cd "$(dirname "$prefix")" && pwd)/$(basename "$prefix")
mkdir -p "$install_root/bin" "$install_root/config" "$install_root/var"
chmod 700 "$install_root/config" "$install_root/var"
cp -a "$source_root/bin/." "$install_root/bin/"
cp -a "$source_root/lib" "$source_root/profiles" "$source_root/docs" "$install_root/"
install -m 0755 "$source_root/ember-role" "$install_root/bin/ember-role"
install -m 0600 "$source_root/config/$role.env.example" "$install_root/config/$role.env"
if [[ "$role" == node ]]; then
  "$python" -m venv "$install_root/venv"
  "$install_root/venv/bin/python" -m pip install --disable-pip-version-check \
    -r "$install_root/lib/ember/requirements-cpu.txt"
  "$install_root/venv/bin/python" -m pip install --disable-pip-version-check \
    -r "$install_root/lib/ember/requirements-spi.txt"
fi
printf 'installed role=%s prefix=%s config=%s/config/%s.env\n' \
  "$role" "$install_root" "$install_root" "$role"
