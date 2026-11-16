#!/usr/bin/env bash
set -euo pipefail
repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
if [[ ${1:-} == --help || $# != 2 || ${1:-} != --archive ]]; then
  echo 'Usage: sudo script/restore.sh --archive /absolute/path/agentowl-backup.tar'
  echo 'Restores into an empty deploy/data. Requires the matching project revision.'
  echo 'First stop the dedicated Runner daemon and unmount its runtime/tasks mounts.'
  echo 'Services remain stopped; restore account UID/subuid mappings and run host preflight before starting.'
  [[ ${1:-} == --help ]] && exit 0 || exit 2
fi
[[ $(uname -s) == Linux && $EUID == 0 ]] || { echo 'Cold restore requires root on Linux to preserve numeric ownership.' >&2; exit 1; }
exec python3 - "$repo" "$2" <<'PY'
import hashlib, json, os, pathlib, pwd, shutil, stat, subprocess, sys, tarfile, tempfile
def digest_file(content):
    digest=hashlib.sha256()
    for chunk in iter(lambda: content.read(1<<20),b''):digest.update(chunk)
    return digest.hexdigest()
repo, source = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
deploy, data = repo/'deploy', repo/'deploy/data'
if not source.is_absolute() or not source.is_file() or source.is_symlink():
    raise SystemExit('Archive must be an absolute regular file path.')
expected=pathlib.Path(str(source)+'.sha256').read_text().strip()
with open(source,'rb') as content:
    actual=digest_file(content)
if len(expected)!=64 or actual!=expected:
    raise SystemExit('Backup checksum does not match; no services were changed.')
if data.is_symlink() or os.path.ismount(data) or data.exists() and (not data.is_dir() or any(data.iterdir())):
    raise SystemExit('Restore only accepts an empty deploy/data; retain the existing directory as a separate backup first.')
with tarfile.open(source,'r:') as archive:
    members=[];seen={};total=0
    for member in archive:
        if len(members)>=200000:raise SystemExit('Archive has too many entries.')
        members.append(member)
        path=pathlib.PurePosixPath(member.name)
        if len(member.name)>4096 or str(path)!=member.name or path.is_absolute() or '..' in path.parts or '\\' in member.name or '\x00' in member.name:
            raise SystemExit('Unsafe archive path.')
        if member.name not in ('agentowl-backup.json','compose.yaml','.env','data') and not member.name.startswith('data/'):
            raise SystemExit('Unexpected archive root.')
        if not (member.isdir() or member.isreg()) or member.name in seen or member.size<0 or member.mode&0o7000 or member.uid<0 or member.gid<0:
            raise SystemExit('Unsupported archive entry type, duplicate or special mode.')
        seen[member.name]=member
        total+=member.size
    for name,member in seen.items():
        for parent in pathlib.PurePosixPath(name).parents:
            if str(parent) in seen and not seen[str(parent)].isdir():
                raise SystemExit('Archive contains a file/directory collision.')
    if not all(name in seen for name in ('agentowl-backup.json','compose.yaml','.env','data')):
        raise SystemExit('Backup is incomplete.')
    if not seen['data'].isdir() or any(not seen[name].isreg() for name in ('agentowl-backup.json','compose.yaml','.env')) or seen['agentowl-backup.json'].size>65536 or seen['compose.yaml'].size>1048576 or seen['.env'].size>1048576:
        raise SystemExit('Backup root types or metadata sizes are invalid.')
    metadata=json.load(archive.extractfile('agentowl-backup.json'))
    if metadata.get('format')!=1 or metadata.get('kind')!='offline-full-data':
        raise SystemExit('Unsupported backup format.')
    if metadata.get('runner'):
        identity=metadata.get('runner_identity') or {}
        uid,gid=identity.get('uid'),identity.get('gid')
        if not isinstance(uid,int) or not isinstance(gid,int) or uid<=0 or gid<0:
            raise SystemExit('Backup Runner identity is invalid.')
        process=subprocess.run(['pgrep','-u',str(uid),'-x','dockerd'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        if process.returncode!=1:
            raise SystemExit('Dedicated Runner daemon is active or its process state cannot be checked; stop it before restoring.')
        try: account=pwd.getpwuid(uid)
        except KeyError: raise SystemExit('Create the dedicated Runner account with UID='+str(uid)+' and GID='+str(gid)+' before restore.')
        if account.pw_gid!=gid:raise SystemExit('Runner GID differs from the backup.')
        for kind in ('subuid','subgid'):
            current=sorted(line.split(':',1)[1] for line in pathlib.Path('/etc/'+kind).read_text().splitlines() if line.startswith(account.pw_name+':'))
            expected_ranges=sorted(line.split(':',1)[1] for line in identity.get(kind,[]))
            if not expected_ranges or current!=expected_ranges:
                raise SystemExit('Runner '+kind+' mappings must match the backup before restore.')
    if (deploy/'compose.yaml').read_bytes()!=archive.extractfile('compose.yaml').read():
        raise SystemExit('Compose differs from backup; restore the matching project revision first.')
    if (deploy/'.env').exists() and (deploy/'.env').read_bytes()!=archive.extractfile('.env').read():
        raise SystemExit('Existing .env differs from backup; retain it separately before restoring.')
    if shutil.disk_usage(deploy).free<total+(2<<30):
        raise SystemExit('Insufficient free disk capacity for a fully allocated restore plus 2 GiB reserve.')
    # The existing deployment is intentionally stopped only after archive validation.
    env=deploy/'.env' if (deploy/'.env').exists() else deploy/'.env.example'
    command=['docker','compose','--env-file',str(env),'-f',str(deploy/'compose.yaml'),'--profile','runner']
    subprocess.run(command+['stop'],check=True)
    if subprocess.check_output(command+['ps','--status','running','--quiet'],text=True).strip():
        raise SystemExit('Some Compose services are still running.')
    with tempfile.TemporaryDirectory(prefix='.agentowl-restore-',dir=deploy) as tmp:
        staging=pathlib.Path(tmp)
        for member in members:
            path=staging/member.name
            path.parent.mkdir(parents=True,exist_ok=True)
            if member.isdir():path.mkdir(exist_ok=True)
            else:
                with open(path,'xb') as target,archive.extractfile(member) as content:
                    shutil.copyfileobj(content,target,1<<20)
                    target.flush();os.fsync(target.fileno())
            os.chown(path,member.uid,member.gid)
            if not member.isdir():os.chmod(path,member.mode&0o777)
        for member in sorted((m for m in members if m.isdir()),key=lambda m:len(m.name),reverse=True):
            os.chmod(staging/member.name,member.mode&0o777)
        if data.exists():data.rmdir()
        os.replace(staging/'data',data)
        if not (deploy/'.env').exists():os.replace(staging/'.env',deploy/'.env')
        os.replace(staging/'agentowl-backup.json',data/'restore-manifest.json')
        os.chown(data/'restore-manifest.json',seen['data'].uid,seen['data'].gid)
        os.chmod(data/'restore-manifest.json',0o600)
        os.sync()
print('Data restored. Services remain stopped; no host systemd or mount settings were changed.')
if metadata.get('runner'):
    identity=metadata['runner_identity']
    print('Runner requires UID='+str(identity['uid'])+', GID='+str(identity['gid'])+' and the original subuid/subgid ranges from data/restore-manifest.json.')
    print('Use a compatible Docker/storage-driver version, run runner-setup.sh to recreate mounts/socket, then runner-preflight.sh before starting services.')
print('After inspecting configuration, run make migrate and make up. Release confirmation and user feedback remain manual.')
PY
