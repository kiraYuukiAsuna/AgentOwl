#!/usr/bin/env bash
set -euo pipefail
repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
version=1.36.1
kiota=${KIOTA_BINARY:-$repo/.cache/tools/kiota}
mode=${1:-generate}
[[ $mode == generate || $mode == --check ]] || { echo 'Usage: gen-client.sh [--check]' >&2; exit 2; }
[[ -x $kiota ]] || { echo "Install Kiota $version in .cache/tools or set KIOTA_BINARY." >&2; exit 1; }
export DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_CLI_HOME="$repo/.cache/dotnet" NUGET_PACKAGES="$repo/.cache/nuget" KIOTA_TUTORIAL_ENABLED=false
mkdir -p "$DOTNET_CLI_HOME" "$NUGET_PACKAGES"
actual=$(python3 - "$kiota" <<'PY'
import subprocess, sys
try:
    result = subprocess.run([sys.argv[1], '--version'], check=True, capture_output=True, text=True, timeout=45)
except subprocess.TimeoutExpired:
    raise SystemExit('Kiota version query timed out; check the .NET runtime and process sandbox.')
print(result.stdout.strip().split('+', 1)[0])
PY
)
[[ $actual == "$version" ]] || { echo "Kiota $version is required (installed version differs)." >&2; exit 1; }
output="$repo/Frontend/lib/api/generated"
if [[ $mode == --check ]]; then
  output=$(mktemp -d "$repo/Frontend/lib/api/.kiota-check.XXXXXX")
  trap 'rm -rf -- "$output"' EXIT
fi
cd "$repo"
python3 - "$kiota" "$output" <<'PY'
import subprocess, sys
try:
    subprocess.run([sys.argv[1], 'generate', '-l', 'TypeScript', '-d', 'backend/api/openapi.yaml',
                    '-c', 'AgentOwlClient', '-n', 'AgentOwl', '-o', sys.argv[2],
                    '--exclude-backward-compatible', '--clean-output'], check=True, timeout=240)
except subprocess.TimeoutExpired:
    raise SystemExit('Kiota generation timed out after 240 seconds.')
PY
if [[ $mode == --check ]]; then
  # Kiota's update-check log varies with network availability; it is not generated source.
  diff -qr -x .kiota.log "$repo/Frontend/lib/api/generated" "$output"
  echo 'Kiota generated client has no drift.'
fi
