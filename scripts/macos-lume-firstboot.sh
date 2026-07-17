#!/bin/bash
set -euo pipefail

marker="/var/db/crabbox-lume-machine-id"
lease_user_marker="/var/db/crabbox-lume-ssh-user"
trust_mount="/Volumes/My Shared Files"
challenge_path="$trust_mount/challenge"
identity_path="$trust_mount/identity"
ssh_user_path="$trust_mount/ssh_user"
authorized_key_path="$trust_mount/authorized_key"
sshd_config_path="/etc/ssh/sshd_config.d/00-crabbox-lease.conf"
legacy_sshd_config_path="/etc/ssh/sshd_config.d/90-crabbox-lease.conf"
platform_uuid="$(/usr/sbin/ioreg -rd1 -c IOPlatformExpertDevice | /usr/bin/awk -F'"' '/IOPlatformUUID/ { print $(NF-1); exit }')"

if [[ -z "$platform_uuid" ]]; then
  echo "could not determine IOPlatformUUID" >&2
  exit 1
fi

identity_changed=false
if [[ ! -r "$marker" ]] || [[ "$(<"$marker")" != "$platform_uuid" ]]; then
  identity_changed=true
  echo "new Lume machine identity detected; rotating OpenSSH host keys"
  /bin/rm -f /etc/ssh/ssh_host_*
  /usr/bin/ssh-keygen -A
fi
expected_host_key="$(/usr/bin/awk '$1 == "ssh-ed25519" { print $2; exit }' /etc/ssh/ssh_host_ed25519_key.pub)"
if [[ -z "$expected_host_key" ]]; then
  echo "could not read the new OpenSSH ED25519 host key" >&2
  exit 1
fi

