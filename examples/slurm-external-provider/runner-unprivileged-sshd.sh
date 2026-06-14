#!/usr/bin/env bash
set -euo pipefail

: "${CBX_LEASE_ID:?missing CBX_LEASE_ID}"
: "${CBX_STATE_DIR:?missing CBX_STATE_DIR}"
: "${CBX_WORK_ROOT:?missing CBX_WORK_ROOT}"
: "${CBX_SSH_PUBLIC_KEY_FILE:?missing CBX_SSH_PUBLIC_KEY_FILE}"

job_dir="${CBX_STATE_DIR}/jobs/${CBX_LEASE_ID}"
sshd_dir="${job_dir}/sshd"
work_dir="${CBX_WORK_ROOT}/${CBX_LEASE_ID}"
endpoint_file="${job_dir}/endpoint.json"
endpoint_tmp="${endpoint_file}.tmp"
authorized_keys="${sshd_dir}/authorized_keys"
host_key="${sshd_dir}/ssh_host_ed25519_key"
config_file="${sshd_dir}/sshd_config"
pid_file="${sshd_dir}/sshd.pid"
log_file="${job_dir}/sshd.log"

mkdir -p "${sshd_dir}" "${work_dir}"
chmod 700 "${job_dir}" "${sshd_dir}" || true

cp "${CBX_SSH_PUBLIC_KEY_FILE}" "${authorized_keys}"
chmod 600 "${authorized_keys}" || true

if [ ! -f "${host_key}" ]; then
  ssh-keygen -q -t ed25519 -N "" -f "${host_key}" -C "crabbox-${CBX_LEASE_ID}-host"
fi
chmod 600 "${host_key}" || true

if [ -n "${CBX_SSH_PORT:-}" ]; then
  port="${CBX_SSH_PORT}"
else
  port="$(python3 - <<'PY'
import random
import socket

for _ in range(200):
    candidate = random.randint(39000, 49999)
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        try:
            sock.bind(("", candidate))
        except OSError:
            continue
    print(candidate)
    break
else:
    raise SystemExit("could not find a free high port")
PY
)"
fi

sshd_path="${CBX_SSHD:-}"
if [ -z "${sshd_path}" ]; then
  sshd_path="$(command -v sshd || true)"
fi
if [ -z "${sshd_path}" ]; then
  echo "sshd not found; install OpenSSH server or replace this runner" >&2
  exit 127
fi

cat >"${config_file}" <<EOF
Port ${port}
ListenAddress 0.0.0.0
HostKey ${host_key}
PidFile ${pid_file}
AuthorizedKeysFile ${authorized_keys}
PasswordAuthentication no
KbdInteractiveAuthentication no
ChallengeResponseAuthentication no
UsePAM no
PermitRootLogin no
AllowTcpForwarding yes
X11Forwarding no
PermitTunnel no
PermitUserEnvironment no
StrictModes no
LogLevel VERBOSE
Subsystem sftp internal-sftp
EOF

host="$(hostname -f 2>/dev/null || hostname)"
user="${CBX_SSH_USER:-${USER:-}}"
ready_check="${CBX_READY_CHECK:-command -v bash && command -v python3 && command -v git && command -v rsync && command -v tar}"

python3 - "${endpoint_tmp}" "${endpoint_file}" "${host}" "${port}" "${user}" "${ready_check}" <<'PY'
import json
import os
import sys

tmp, final, host, port, user, ready_check = sys.argv[1:7]
payload = {
    "host": host,
    "port": port,
    "user": user,
    "readyCheck": ready_check,
}
with open(tmp, "w", encoding="utf-8") as handle:
    json.dump(payload, handle, separators=(",", ":"))
    handle.write("\n")
os.replace(tmp, final)
PY

cleanup() {
  rm -f "${endpoint_file}" "${endpoint_tmp}" "${pid_file}"
}
trap cleanup EXIT INT TERM

echo "crabbox Slurm SSH endpoint ready host=${host} port=${port} user=${user}" >&2
exec "${sshd_path}" -D -e -f "${config_file}" >>"${log_file}" 2>&1
