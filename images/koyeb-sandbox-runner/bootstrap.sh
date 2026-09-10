#!/usr/bin/env bash
set -euo pipefail
set +x
umask 077

export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

state_root="${CRABBOX_KOYEB_STATE_ROOT:-/var/lib/crabbox-koyeb}"
runtime_root="${CRABBOX_KOYEB_RUNTIME_ROOT:-/run/crabbox-koyeb}"
tailscale_socket="${runtime_root}/tailscaled.sock"
desktop_runtime="${runtime_root}/user"
display=:99
geometry=1920x1080x24
ssh_user=crabbox
network="${CRABBOX_KOYEB_NETWORK:-tailscale}"

log() {
  printf 'crabbox-koyeb-bootstrap: %s\n' "$*" >&2
}

fail() {
  log "$*"
  exit 2
}

: "${CRABBOX_KOYEB_LEASE_ID:?missing CRABBOX_KOYEB_LEASE_ID}"
: "${CRABBOX_KOYEB_SSH_PUBLIC_KEY_FILE:?missing CRABBOX_KOYEB_SSH_PUBLIC_KEY_FILE}"

lease_id="$CRABBOX_KOYEB_LEASE_ID"
public_key_file="$CRABBOX_KOYEB_SSH_PUBLIC_KEY_FILE"
tailscale_auth_key=""
tailscale_hostname=""
tailscale_tags=""
tailscale_login_server=""
private_host=""
case "$network" in
  tailscale)
    : "${CRABBOX_KOYEB_TAILSCALE_AUTH_KEY:?missing CRABBOX_KOYEB_TAILSCALE_AUTH_KEY}"
    : "${CRABBOX_KOYEB_TAILSCALE_HOSTNAME:?missing CRABBOX_KOYEB_TAILSCALE_HOSTNAME}"
    : "${CRABBOX_KOYEB_TAILSCALE_TAGS:?missing CRABBOX_KOYEB_TAILSCALE_TAGS}"
    tailscale_auth_key="$CRABBOX_KOYEB_TAILSCALE_AUTH_KEY"
    tailscale_hostname="$CRABBOX_KOYEB_TAILSCALE_HOSTNAME"
    tailscale_tags="$CRABBOX_KOYEB_TAILSCALE_TAGS"
    tailscale_login_server="${CRABBOX_KOYEB_TAILSCALE_LOGIN_SERVER:-}"
    ;;
  koyeb-mesh)
    : "${CRABBOX_KOYEB_PRIVATE_HOST:?missing CRABBOX_KOYEB_PRIVATE_HOST}"
    private_host="$CRABBOX_KOYEB_PRIVATE_HOST"
    ;;
  *) fail "invalid Koyeb network transport" ;;
esac
unset CRABBOX_KOYEB_TAILSCALE_AUTH_KEY CRABBOX_KOYEB_PRIVATE_HOST
unset SANDBOX_SECRET KOYEB_API_TOKEN

