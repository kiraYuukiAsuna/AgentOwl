#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
runner_root="$script_dir/../deploy/data/runner"
image=${RUNNER_TEST_IMAGE:-}
resource_limits=required
while (($#)); do
  case "$1" in
    --data-root) runner_root=${2:?missing path}; shift 2 ;;
    --image) image=${2:?missing image}; shift 2 ;;
    --resource-limits) resource_limits=${2:?missing resource limits mode}; shift 2 ;;
    --help|-h) printf '%s\n' 'Usage: runner-preflight.sh --data-root ABSOLUTE_PATH --image repository@sha256:DIGEST [--resource-limits required|disabled]' 'Resource limits default to required. Disabled is for development only: CPU, memory and PID usage are unlimited.'; exit 0 ;;
    *) echo 'Unknown argument.' >&2; exit 2 ;;
  esac
done
[[ $resource_limits == required || $resource_limits == disabled ]] || { echo 'Resource limits must be required or disabled.' >&2; exit 2; }
[[ $(uname -s) == Linux ]] || { echo 'Runner requires Linux.' >&2; exit 1; }
if [[ $resource_limits == disabled ]]; then
  echo 'WARNING: CPU, memory and PID limits are disabled. A task can exhaust host resources; use only for development.' >&2
fi
[[ $image =~ @sha256:[a-f0-9]{64}$ ]] || { echo 'Pass an approved digest-pinned toolchain image via --image.' >&2; exit 1; }
for utility in docker python3 findmnt; do command -v "$utility" >/dev/null || { echo "Missing $utility" >&2; exit 1; }; done
runner_root=$(cd -- "$runner_root" && pwd -P)
socket="$runner_root/control/docker.sock"
[[ -S $socket ]] || { echo 'Dedicated runtime socket is unavailable.' >&2; exit 1; }
info=$(docker --host "unix://$socket" info --format '{{json .}}')
printf '%s' "$info" | python3 -c '
import json, sys
info = json.load(sys.stdin)
assert any("rootless" in x for x in info.get("SecurityOptions", [])), "daemon is not rootless"
assert info.get("DockerRootDir") == sys.argv[1] + "/runtime", "wrong daemon data-root"
if sys.argv[2] == "required":
    assert info.get("CgroupVersion") == "2" and info.get("CgroupDriver") == "systemd", "cgroup v2/systemd required"
    for feature in ("MemoryLimit", "PidsLimit", "CpuCfsQuota"):
        assert info.get(feature) is True, feature + " is not enforced"
' "$runner_root" "$resource_limits"
findmnt --json --target "$runner_root/tasks" | python3 -c '
import json, sys
m=json.load(sys.stdin)["filesystems"][0]
assert m["fstype"] == "tmpfs", "scratch must be tmpfs"
assert "size=" in m["options"] and "nr_inodes=" in m["options"], "scratch capacity limits missing"
'
findmnt --json --target "$runner_root/runtime" | python3 -c '
import json, sys
m=json.load(sys.stdin)["filesystems"][0]
assert m["fstype"] in ("ext4", "xfs") and m["source"].startswith("/dev/"), "runtime must be a dedicated bounded block filesystem"
'
python3 - "$runner_root" <<'PY'
import os, sys
root=sys.argv[1]
assert os.stat(root).st_dev != os.stat(root+'/runtime').st_dev, 'runtime shares controller filesystem'
for sub in ('tasks','runtime','manifests'):
    v=os.statvfs(root+'/'+sub)
    assert v.f_bavail*v.f_frsize >= 256*1024*1024 and v.f_favail >= 1024, 'insufficient runner storage reserve'
PY
docker --host "unix://$socket" image inspect "$image" >/dev/null
# This fixed probe checks isolation inside the approved image, not repository code.
docker_args=(run --rm --network none --read-only --user 65532:65532
  --cap-drop ALL --security-opt no-new-privileges:true)
probe='
    test "$(id -u)" != 0
    for interface in /sys/class/net/*; do test "${interface##*/}" = lo; done
    root_readonly=false
    while read -r source target type options rest; do
      if [ "$target" = / ]; then
        case ",$options," in *,ro,*) root_readonly=true ;; esac
        break
      fi
    done < /proc/mounts
    test "$root_readonly" = true
    test ! -e /var/run/docker.sock
    test ! -e /runner/control/docker.sock
  '
if [[ $resource_limits == required ]]; then
  docker_args+=(--memory 64m --memory-swap 64m --cpus 0.25 --pids-limit 32)
  probe+='
    test "$(cat /sys/fs/cgroup/memory.max)" = 67108864
    test "$(cat /sys/fs/cgroup/pids.max)" = 32
    set -- $(cat /sys/fs/cgroup/cpu.max)
    test "$1" != max
    test "$1" -gt 0
    test "$1" -lt "$2"
  '
fi
docker --host "unix://$socket" "${docker_args[@]}" --entrypoint /bin/sh "$image" -ec "$probe"
if [[ $resource_limits == required ]]; then
  echo 'Runner rootless runtime, filesystem bounds, container isolation and actual cgroup limits verified.'
else
  echo 'Runner rootless runtime, filesystem bounds and container isolation verified. CPU, memory and PID limits are disabled.'
fi
