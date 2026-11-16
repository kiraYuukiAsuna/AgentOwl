#!/usr/bin/env bash
set -euo pipefail

# Run explicitly on the Linux deployment host; never invoked by application startup.
usage() {
  cat <<'USAGE'
Usage: sudo script/runner-setup.sh [--data-root ABSOLUTE_PATH] [--user agentowl-runner]
                                [--runtime-gb 8] [--scratch-mb 1024]
                                [--resource-limits required|disabled]
Requires rootless Docker tools, systemd, uidmap, e2fsprogs and Python 3.
Resource limits default to required and need cgroup v2. The explicit disabled
development mode leaves CPU, memory and PID usage unlimited.
Creates a dedicated account, a preallocated bounded runtime filesystem and bounded
scratch tmpfs. Does not install packages, pull images, or modify the main Docker daemon.
USAGE
}

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
runner_root="$script_dir/../deploy/data/runner"
runner_user=agentowl-runner
runtime_gb=8
scratch_mb=1024
resource_limits=required
while (($#)); do
  case "$1" in
    --data-root) runner_root=${2:?missing path}; shift 2 ;;
    --user) runner_user=${2:?missing user}; shift 2 ;;
    --runtime-gb) runtime_gb=${2:?missing size}; shift 2 ;;
    --scratch-mb) scratch_mb=${2:?missing size}; shift 2 ;;
    --resource-limits) resource_limits=${2:?missing resource limits mode}; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done
[[ $resource_limits == required || $resource_limits == disabled ]] || { echo 'Resource limits must be required or disabled.' >&2; exit 2; }
[[ $(uname -s) == Linux && $EUID == 0 ]] || { echo 'Run explicitly as root on a Linux host.' >&2; exit 1; }
[[ $runner_user =~ ^[a-z][a-z0-9-]{1,30}$ && $runtime_gb =~ ^[0-9]+$ && $scratch_mb =~ ^[0-9]+$ ]] || { echo 'Invalid account or size.' >&2; exit 1; }
((runtime_gb >= 4 && runtime_gb <= 128 && scratch_mb >= 512 && scratch_mb <= 8192)) || { echo 'Sizes outside supported bounds.' >&2; exit 1; }
for utility in dockerd-rootless.sh newuidmap newgidmap useradd runuser loginctl systemctl fallocate mkfs.ext4 mount mountpoint findmnt python3; do
  command -v "$utility" >/dev/null || { echo "Missing prerequisite: $utility" >&2; exit 1; }
done
if [[ $resource_limits == required ]]; then
  [[ -f /sys/fs/cgroup/cgroup.controllers ]] || { echo 'cgroup v2 is required.' >&2; exit 1; }
else
  echo 'WARNING: CPU, memory and PID limits are disabled. A task can exhaust host resources; use only for development.' >&2
