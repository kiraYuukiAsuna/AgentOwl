#!/usr/bin/env bash
set -euo pipefail
repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
exec python3 - "$repo" "$@" <<'PY'
import argparse, os, pathlib, re, signal, subprocess, sys, time

root = pathlib.Path(sys.argv[1])
parser = argparse.ArgumentParser(description='Run the local API, Worker and Next.js against explicitly configured services.')
parser.add_argument('--check-env', action='store_true')
parser.add_argument('--skip-migrate', action='store_true')
args = parser.parse_args(sys.argv[2:])
env = {}
for name in (root/'deploy/.env', root/'deploy/data/secrets/dev.env'):
    if not name.is_file():
        raise SystemExit('Missing local configuration; run make init first.')
    for line in name.read_text().splitlines():
        if not line.strip() or line.lstrip().startswith('#'):
            continue
        if '=' not in line:
            raise SystemExit('Invalid dotenv entry in ' + name.name)
        key, value = line.split('=', 1)
        key, value = key.strip(), value.strip()
        if not re.fullmatch(r'[A-Z][A-Z0-9_]*', key):
            raise SystemExit('Invalid configuration key in ' + name.name)
        if len(value) >= 2 and value[0] == value[-1] and value[0] in ('"', "'"):
            value = value[1:-1]
        env[key] = value
env.update(os.environ)
if env.get('DEV_DATABASE_URL'):
    env['DATABASE_URL'] = env['DEV_DATABASE_URL']
    env.pop('DATABASE_URL_FILE', None)
elif os.environ.get('DATABASE_URL'):
    env.pop('DATABASE_URL_FILE', None)
env['REDIS_URL'] = env.get('DEV_REDIS_URL', '')
env['API_INTERNAL_URL'] = 'http://127.0.0.1:8080'
env['NEXT_TELEMETRY_DISABLED'] = '1'
env.setdefault('GOCACHE', str(root/'.cache/go-build'))
for key in ('ENCRYPTION_KEY_FILE', 'BOOTSTRAP_PASSWORD_FILE'):
    if not pathlib.Path(env[key]).is_file():
        raise SystemExit('Missing secret file for ' + key)
if args.check_env:
    print('Local configuration references are present; no services were contacted.')
    raise SystemExit(0)
if not (root/'Frontend/node_modules').is_dir():
    raise SystemExit('Frontend dependencies are missing; run make bootstrap first.')
if not args.skip_migrate:
    subprocess.run(['go', 'run', './cmd/migrate'], cwd=root/'backend', env=env, check=True)
children = []
stopping = False
def stop(signum=None, frame=None):
    global stopping
    stopping = True
signal.signal(signal.SIGINT, stop)
signal.signal(signal.SIGTERM, stop)
try:
    for command, cwd in ((['go','run','./cmd/api'], root/'backend'),
                         (['go','run','./cmd/worker'], root/'backend'),
                         (['npm','run','dev','--','--hostname','127.0.0.1','--port','3000'], root/'Frontend')):
        children.append(subprocess.Popen(command, cwd=cwd, env=env, start_new_session=True))
    print('Local workspace: http://localhost:3000. Runner execution requires the Linux deployment host.')
    while not stopping:
        if any(child.poll() is not None for child in children):
            raise SystemExit('A development process exited; stopping the remaining processes.')
        time.sleep(0.5)
finally:
    for child in children:
        if child.poll() is None:
            os.killpg(child.pid, signal.SIGTERM)
    for child in children:
        try:
            child.wait(timeout=10)
        except subprocess.TimeoutExpired:
            os.killpg(child.pid, signal.SIGKILL)
            child.wait()
PY