challenge_processed=false
challenge=""
sshd_config_changed=false
if [[ -r "$challenge_path" ]]; then
  if [[ ! -r "$ssh_user_path" ]] || [[ ! -r "$authorized_key_path" ]]; then
    echo "incomplete Crabbox bootstrap trust input" >&2
    exit 1
  fi
  ssh_user="$(<"$ssh_user_path")"
  if [[ ! "$ssh_user" =~ ^[a-zA-Z_][a-zA-Z0-9._-]*$ ]]; then
    echo "invalid Crabbox bootstrap SSH user" >&2
    exit 1
  fi
  ssh_home="$(/usr/bin/dscl . -read "/Users/$ssh_user" NFSHomeDirectory 2>/dev/null | /usr/bin/sed -n 's/^NFSHomeDirectory: //p')"
  if [[ "$ssh_home" != /* ]] || [[ "$ssh_home" == *$'\n'* ]] || [[ ! -d "$ssh_home" ]]; then
    echo "invalid Crabbox bootstrap SSH home" >&2
    exit 1
  fi
  if [[ "$(/usr/bin/wc -l <"$authorized_key_path" | /usr/bin/tr -d ' ')" != "1" ]] ||
    ! /usr/bin/awk '$1 == "ssh-ed25519" && $2 ~ /^[A-Za-z0-9+\/=]+$/ { valid=1 } END { exit !valid }' "$authorized_key_path"; then
    echo "invalid Crabbox bootstrap SSH public key" >&2
    exit 1
  fi
  challenge="$(<"$challenge_path")"
  if [[ ! "$challenge" =~ ^[A-Za-z0-9_-]{43}$ ]]; then
    echo "invalid Crabbox bootstrap trust challenge" >&2
    exit 1
  fi
  ssh_group="$(/usr/bin/id -gn "$ssh_user")"
  /usr/bin/install -d -o "$ssh_user" -g "$ssh_group" -m 0700 "$ssh_home/.ssh"
  /usr/bin/install -o "$ssh_user" -g "$ssh_group" -m 0600 \
    "$authorized_key_path" "$ssh_home/.ssh/authorized_keys"
  sshd_config_tmp="$(/usr/bin/mktemp /tmp/crabbox-lume-sshd.XXXXXX)"
  /usr/bin/printf '%s\n' \
    "AllowUsers $ssh_user" \
    'AuthenticationMethods publickey' \
    'PubkeyAuthentication yes' \
    'PasswordAuthentication no' \
    'KbdInteractiveAuthentication no' \
    'HostbasedAuthentication no' \
    'GSSAPIAuthentication no' \
    'PermitEmptyPasswords no' \
    'AuthorizedKeysCommand none' \
    'AuthorizedPrincipalsFile none' \
    'TrustedUserCAKeys none' \
    'AuthorizedKeysFile .ssh/authorized_keys' >"$sshd_config_tmp"
  challenge_processed=true
  sshd_config_changed=true
elif [[ "$identity_changed" == true ]] || [[ ! -f "$sshd_config_path" ]]; then
  # This deny-all configuration is baked into the stopped golden image. A
  # clone therefore cannot accept any SSH login while its VirtioFS bootstrap
  # share is mounting or if the share never appears.
  sshd_config_tmp="$(/usr/bin/mktemp /tmp/crabbox-lume-sshd.XXXXXX)"
  /usr/bin/printf '%s\n' \
    'AuthenticationMethods publickey' \
    'PubkeyAuthentication yes' \
    'PasswordAuthentication no' \
    'KbdInteractiveAuthentication no' \
    'HostbasedAuthentication no' \
    'GSSAPIAuthentication no' \
    'PermitEmptyPasswords no' \
    'AuthorizedKeysCommand none' \
    'AuthorizedPrincipalsFile none' \
    'TrustedUserCAKeys none' \
    'AuthorizedKeysFile none' >"$sshd_config_tmp"
  sshd_config_changed=true
fi
if [[ "$sshd_config_changed" == true ]]; then
  /usr/bin/install -o root -g wheel -m 0600 "$sshd_config_tmp" \
    "$sshd_config_path"
  /bin/rm -f "$sshd_config_tmp"
  /bin/rm -f "$legacy_sshd_config_path"
  /usr/sbin/sshd -t
fi

preserved_lease_user=""
if [[ "$challenge_processed" != true ]] && [[ "$sshd_config_changed" != true ]] && [[ -r "$lease_user_marker" ]]; then
  preserved_lease_user="$(<"$lease_user_marker")"
  if [[ ! "$preserved_lease_user" =~ ^[a-zA-Z_][a-zA-Z0-9._-]*$ ]]; then
    echo "invalid preserved Crabbox lease SSH user" >&2
    exit 1
  fi
fi
verify_user="${ssh_user:-${preserved_lease_user:-lume}}"
effective_sshd="$(/usr/sbin/sshd -T -C "user=$verify_user,host=localhost,addr=127.0.0.1")"
require_effective_sshd() {
  if ! /usr/bin/grep -Fqx "$1" <<<"$effective_sshd"; then
    echo "effective sshd policy does not enforce: $1" >&2
    exit 1
  fi
}
require_effective_sshd 'authenticationmethods publickey'
require_effective_sshd 'pubkeyauthentication yes'
require_effective_sshd 'passwordauthentication no'
require_effective_sshd 'kbdinteractiveauthentication no'
require_effective_sshd 'hostbasedauthentication no'
require_effective_sshd 'gssapiauthentication no'
require_effective_sshd 'permitemptypasswords no'
require_effective_sshd 'authorizedkeyscommand none'
require_effective_sshd 'authorizedprincipalsfile none'
require_effective_sshd 'trustedusercakeys none'
if [[ "$challenge_processed" == true ]] || [[ -n "$preserved_lease_user" ]]; then
  require_effective_sshd 'authorizedkeysfile .ssh/authorized_keys'
  effective_allowusers="$(/usr/bin/grep '^allowusers ' <<<"$effective_sshd" || true)"
  if [[ "$effective_allowusers" != "allowusers $verify_user" ]]; then
    echo "effective sshd policy does not allow only the lease user" >&2
    exit 1
  fi
else
  require_effective_sshd 'authorizedkeysfile none'
fi

if [[ "$challenge_processed" == true ]]; then
  lease_user_tmp="$(/usr/bin/mktemp "${lease_user_marker}.XXXXXX")"
  /usr/bin/printf '%s\n' "$ssh_user" >"$lease_user_tmp"
  /usr/sbin/chown root:wheel "$lease_user_tmp"
  /bin/chmod 0600 "$lease_user_tmp"
  /bin/mv -f "$lease_user_tmp" "$lease_user_marker"
elif [[ "$sshd_config_changed" == true ]]; then
  /bin/rm -f "$lease_user_marker"
fi

/bin/launchctl kickstart -k system/com.openssh.sshd >/dev/null

# Publish the clone identity only after sshd actually serves the new key.
# Crabbox treats the marker as the boundary between its unpinned identity poll
# and the connection that pins this key in the lease's known_hosts file.
sshd_ready=false
for _ in {1..60}; do
  served_host_key="$(
    /usr/bin/ssh-keyscan -T 2 -t ed25519 127.0.0.1 2>/dev/null |
      /usr/bin/awk '$2 == "ssh-ed25519" { print $3; exit }' || true
  )"
  if [[ -n "$served_host_key" ]] && [[ "$served_host_key" == "$expected_host_key" ]]; then
    sshd_ready=true
    break
  fi
  /bin/sleep 1
done
if [[ "$sshd_ready" != true ]]; then
  echo "sshd did not serve the new ED25519 host key after rotation" >&2
  exit 1
fi

tmp="$(/usr/bin/mktemp "${marker}.XXXXXX")"
trap '/bin/rm -f "$tmp"' EXIT
/usr/bin/printf '%s\n' "$platform_uuid" >"$tmp"
/usr/sbin/chown root:wheel "$tmp"
/bin/chmod 0644 "$tmp"
/bin/mv -f "$tmp" "$marker"
trap - EXIT

# Lume maps Crabbox's fresh host directory through VirtioFS. The challenge
# binds this response to the exact `lume run` process, so Crabbox can pin the
# rotated key before making any network connection to the guest.
if [[ "$challenge_processed" == true ]]; then
  identity_tmp="$(/usr/bin/mktemp "${identity_path}.XXXXXX")"
  trap '/bin/rm -f "$identity_tmp"' EXIT
  /usr/bin/printf '%s %s ssh-ed25519 %s\n' \
    "$challenge" "$platform_uuid" "$expected_host_key" >"$identity_tmp"
  /bin/chmod 0644 "$identity_tmp"
  /bin/mv -f "$identity_tmp" "$identity_path"
  /bin/rm -f "$challenge_path" "$ssh_user_path" "$authorized_key_path"
  trap - EXIT
fi
