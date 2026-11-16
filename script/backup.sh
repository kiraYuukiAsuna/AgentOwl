#!/usr/bin/env bash
set -euo pipefail
repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
if [[ ${1:-} == --help || $# != 2 || ${1:-} != --output ]]; then
  echo 'Usage: sudo script/backup.sh --output /absolute/path/agentowl-backup.tar'
  echo 'Cold backup: stops all Compose services. If Runner was installed, first stop'
  echo 'its dedicated user systemd daemon and unmount deploy/data/runner/{runtime,tasks}.'
  echo 'The script never changes host systemd or mount state. It leaves services stopped.'
  [[ ${1:-} == --help ]] && exit 0 || exit 2
fi
[[ $(uname -s) == Linux && $EUID == 0 ]] || { echo 'Cold backup requires root on Linux to preserve numeric ownership.' >&2; exit 1; }
exec python3 - "$repo" "$2" <<'PY'
import datetime, hashlib, json, os, pathlib, pwd, stat, subprocess, sys, tarfile, tempfile
def digest_file(content):
    digest=hashlib.sha256()
    for chunk in iter(lambda: content.read(1<<20),b''):digest.update(chunk)
    return digest.hexdigest()
repo, destination = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
deploy, data = repo/'deploy', repo/'deploy/data'
if not destination.is_absolute() or not destination.parent.is_dir():
    raise SystemExit('Output must be an absolute path with an existing parent directory.')
destination = destination.parent.resolve()/destination.name
if destination.is_relative_to(data.resolve()) or destination.exists() or pathlib.Path(str(destination)+'.sha256').exists():
    raise SystemExit('Output must be new and outside deploy/data.')
if not data.is_dir() or data.is_symlink() or not (deploy/'.env').is_file() or (deploy/'.env').is_symlink() or (deploy/'compose.yaml').is_symlink():
    raise SystemExit('Initialized deployment data and .env are required.')
runner = data/'runner'
if runner.exists():
    if not (runner/'home').is_dir():
        raise SystemExit('Runner installation is incomplete; retain its data separately and resolve setup before backup.')
    runner_uid=(runner/'home').stat().st_uid
    if runner_uid<=0:
        raise SystemExit('Runner must use a dedicated non-root account.')
    process=subprocess.run(['pgrep','-u',str(runner_uid),'-x','dockerd'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    if process.returncode!=1:
        raise SystemExit('Dedicated Runner daemon is active or its process state cannot be checked; stop it explicitly before backup.')
    for name in ('runtime', 'tasks'):
        if os.path.ismount(runner/name):
            raise SystemExit('Stop the dedicated Runner daemon and unmount runner/'+name+' before backup; no host service was changed.')
    socket = runner/'control/docker.sock'
    if socket.exists() and subprocess.run(['docker','--host','unix://'+str(socket),'info'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=15).returncode == 0:
        raise SystemExit('Dedicated Runner daemon is still active; stop it explicitly before backup.')
command = ['docker','compose','--env-file',str(deploy/'.env'),'-f',str(deploy/'compose.yaml'),'--profile','runner']
subprocess.run(command+['stop'],check=True)
if subprocess.check_output(command+['ps','--status','running','--quiet'],text=True).strip():
    raise SystemExit('Some Compose services are still running; refusing backup.')
os.sync()
excluded = {'data/runner/runtime','data/runner/tasks','data/runner/control'}
entries = []
for root, dirs, files in os.walk(data, followlinks=False):
    root = pathlib.Path(root)
    for name in sorted(dirs+files):
        p = root/name
        relative = p.relative_to(deploy).as_posix()
        if relative in excluded:
            dirs.remove(name)
            continue
        mode = p.lstat().st_mode
        if not (stat.S_ISDIR(mode) or stat.S_ISREG(mode)) or mode&0o7000:
            raise SystemExit('Unsupported backup entry (links/devices/sockets are refused): '+relative)
        entries.append(p)
metadata = {'format':1,'created_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),
            'kind':'offline-full-data','runner':runner.exists(), 'runner_identity':None,
            'docker_cli':subprocess.check_output(['docker','--version'],text=True).strip()}
if runner.exists():
    identity = (runner/'home').stat()
    account = pwd.getpwuid(identity.st_uid).pw_name
    metadata['runner_identity'] = {'uid':identity.st_uid,'gid':identity.st_gid,
      'subuid':[line.strip() for line in pathlib.Path('/etc/subuid').read_text().splitlines() if line.startswith(account+':')],
      'subgid':[line.strip() for line in pathlib.Path('/etc/subgid').read_text().splitlines() if line.startswith(account+':')]}
fd = os.open(destination,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
try:
    with os.fdopen(fd,'wb') as output, tarfile.open(fileobj=output,mode='w') as archive:
        import io
        body=json.dumps(metadata,sort_keys=True).encode()
        info=tarfile.TarInfo('agentowl-backup.json');info.size=len(body);info.mode=0o600
        archive.addfile(info,io.BytesIO(body))
        for p in [deploy/'compose.yaml',deploy/'.env',data]+sorted(entries):
            archive.add(p,arcname=p.relative_to(deploy).as_posix(),recursive=False)
    with open(destination,'rb') as content:
        digest=digest_file(content)
    with open(str(destination)+'.sha256','x') as checksum:
        os.chmod(checksum.name,0o600);checksum.write(digest+'\n')
except BaseException:
    destination.unlink(missing_ok=True)
    raise
print('Cold backup created: '+str(destination))
print('The archive contains credentials; keep its 0600 permissions. Services remain stopped.')
print('Restore requires the same project revision, numeric UID/GID and Runner subordinate-ID mappings.')
PY
