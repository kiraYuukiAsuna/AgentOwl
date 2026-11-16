#!/usr/bin/env bash
set -euo pipefail
repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
mode=${1:-check}
[[ $mode == check || $mode == --ci ]] || { echo 'Usage: check.sh [--ci]' >&2; exit 2; }
if [[ $mode == --ci && -z ${AGENTOWL_TEST_DATABASE_URL:-} ]]; then
  echo 'CI requires AGENTOWL_TEST_DATABASE_URL for real PostgreSQL integration tests.' >&2
  exit 1
fi
export GOCACHE=${GOCACHE:-$repo/.cache/go-build}
export NEXT_TELEMETRY_DISABLED=1
cd "$repo/backend"
formatting=$(gofmt -l cmd internal migrations)
[[ -z $formatting ]] || { printf '%s\n' "$formatting"; echo 'Run gofmt on these files.' >&2; exit 1; }
go vet ./...
go test -race ./...
go build ./...
cd "$repo/Frontend"
npm run lint
npm run typecheck
npm test
npm run build
cd "$repo"
bash -n script/*.sh
python3 script/init-env-test.py
python3 - <<'PY'
import json
with open('backend/api/openapi.yaml') as f:
    doc = json.load(f)
assert doc['openapi'] == '3.1.0'
ids = [operation['operationId'] for path in doc['paths'].values() for method, operation in path.items()
       if method in ('get','post','put','patch','delete','head','options')]
assert len(ids) == len(set(ids)), 'OpenAPI operationId values must be unique'
assert doc['paths'], 'OpenAPI operations are missing'
print('OpenAPI document structure and operation IDs verified.')
PY
if command -v docker >/dev/null && docker compose version >/dev/null 2>&1; then
  docker compose --env-file deploy/.env.example -f deploy/compose.yaml config --quiet
elif [[ $mode == --ci ]]; then
  echo 'CI requires Docker Compose for deployment validation.' >&2
  exit 1
else
  echo 'Docker Compose is unavailable; deployment parsing was not checked.'
fi
if [[ $mode == --ci ]]; then
  script/gen-client.sh --check
fi