[[ "$(id -u)" -eq 0 ]] || fail "bootstrap must run as root through the Sandbox executor"
[[ "$(uname -m)" == x86_64 ]] || fail "runner image supports linux/amd64 only"
[[ "$lease_id" =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]] || fail "invalid lease id"
if [[ "$network" == tailscale ]]; then
  [[ "$tailscale_hostname" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || fail "invalid Tailscale hostname"
  [[ -n "$tailscale_auth_key" && "${#tailscale_auth_key}" -le 4096 ]] || fail "invalid Tailscale auth key"
  [[ "$tailscale_auth_key" != *$'\n'* && "$tailscale_auth_key" != *$'\r'* ]] || fail "invalid Tailscale auth key"
  if [[ -n "$tailscale_login_server" ]]; then
    [[ "$tailscale_login_server" =~ ^https://[^[:space:]]+$ ]] || fail "invalid Tailscale login server"
  fi
  IFS=',' read -r -a requested_tags <<<"$tailscale_tags"
  [[ "${#requested_tags[@]}" -gt 0 ]] || fail "at least one Tailscale tag is required"
  for tag in "${requested_tags[@]}"; do
    [[ "$tag" =~ ^tag:[a-z0-9][a-z0-9-]{0,62}$ ]] || fail "invalid Tailscale tag"
  done
else
  [[ "$private_host" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.internal$ ]] || fail "invalid Koyeb private host"
fi

[[ -f "$public_key_file" && ! -L "$public_key_file" ]] || fail "SSH public key must be a regular non-symlink file"
[[ "$(stat -c %s -- "$public_key_file")" -le 16384 ]] || fail "SSH public key file is too large"
[[ "$(awk 'NF { count += 1 } END { print count + 0 }' "$public_key_file")" -eq 1 ]] || fail "SSH public key file must contain exactly one key"
/usr/bin/ssh-keygen -l -f "$public_key_file" >/dev/null 2>&1 || fail "invalid SSH public key"

for required in \
  /usr/sbin/sshd \
  /usr/bin/Xvfb \
  /usr/bin/x11vnc \
  /usr/bin/setpriv \
  /usr/bin/tigervncpasswd \
  /usr/bin/python3 \
  /usr/bin/pip3 \
  /usr/bin/git \
  /usr/bin/make \
  /usr/local/bin/node \
  /usr/local/bin/code-server \
  /usr/local/bin/crabbox-worker-browser \
  /usr/local/bin/crabbox-worker-terminal \
  /usr/bin/google-chrome-stable; do
  [[ -x "$required" ]] || fail "missing image dependency: $required"
done
if [[ "$network" == tailscale ]]; then
  for required in /usr/local/bin/tailscale /usr/local/sbin/tailscaled; do
    [[ -x "$required" ]] || fail "missing image dependency: $required"
  done
fi

for managed_dir in "$state_root" "$runtime_root"; do
  if [[ -L "$managed_dir" || ( -e "$managed_dir" && ! -d "$managed_dir" ) ]]; then
    fail "unsafe managed directory: $managed_dir"
  fi
done
install -d -m 0700 -o root -g root "$state_root"
# The unprivileged desktop owns a private child of this root-owned directory.
# Execute-only access permits traversal without exposing root-owned runtime data.
install -d -m 0711 -o root -g root "$runtime_root"
exec 9>"${runtime_root}/operation.lock"
flock -w 30 9 || fail "timed out waiting for runner operation lock"

bootstrap_succeeded=false
cleanup() {
  status=$?
  unset tailscale_auth_key
  if [[ "$status" -ne 0 && "$bootstrap_succeeded" != true ]]; then
    CRABBOX_KOYEB_LOCK_HELD=1 CRABBOX_KOYEB_STATE_ROOT="$state_root" CRABBOX_KOYEB_RUNTIME_ROOT="$runtime_root" \
      /usr/local/libexec/crabbox-koyeb-sandbox/teardown.sh --bootstrap-failure >/dev/null 2>&1 || true
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

identity_file="${state_root}/lease-id"
if [[ -s "$identity_file" ]]; then
  existing_lease_id="$(cat "$identity_file")"
  [[ "$existing_lease_id" == "$lease_id" ]] || fail "runner belongs to a different lease"
  if CRABBOX_KOYEB_STATE_ROOT="$state_root" CRABBOX_KOYEB_RUNTIME_ROOT="$runtime_root" \
      /usr/local/libexec/crabbox-koyeb-sandbox/healthcheck.sh >/dev/null 2>&1; then
    unset tailscale_auth_key
    bootstrap_succeeded=true
    cat "${state_root}/crabbox-ready.json"
    exit 0
  fi
  CRABBOX_KOYEB_LOCK_HELD=1 CRABBOX_KOYEB_STATE_ROOT="$state_root" CRABBOX_KOYEB_RUNTIME_ROOT="$runtime_root" \
    /usr/local/libexec/crabbox-koyeb-sandbox/teardown.sh --bootstrap-failure >/dev/null 2>&1 || true
fi
printf '%s\n' "$lease_id" >"$identity_file"
chmod 0600 "$identity_file"

sshd_dir="${state_root}/sshd"
authorized_keys_file=/var/lib/crabbox/authorized_keys
install -d -m 0700 -o root -g root "$sshd_dir"
install -d -m 0755 -o root -g root /run/sshd
install -m 0640 -o root -g "$ssh_user" "$public_key_file" "$authorized_keys_file"
if [[ ! -s "${sshd_dir}/ssh_host_ed25519_key" ]]; then
  /usr/bin/ssh-keygen -q -t ed25519 -N '' -f "${sshd_dir}/ssh_host_ed25519_key"
fi
chmod 0600 "${sshd_dir}/ssh_host_ed25519_key"

cat >"${sshd_dir}/sshd_config" <<EOF
Port 22
AddressFamily inet
HostKey ${sshd_dir}/ssh_host_ed25519_key
PidFile ${runtime_root}/sshd.internal.pid
AuthorizedKeysFile ${authorized_keys_file}
AllowUsers ${ssh_user}
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
ChallengeResponseAuthentication no
PermitEmptyPasswords no
PermitRootLogin no
UsePAM no
AllowAgentForwarding no
AllowTcpForwarding local
GatewayPorts no
X11Forwarding no
PermitTunnel no
PermitUserEnvironment no
StrictModes yes
LogLevel VERBOSE
Subsystem sftp internal-sftp
EOF
sed -i '3iListenAddress 127.0.0.1' "${sshd_dir}/sshd_config"
/usr/sbin/sshd -t -f "${sshd_dir}/sshd_config"

install -d -m 0755 -o "$ssh_user" -g "$ssh_user" /workspace/crabbox
install -d -m 0700 -o "$ssh_user" -g "$ssh_user" "$desktop_runtime"
install -d -m 0750 -o root -g "$ssh_user" /var/lib/crabbox
printf '%s\n' "$lease_id" >/var/lib/crabbox/lease-id
chown root:"$ssh_user" /var/lib/crabbox/lease-id
chmod 0640 /var/lib/crabbox/lease-id
vnc_password_file=/var/lib/crabbox/vnc.password
vnc_auth_file=/var/lib/crabbox/vnc.pass
if [[ ! -s "$vnc_password_file" ]]; then
  openssl rand -base64 18 >"$vnc_password_file"
fi
head -c 8 "$vnc_password_file" | /usr/bin/tigervncpasswd -f >"$vnc_auth_file"
chown "$ssh_user:$ssh_user" "$vnc_password_file" "$vnc_auth_file"
chmod 0600 "$vnc_password_file" "$vnc_auth_file"
printf 'CRABBOX_DESKTOP_ENV=xfce\nDISPLAY=%s\n' "$display" >/var/lib/crabbox/desktop.env
printf 'CHROME_BIN=/usr/local/bin/crabbox-browser\nBROWSER=/usr/local/bin/crabbox-browser\n' >/var/lib/crabbox/browser.env
chown "$ssh_user:$ssh_user" /var/lib/crabbox/desktop.env /var/lib/crabbox/browser.env
chmod 0644 /var/lib/crabbox/desktop.env /var/lib/crabbox/browser.env

start_owned() {
  local name="$1"
  local expected="$2"
  shift 2
  env -i PATH="$PATH" HOME=/root "$@" </dev/null >>"${state_root}/${name}.log" 2>&1 &
  local pid=$!
  printf '%s\n' "$pid" >"${runtime_root}/${name}.pid"
  sleep 0.2
  kill -0 "$pid" 2>/dev/null || fail "$name failed to start"
  local cmdline
  cmdline="$(tr '\0' ' ' <"/proc/${pid}/cmdline" 2>/dev/null || true)"
  [[ "$cmdline" == *"$expected"* ]] || fail "$name ownership check failed"
}

tailscale_ip=""
tailscale_status=""
if [[ "$network" == tailscale ]]; then
  start_owned tailscaled "--socket=${tailscale_socket}" \
    setsid /usr/local/sbin/tailscaled \
      --tun=userspace-networking \
      --state=mem: \
      --socket="$tailscale_socket" \
      --no-logs-no-support
  for _ in $(seq 1 60); do
    [[ -S "$tailscale_socket" ]] && break
    sleep 0.25
  done
  [[ -S "$tailscale_socket" ]] || fail "tailscaled socket did not become ready"

  tailscale_up_args=(
    --accept-dns=false
    --auth-key="file:/dev/stdin"
    --hostname="$tailscale_hostname"
    --advertise-tags="$tailscale_tags"
    --timeout=120s
  )
  if [[ -n "$tailscale_login_server" ]]; then
    tailscale_up_args+=(--login-server="$tailscale_login_server")
  fi
  printf '%s' "$tailscale_auth_key" | tailscale --socket="$tailscale_socket" up "${tailscale_up_args[@]}"
  unset tailscale_auth_key

  for _ in $(seq 1 60); do
    tailscale_status="$(tailscale --socket="$tailscale_socket" status --json 2>/dev/null || true)"
    if jq -e '.BackendState == "Running"' >/dev/null 2>&1 <<<"$tailscale_status"; then
      tailscale_ip="$(tailscale --socket="$tailscale_socket" ip -4 2>/dev/null | head -n1 || true)"
      [[ -n "$tailscale_ip" ]] && break
    fi
    sleep 1
  done
  [[ -n "$tailscale_ip" ]] || fail "Tailscale did not reach Running state"
fi

start_owned sshd "${sshd_dir}/sshd_config" \
  setsid /usr/sbin/sshd -D -e -f "${sshd_dir}/sshd_config"
start_owned xvfb "$display" \
  setsid /usr/bin/Xvfb "$display" -screen 0 "$geometry" -nolisten tcp -ac
for _ in $(seq 1 60); do
  [[ -S "/tmp/.X11-unix/X${display#:}" ]] && break
  sleep 0.25
done
[[ -S "/tmp/.X11-unix/X${display#:}" ]] || fail "Xvfb did not become ready"

start_owned desktop "desktop-session.sh" \
  setsid /usr/bin/setpriv --reuid="$ssh_user" --regid="$ssh_user" --init-groups \
    /usr/bin/env -i PATH="$PATH" HOME="/home/${ssh_user}" USER="$ssh_user" LOGNAME="$ssh_user" \
      DISPLAY="$display" XDG_RUNTIME_DIR="$desktop_runtime" CRABBOX_KOYEB_SESSION_LOG_ROOT="$desktop_runtime" \
      /usr/bin/dbus-run-session -- /usr/local/libexec/crabbox-koyeb-sandbox/desktop-session.sh
start_owned x11vnc "-rfbport 5900" \
  setsid /usr/bin/setpriv --reuid="$ssh_user" --regid="$ssh_user" --init-groups \
    /usr/bin/env -i PATH="$PATH" HOME="/home/${ssh_user}" USER="$ssh_user" LOGNAME="$ssh_user" DISPLAY="$display" \
      /usr/bin/x11vnc -display "$display" -localhost -rfbport 5900 -forever -shared \
        -rfbauth "$vnc_auth_file" -noshm -wait 16 -defer 8 -nowait_bog

for process_pattern in 'xfce4-terminal' 'google-chrome'; do
  process_ready=false
  for _ in $(seq 1 60); do
    if pgrep -u "$ssh_user" -f "$process_pattern" >/dev/null 2>&1; then
      process_ready=true
      break
    fi
    sleep 0.25
  done
  [[ "$process_ready" == true ]] || fail "$process_pattern did not become ready"
done

for port in 22 5900; do
  for _ in $(seq 1 60); do
    nc -z 127.0.0.1 "$port" >/dev/null 2>&1 && break
    sleep 0.25
  done
  nc -z 127.0.0.1 "$port" >/dev/null 2>&1 || fail "loopback port $port did not become ready"
done

host_public_key="$(cat "${sshd_dir}/ssh_host_ed25519_key.pub")"
ready_tmp="${state_root}/crabbox-ready.json.tmp"
if [[ "$network" == tailscale ]]; then
  tailscale --socket="$tailscale_socket" serve --tcp=22 tcp://127.0.0.1:22 --bg >/dev/null
  tailscale_dns_name="$(jq -r '.Self.DNSName // empty' <<<"$tailscale_status" | sed 's/\.$//')"
  jq -cn \
    --arg schema "crabbox-koyeb-sandbox-runner/v1" \
    --arg leaseId "$lease_id" \
    --arg sshUser "$ssh_user" \
    --arg sshHost "$tailscale_ip" \
    --arg sshHostKey "$host_public_key" \
    --arg tailscaleDNSName "$tailscale_dns_name" \
    '{schema:$schema,leaseId:$leaseId,ssh:{user:$sshUser,host:$sshHost,port:22,hostKey:$sshHostKey},tailscale:{ipv4:$sshHost,dnsName:$tailscaleDNSName},desktop:{display:":99",vncHost:"127.0.0.1",vncPort:5900,browser:"/usr/local/bin/crabbox-browser",terminal:"xfce4-terminal"}}' \
    >"$ready_tmp"
  printf '%s\n' "$tailscale_ip" >/var/lib/crabbox/tailscale-ipv4
  printf '%s\n' "$tailscale_hostname" >/var/lib/crabbox/tailscale-hostname
  printf '%s\n' "$tailscale_dns_name" >/var/lib/crabbox/tailscale-fqdn
  chmod 0644 /var/lib/crabbox/tailscale-ipv4 /var/lib/crabbox/tailscale-hostname /var/lib/crabbox/tailscale-fqdn
else
  jq -cn \
    --arg schema "crabbox-koyeb-sandbox-runner/v2" \
    --arg leaseId "$lease_id" \
    --arg sshUser "$ssh_user" \
    --arg sshHost "$private_host" \
    --arg sshHostKey "$host_public_key" \
    --arg transport "koyeb-mesh" \
    '{schema:$schema,leaseId:$leaseId,ssh:{user:$sshUser,host:$sshHost,port:22,hostKey:$sshHostKey},network:{transport:$transport,privateHost:$sshHost},desktop:{display:":99",vncHost:"127.0.0.1",vncPort:5900,browser:"/usr/local/bin/crabbox-browser",terminal:"xfce4-terminal"}}' \
    >"$ready_tmp"
fi
chmod 0644 "$ready_tmp"
mv -fT -- "$ready_tmp" "${state_root}/crabbox-ready.json"

CRABBOX_KOYEB_STATE_ROOT="$state_root" CRABBOX_KOYEB_RUNTIME_ROOT="$runtime_root" \
  /usr/local/libexec/crabbox-koyeb-sandbox/healthcheck.sh >/dev/null
bootstrap_succeeded=true
cat "${state_root}/crabbox-ready.json"
