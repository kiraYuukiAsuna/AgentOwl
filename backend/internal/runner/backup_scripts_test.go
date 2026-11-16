package runner

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// Execute the real embedded Python against temporary directories. All process
// calls, chown and sync are intercepted: no Docker, host daemon or mount is used.
func TestColdBackupScriptsRoundTripAndUnsafeArchives(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("backup scripts require Python 3 for their safety tests")
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	program := `
import contextlib, hashlib, io, json, os, pathlib, stat, subprocess, sys, tarfile, tempfile
from unittest.mock import patch
repository=pathlib.Path(sys.argv[1])
def source(name):
    text=(repository/'script'/name).read_text()
    return text.split("<<'PY'\n",1)[1].rsplit('\nPY',1)[0]
backup,restore=source('backup.sh'),source('restore.sh')
compile(backup,'backup.sh','exec');compile(restore,'restore.sh','exec')
calls=[]
def run(command,**kwargs):
    calls.append(command)
    return subprocess.CompletedProcess(command,1 if command[0]=='pgrep' else 0)
def output(command,**kwargs):
    calls.append(command)
    return 'Docker fake-protocol-only' if command==['docker','--version'] else ''
def execute(script,root,arg):
    calls.clear();capture=io.StringIO()
    with patch.object(sys,'argv',['script',str(root),str(arg)]),patch('subprocess.run',run),patch('subprocess.check_output',output),patch('os.chown'),patch('os.sync'),contextlib.redirect_stdout(capture):
        exec(compile(script,'backup-restore','exec'),{})
    return capture.getvalue()
def layout(root):
    deploy=root/'deploy';deploy.mkdir(parents=True)
    (deploy/'compose.yaml').write_text('name: agentowl-test\nservices: {}\n')
    (deploy/'.env.example').write_text('AUTH_MODE=local\n')
    return deploy
def checksum(path):
    digest=hashlib.sha256(path.read_bytes()).hexdigest()
    pathlib.Path(str(path)+'.sha256').write_text(digest+'\n')
with tempfile.TemporaryDirectory(prefix='agentowl-backup-test-') as tmp:
    base=pathlib.Path(tmp);original=layout(base/'original')
    (original/'.env').write_text('AUTH_MODE=local\n')
    secrets=original/'data/secrets';secrets.mkdir(parents=True)
    secret=secrets/'key';secret.write_text('private-test-value');secret.chmod(0o600)
    archive=base/'backup.tar'
    printed=execute(backup,original.parent,archive)
    assert 'private-test-value' not in printed
    assert stat.S_IMODE(archive.stat().st_mode)==0o600
    assert any(command[-1]=='stop' for command in calls)
    restored=layout(base/'restored')
    execute(restore,restored.parent,archive)
    assert (restored/'data/secrets/key').read_text()=='private-test-value'
    assert stat.S_IMODE((restored/'data/secrets/key').stat().st_mode)==0o600
    try:execute(restore,restored.parent,archive)
    except SystemExit:assert not calls
    else:raise AssertionError('existing data overwritten')
    for variant in ('traversal','absolute','symlink','hardlink','device','duplicate','collision','special-mode','wrong-root-type'):
        bad=base/(variant+'.tar');target=layout(base/variant)
        with tarfile.open(bad,'w') as tar:
            def add(name,body=b'',kind=tarfile.REGTYPE,mode=0o600):
                info=tarfile.TarInfo(name);info.type=kind;info.mode=mode
                if kind==tarfile.REGTYPE:info.size=len(body)
                if kind in (tarfile.SYMTYPE,tarfile.LNKTYPE):info.linkname='/tmp/outside-agentowl'
                tar.addfile(info,io.BytesIO(body) if kind==tarfile.REGTYPE else None)
            add('agentowl-backup.json',json.dumps({'format':1,'kind':'offline-full-data','runner':False}).encode())
            add('compose.yaml',(target/'compose.yaml').read_bytes());add('.env',b'AUTH_MODE=local\n')
            add('data',kind=tarfile.REGTYPE if variant=='wrong-root-type' else tarfile.DIRTYPE,mode=0o700)
            if variant=='traversal':add('data/../../escape',b'escape')
            elif variant=='absolute':add('/tmp/outside-agentowl',b'escape')
            elif variant=='symlink':add('data/link',kind=tarfile.SYMTYPE)
            elif variant=='hardlink':add('data/link',kind=tarfile.LNKTYPE)
            elif variant=='device':add('data/device',kind=tarfile.CHRTYPE)
            elif variant=='duplicate':add('data/x',b'one');add('data/x',b'two')
            elif variant=='collision':add('data/x',b'file');add('data/x/child',b'child')
            elif variant=='special-mode':add('data/exec',b'file',mode=0o4755)
        checksum(bad)
        try:execute(restore,target.parent,bad)
        except SystemExit:assert not calls,variant+' contacted Docker before validation'
        else:raise AssertionError(variant+' accepted')
        assert not (target/'data').exists()
    damaged=base/'damaged.tar';damaged.write_bytes(archive.read_bytes())
    pathlib.Path(str(damaged)+'.sha256').write_text('0'*64)
    target=layout(base/'damaged')
    try:execute(restore,target.parent,damaged)
    except SystemExit:assert not calls
    else:raise AssertionError('checksum mismatch accepted')
print('cold backup roundtrip and 10 malicious archive/checksum cases passed with fake process adapter')
`
	command := exec.Command(python, "-c", program, repo)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("backup script safety checks: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}
