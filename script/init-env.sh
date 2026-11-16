#!/usr/bin/env bash
set -euo pipefail
repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
exec python3 - "$repo" "$@" <<'PY'
import argparse, base64, os, pathlib, secrets, sys, uuid

repo = pathlib.Path(sys.argv[1])
parser = argparse.ArgumentParser(description='Create local configuration and secret files without printing secret values.')
parser.add_argument('--directory', default=str(repo / 'deploy'))
args = parser.parse_args(sys.argv[2:])
deploy = pathlib.Path(args.directory).resolve()
deploy.mkdir(parents=True, exist_ok=True)
data = deploy / 'data'
def directory(path, mode):
    if path.is_symlink() or path.exists() and not path.is_dir():
        raise SystemExit('Refusing a non-directory or symlink configuration path: ' + path.name)
    path.mkdir(mode=mode, exist_ok=True)
directory(data, 0o755)
private = data / 'secrets'
directory(private, 0o700)
os.chmod(private, 0o700)
for name in ('artifacts', 'workspaces', 'backups'):
    directory(data / name, 0o700)

def write_once(path, text, mode):
    try:
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, mode)
    except FileExistsError:
        if path.is_symlink() or not path.is_file():
            raise SystemExit('Refusing a non-regular configuration path: ' + path.name)
        return
    with os.fdopen(fd, 'w') as f:
        f.write(text)

def secret_file(name, factory):
    path = private / name
    write_once(path, factory() + '\n', 0o644)
    # Parent mode 0700 protects host access; the individual read-only container bind
    # must be readable by the service's non-root UID.
    os.chmod(path, 0o644)
    return path.read_text().strip()

password = secret_file('postgres_password', lambda: secrets.token_hex(24))
secret_file('encryption_key', lambda: base64.b64encode(secrets.token_bytes(32)).decode())
secret_file('bootstrap_password', lambda: secrets.token_urlsafe(24))
secret_file('runner_service_token', lambda: secrets.token_hex(32))
secret_file('runner_callback_token', lambda: secrets.token_hex(32))
secret_file('oidc_client_secret', lambda: '')
secret_file('database_url', lambda: f'postgresql://agentowl:{password}@postgres:5432/agentowl?sslmode=disable')
secret_file('dev_database_url', lambda: f'postgresql://agentowl:{password}@127.0.0.1:55432/agentowl?sslmode=disable')

template = (repo / 'deploy' / '.env.example').read_text()
template = template.replace('APP_UID=1000', f'APP_UID={os.getuid()}').replace('APP_GID=1000', f'APP_GID={os.getgid()}')
template = template.replace('RUNNER_MACHINE_ID=', 'RUNNER_MACHINE_ID=' + str(uuid.uuid4()))
write_once(deploy / '.env', template, 0o600)
refs = {'DATABASE_URL_FILE': private / 'dev_database_url', 'ENCRYPTION_KEY_FILE': private / 'encryption_key',
        'BOOTSTRAP_PASSWORD_FILE': private / 'bootstrap_password',
        'OIDC_CLIENT_SECRET_FILE': private / 'oidc_client_secret'}
dev = ''.join(f'{key}={value}\n' for key, value in refs.items())
dev += f'DATA_DIR={data}\nLISTEN_ADDR=127.0.0.1:8080\nINTERNAL_LISTEN_ADDR=127.0.0.1:8081\n'
write_once(private / 'dev.env', dev, 0o600)
print('Configuration and secret files initialized under ' + str(deploy) + '; existing files preserved.')
print('Local bootstrap password: data/secrets/bootstrap_password. No secret values were printed.')
PY
