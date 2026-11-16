package runner

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// Run the shell scripts with command shims and fixture paths. No Docker daemon,
// root account, mount, systemd service, or host configuration is used.
func TestRunnerRuntimeScriptsResourceModes(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("runner script tests require Python 3")
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	program := `
import json, os, pathlib, subprocess, sys, tempfile
repository=pathlib.Path(sys.argv[1])
shim=r"""
import json, os, pathlib, subprocess, sys, types
name=pathlib.Path(sys.argv[0]).name
args=sys.argv[1:]
root=pathlib.Path(os.environ['FIXTURE_ROOT'])
with (root/'calls').open('a') as log:
    log.write(json.dumps([name]+args)+'\n')
case=os.environ.get('FIXTURE_CASE','')
if name=='uname':
    print('Linux')
elif name=='id':
    print('65532' if len(args)==1 and args[0]=='-u' else '1001')
elif name=='getent':
    print('agentowl-runner:x:1001:1001::'+str(root/'data/home')+':/bin/bash')
elif name=='runuser':
    raise SystemExit(subprocess.call(args[args.index('--')+1:]))
elif name=='python3':
    # Only replace host filesystem identity/capacity; execute the actual checks.
    if args[0]=='-':
        code=sys.stdin.read()
        sys.argv=['-']+args[1:]
    else:
        assert args[0]=='-c',args
        code=args[1]
        sys.argv=['-c']+args[2:]
    original_stat=os.stat
    def fixture_stat(path,*a,**kw):
        value=original_stat(path,*a,**kw)
        if str(path)==str(root/'data/runtime'):
            return types.SimpleNamespace(st_dev=value.st_dev+(case!='shared-runtime'))
        return value
    os.stat=fixture_stat
    os.statvfs=lambda path: types.SimpleNamespace(f_bavail=1 if case=='no-reserve' else 1024*1024,
                                                 f_frsize=4096,f_favail=10000)
    exec(compile(code,'script-python','exec'),{'__name__':'__main__'})
elif name=='findmnt':
    if '--fstab' in args:
        pass
    else:
        tasks=args[-1].endswith('/tasks')
        mount={'fstype':'tmpfs' if tasks else 'ext4','source':'tmpfs' if tasks else '/dev/loop-test',
               'options':'rw,size=1g,nr_inodes=100000'}
        if case=='unbounded-scratch' and tasks:mount['options']='rw'
        if case=='wrong-runtime' and not tasks:mount['source']='overlay'
        print(json.dumps({'filesystems':[mount]}))
elif name=='docker':
    assert args[:2]==['--host','unix://'+str(root/'data/control/docker.sock')],args
    command=args[2:]
    if command[0]=='info':
        info={'SecurityOptions':['name=rootless'],'CgroupVersion':'2','CgroupDriver':'systemd',
              'DockerRootDir':str(root/'data/runtime'),'MemoryLimit':True,'PidsLimit':True,'CpuCfsQuota':True}
        if case=='v1':
            info.update(CgroupVersion='1',CgroupDriver='none',MemoryLimit=False,PidsLimit=False,CpuCfsQuota=False)
        if case=='rootful':info['SecurityOptions']=[]
        if case=='wrong-root':info['DockerRootDir']='/var/lib/docker'
        if case=='no-cpu':info['CpuCfsQuota']=False
        print(json.dumps(info))
    elif command[:2]==['image','inspect']:
        pass
    elif command[0]=='run':
        probe=command[-1]
        for original,target in [('/sys/class/net',root/'net'),('/proc/mounts',root/'proc-mounts'),
                                ('/sys/fs/cgroup',root/'cgroup'),('/var/run/docker.sock',root/'socket-leak'),
                                ('/runner/control/docker.sock',root/'control-socket-leak')]:
            probe=probe.replace(original,str(target))
        if case=='root-user':probe=probe.replace('$(id -u)','0')
        raise SystemExit(subprocess.call(['/bin/sh','-ec',probe]))
    else:
        raise AssertionError(command)
elif name in ('chown','mountpoint','systemctl','loginctl'):
    pass
else:
    raise AssertionError('unexpected host operation: '+name+' '+str(args))
"""
with tempfile.TemporaryDirectory(prefix='agentowl-scripts-',dir='/tmp') as temporary:
    root=pathlib.Path(temporary).resolve()
    commands=root/'bin';commands.mkdir()
    for name in ('uname','id','getent','runuser','python3','findmnt','docker','chown','mountpoint',
                 'systemctl','loginctl','dockerd-rootless.sh','newuidmap','newgidmap','useradd',
                 'fallocate','mkfs.ext4','mount'):
        path=commands/name
        path.write_text('#!'+sys.executable+'\n'+shim);path.chmod(0o700)
    for part in ('data/runtime','data/tasks','data/control','data/manifests','data/home','etc','net/lo','cgroup'):
        (root/part).mkdir(parents=True,exist_ok=True)
    for name in ('subuid','subgid'):
        (root/'etc'/name).write_text('agentowl-runner:100000:65536\n')
    (root/'data/runtime.ext4').touch()
    (root/'proc-mounts').write_text('overlay / overlay ro,nosuid,nodev 0 0\n')
    for name,value in [('memory.max','67108864'),('pids.max','32'),('cpu.max','25000 100000')]:
        (root/'cgroup'/name).write_text(value+'\n')
    socket_path=root/'data/control/docker.sock'
    socket_path.touch()
    try:
        setup=(repository/'script/runner-setup.sh').read_text()
        # Redirect privileged host paths and root identity only in the fixture.
        setup=setup.replace('$EUID == 0','1 == 1').replace('/etc/',str(root/'etc')+'/')
        setup=setup.replace('/sys/fs/cgroup/cgroup.controllers',str(root/'cgroup.controllers'))
        (root/'setup.sh').write_text(setup)
        preflight=(repository/'script/runner-preflight.sh').read_text()
        # A regular fixture file stands in for the Unix socket; no socket is opened.
        assert '[[ -S $socket ]]' in preflight
        (root/'preflight.sh').write_text(preflight.replace('[[ -S $socket ]]','[[ -f $socket ]]'))
        environment=dict(os.environ,PATH=str(commands)+os.pathsep+os.environ['PATH'],FIXTURE_ROOT=str(root))
        image='toolchain@sha256:'+'a'*64
        def invoke(script,mode=None,case='',success=True,extra=()):
            (root/'calls').write_text('')
            command=['bash',str(root/(script+'.sh')),'--data-root',str(root/'data')]
            if script=='preflight':command+=['--image',image]
            if mode is not None:command+=['--resource-limits',mode]
            result=subprocess.run(command+list(extra),env=dict(environment,FIXTURE_CASE=case),
                                  text=True,stdout=subprocess.PIPE,stderr=subprocess.STDOUT)
            assert (result.returncode==0)==success,(script,mode,case,result.stdout)
            calls=[json.loads(line) for line in (root/'calls').read_text().splitlines()]
            return result.stdout,calls
        for script in ('setup','preflight'):
            output,calls=invoke(script,'unexpected',success=False)
            assert 'required or disabled' in output and not calls
            invoke(script,success=False,extra=('--resource-limits',))
        # Setup defaults to strict even when the caller exports a disabled mode.
        environment['RUNNER_RESOURCE_LIMITS']='disabled'
        output,calls=invoke('setup',success=False)
        assert 'cgroup v2 is required' in output
        output,calls=invoke('setup','disabled')
        assert 'WARNING' in output and 'Web task execution resource limits: disabled' in output
        assert '--resource-limits "disabled"' in output
        assert not (root/'etc/systemd').exists()
        assert not any(call[:2]==['systemctl','daemon-reload'] for call in calls)
        config=json.loads((root/'data/home/.config/agentowl/daemon.json').read_text())
        assert config['data-root']==str(root/'data/runtime')
        assert config['hosts']==['unix://'+str(root/'data/control/docker.sock')]
        unit=(root/'data/home/.config/systemd/user/agentowl-docker.service').read_text()
        assert str(commands/'dockerd-rootless.sh') in unit
        assert '--config-file="'+str(root/'data/home/.config/agentowl/daemon.json')+'"' in unit
        (root/'cgroup.controllers').touch()
        output,calls=invoke('setup')
        assert 'Web task execution resource limits: required' in output and 'WARNING' not in output
        assert 'Delegate=cpu cpuset io memory pids' in (root/'etc/systemd/system/user@1001.service.d/agentowl-delegate.conf').read_text()
        assert not any('restart' in call or 'reboot' in call for call in calls)
        # Strict mode enforces v2 and the resource capability flags.
        for case in ('v1','no-cpu'):
            output,calls=invoke('preflight',case=case,success=False)
            assert not any(call[0]=='docker' and 'run' in call for call in calls)
        for mode,case in ((None,''),('required',''),('disabled','v1'),('disabled','')):
            output,calls=invoke('preflight',mode,case)
            run=next(call for call in calls if call[0]=='docker' and call[3]=='run')
            for option in ('--memory','--memory-swap','--cpus','--pids-limit'):
                assert (option in run)==(mode!='disabled'),run
            for flag,value in (('--network','none'),('--user','65532:65532'),
                               ('--cap-drop','ALL'),('--security-opt','no-new-privileges:true')):
                assert run[run.index(flag)+1]==value
            assert '--read-only' in run and '--rm' in run
            assert ('/sys/fs/cgroup/' in run[-1])==(mode!='disabled')
            assert ('actual cgroup limits verified' in output)==(mode!='disabled')
            assert ('WARNING' in output)==(mode=='disabled')
        # Both modes keep every non-cgroup boundary and execute the isolation probe.
        for mode in ('required','disabled'):
            for case in ('rootful','wrong-root','unbounded-scratch','wrong-runtime','shared-runtime','no-reserve','root-user'):
                invoke('preflight',mode,case,success=False)
            (root/'proc-mounts').write_text('overlay / overlay rw 0 0\n')
            invoke('preflight',mode,success=False)
            (root/'proc-mounts').write_text('overlay / overlay ro 0 0\n')
            (root/'net/eth0').mkdir()
            invoke('preflight',mode,success=False)
            (root/'net/eth0').rmdir()
            for name in ('socket-leak','control-socket-leak'):
                (root/name).touch()
                invoke('preflight',mode,success=False)
                (root/name).unlink()
        (root/'cgroup/memory.max').write_text('max\n')
        invoke('preflight','required',success=False)
        invoke('preflight','disabled')
        socket_path.unlink()
        output,calls=invoke('preflight','disabled',success=False)
        assert 'socket is unavailable' in output and not any(call[0]=='docker' for call in calls)
    finally:
        socket_path.unlink(missing_ok=True)
print('setup/preflight defaults, explicit resource modes and retained isolation boundaries passed with local command fixtures')
`
	command := exec.Command(python, "-c", program, repo)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("runner script resource modes: %v\n%s", err, out)
	} else {
		t.Log(string(out))
	}
}