fi
mkdir -p -- "$runner_root"
runner_root=$(cd -- "$runner_root" && pwd -P)
[[ $runner_root == /* && $runner_root != *$'\n'* && $runner_root != *'"'* && $runner_root != *'%'* && $runner_root != *'\'* ]] || { echo 'Unsupported path characters.' >&2; exit 1; }
runner_home="$runner_root/home"
if id "$runner_user" >/dev/null 2>&1; then
  existing_home=$(getent passwd "$runner_user" | cut -d: -f6)
  [[ $existing_home == "$runner_home" ]] || { echo 'Existing account has a different home; refusing to repurpose it.' >&2; exit 1; }
else
  useradd --create-home --home-dir "$runner_home" --shell /bin/bash "$runner_user"
fi
runner_uid=$(id -u "$runner_user")
runner_gid=$(id -g "$runner_user")
python3 - "$runner_user" <<'PY'
import sys
user = sys.argv[1]
for name in ('/etc/subuid', '/etc/subgid'):
    ranges = [int(line.split(':')[2]) for line in open(name) if line.startswith(user + ':')]
    if not ranges or sum(ranges) < 65536:
        raise SystemExit(f'{name} must grant {user} at least 65536 subordinate IDs; configure explicitly before retrying')
PY

mkdir -p -- "$runner_root"/{runtime,tasks,control,manifests}
chmod 0711 "$runner_root"
runuser -u "$runner_user" -- test -x "$runner_root" || { echo 'Runner account cannot traverse the project parent directories. Relocate the project to an accessible deployment directory before retrying.' >&2; exit 1; }
image="$runner_root/runtime.ext4"
if [[ ! -e $image ]]; then
  available_kb=$(df -Pk "$runner_root" | awk 'NR==2 {print $4}')
  required_kb=$((runtime_gb * 1024 * 1024 + 2 * 1024 * 1024))
  ((available_kb > required_kb)) || { echo 'Runtime allocation would consume the host disk reserve.' >&2; exit 1; }
  fallocate -l "${runtime_gb}G" "$image"
  chmod 0600 "$image"
  mkfs.ext4 -q -F -E nodiscard -N 262144 "$image"
elif [[ ! -f $image || -L $image ]]; then
  echo 'Runtime backing path is not a regular file.' >&2; exit 1
fi

fstab_path() { python3 -c 'import sys; print(sys.argv[1].replace(" ", "\\040").replace("\t", "\\011"))' "$1"; }
runtime_mount=$(fstab_path "$runner_root/runtime")
scratch_mount=$(fstab_path "$runner_root/tasks")
image_path=$(fstab_path "$image")
if ! findmnt --fstab --mountpoint "$runner_root/runtime" --noheadings >/dev/null 2>&1; then
  printf '%s %s ext4 loop,nosuid,nodev 0 0\n' "$image_path" "$runtime_mount" >> /etc/fstab
fi
if ! findmnt --fstab --mountpoint "$runner_root/tasks" --noheadings >/dev/null 2>&1; then
  printf 'tmpfs %s tmpfs rw,nosuid,nodev,size=%sM,nr_inodes=100000,mode=0755,uid=%s,gid=%s 0 0\n' "$scratch_mount" "$scratch_mb" "$runner_uid" "$runner_gid" >> /etc/fstab
fi
mountpoint -q "$runner_root/runtime" || mount "$runner_root/runtime"
mountpoint -q "$runner_root/tasks" || mount "$runner_root/tasks"
chown "$runner_uid:$runner_gid" "$runner_root/runtime" "$runner_root/tasks" "$runner_root/control" "$runner_root/manifests"
chmod 0700 "$runner_root/control" "$runner_root/manifests"

mkdir -p -- "$runner_home/.config/agentowl" "$runner_home/.config/systemd/user"
daemon_config="$runner_home/.config/agentowl/daemon.json"
python3 - "$daemon_config" "$runner_root" <<'PY'
import json, sys
name, root = sys.argv[1:]
with open(name, 'w') as f:
    json.dump({'data-root': root + '/runtime', 'hosts': ['unix://' + root + '/control/docker.sock'],
               'log-driver': 'json-file', 'log-opts': {'max-size': '2m', 'max-file': '2'}}, f, indent=2)
PY
daemon_bin=$(command -v dockerd-rootless.sh)
cat > "$runner_home/.config/systemd/user/agentowl-docker.service" <<EOF
[Unit]
Description=AgentOwl dedicated rootless Docker runtime
After=default.target
[Service]
Type=simple
ExecStartPre=/usr/bin/rm -f "$runner_root/control/docker.sock"
ExecStart=$daemon_bin --config-file="$daemon_config"
Restart=on-failure
RestartSec=3
Delegate=yes
LimitNOFILE=65536
TasksMax=infinity
KillMode=mixed
[Install]
WantedBy=default.target
EOF
chown -R "$runner_uid:$runner_gid" "$runner_home/.config"
chmod 0600 "$daemon_config"
if [[ $resource_limits == required ]]; then
  mkdir -p "/etc/systemd/system/user@${runner_uid}.service.d"
  cat > "/etc/systemd/system/user@${runner_uid}.service.d/agentowl-delegate.conf" <<'EOF'
[Service]
Delegate=cpu cpuset io memory pids
EOF
  systemctl daemon-reload
fi
loginctl enable-linger "$runner_user"
systemctl start "user@${runner_uid}.service"
runuser -u "$runner_user" -- env "XDG_RUNTIME_DIR=/run/user/$runner_uid" "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$runner_uid/bus" systemctl --user daemon-reload
runuser -u "$runner_user" -- env "XDG_RUNTIME_DIR=/run/user/$runner_uid" "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$runner_uid/bus" systemctl --user enable --now agentowl-docker.service

cat <<EOF
Dedicated runtime configured. Set deployment values:
RUNNER_UID=$runner_uid
RUNNER_GID=$runner_gid
RUNNER_HOST_DATA_ROOT=$runner_root
RUNNER_DOCKER_SOCKET=/runner/control/docker.sock
Web task execution resource limits: $resource_limits
Save this mode in Settings > Task execution before starting tasks.
Before enabling execution, pull an approved digest-pinned toolchain image into the
dedicated daemon and run script/runner-preflight.sh --data-root "$runner_root" --resource-limits "$resource_limits".
The main Docker daemon was not modified. Startup remains blocked until preflight passes.
EOF
