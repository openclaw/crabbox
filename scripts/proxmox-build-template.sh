#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Build a Crabbox-ready Proxmox VE QEMU template from a public Ubuntu cloud image.

Run this on a Proxmox VE node as root. It creates a local VM template only; it
does not use Crabbox API tokens, inject lease SSH keys, or bake secrets into the
image.

Environment:
  CRABBOX_PROXMOX_TEMPLATE_ID        VMID to create (default: 9400)
  CRABBOX_PROXMOX_TEMPLATE_NAME      Template name (default: crabbox-ubuntu-2404)
  CRABBOX_PROXMOX_STORAGE            Target Proxmox storage (default: local-lvm)
  CRABBOX_PROXMOX_BRIDGE             Network bridge (default: vmbr0)
  CRABBOX_PROXMOX_USER               Cloud-init user Crabbox configures (default: crabbox)
  CRABBOX_PROXMOX_IMAGE_URL          Cloud image URL
  CRABBOX_PROXMOX_IMAGE_SHA256       Optional expected image sha256
  CRABBOX_PROXMOX_CORES              Template vCPU count (default: 2)
  CRABBOX_PROXMOX_MEMORY_MB          Template memory in MiB (default: 4096)
  CRABBOX_PROXMOX_DISK_SIZE          Final root disk size, qm syntax (default: 32G)
  CRABBOX_PROXMOX_REPLACE_TEMPLATE   Destroy an existing VM/template first when set to 1

Example:
  CRABBOX_PROXMOX_STORAGE=local-lvm ./scripts/proxmox-build-template.sh
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

template_id="${CRABBOX_PROXMOX_TEMPLATE_ID:-9400}"
template_name="${CRABBOX_PROXMOX_TEMPLATE_NAME:-crabbox-ubuntu-2404}"
storage="${CRABBOX_PROXMOX_STORAGE:-local-lvm}"
bridge="${CRABBOX_PROXMOX_BRIDGE:-vmbr0}"
vm_user="${CRABBOX_PROXMOX_USER:-crabbox}"
image_url="${CRABBOX_PROXMOX_IMAGE_URL:-https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img}"
image_sha256="${CRABBOX_PROXMOX_IMAGE_SHA256:-}"
cores="${CRABBOX_PROXMOX_CORES:-2}"
memory_mb="${CRABBOX_PROXMOX_MEMORY_MB:-4096}"
disk_size="${CRABBOX_PROXMOX_DISK_SIZE:-32G}"
replace_template="${CRABBOX_PROXMOX_REPLACE_TEMPLATE:-0}"

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

is_uint() {
  [[ "$1" =~ ^[0-9]+$ ]]
}

fetch_image() {
  local url="$1"
  local dest="$2"

  if command -v curl >/dev/null 2>&1; then
    curl -fL --retry 3 --output "$dest" "$url"
  elif command -v wget >/dev/null 2>&1; then
    wget -O "$dest" "$url"
  else
    die "missing curl or wget"
  fi
}

if [[ "${EUID}" -ne 0 ]]; then
  die "run this script on a Proxmox VE node as root"
fi

is_uint "$template_id" || die "CRABBOX_PROXMOX_TEMPLATE_ID must be numeric"
is_uint "$cores" || die "CRABBOX_PROXMOX_CORES must be numeric"
is_uint "$memory_mb" || die "CRABBOX_PROXMOX_MEMORY_MB must be numeric"

need_cmd qm
need_cmd pvesm
need_cmd virt-customize
need_cmd qemu-img
need_cmd sha256sum
need_cmd awk

if qm status "$template_id" >/dev/null 2>&1; then
  if [[ "$replace_template" == "1" ]]; then
    printf 'destroying existing VM/template %s before rebuild\n' "$template_id" >&2
    qm destroy "$template_id" --purge 1
  else
    die "VMID $template_id already exists; set CRABBOX_PROXMOX_REPLACE_TEMPLATE=1 to rebuild it"
  fi
fi

workdir="$(mktemp -d /var/tmp/crabbox-proxmox-template.XXXXXX)"
cleanup() {
  rm -rf "$workdir"
}
trap cleanup EXIT

source_image="$workdir/source.img"
custom_image="$workdir/${template_name}.qcow2"

printf 'downloading %s\n' "$image_url" >&2
fetch_image "$image_url" "$source_image"

if [[ -n "$image_sha256" ]]; then
  printf '%s  %s\n' "$image_sha256" "$source_image" | sha256sum -c -
fi

printf 'customizing image packages and services\n' >&2
qemu-img convert -O qcow2 "$source_image" "$custom_image"
virt-customize -a "$custom_image" \
  --install cloud-init,qemu-guest-agent,openssh-server,ca-certificates,curl,git,jq,rsync,tar \
  --run-command 'systemctl enable cloud-init qemu-guest-agent ssh || true' \
  --run-command 'cloud-init clean --logs'

printf 'creating Proxmox template VMID=%s name=%s storage=%s bridge=%s\n' "$template_id" "$template_name" "$storage" "$bridge" >&2
qm create "$template_id" \
  --name "$template_name" \
  --memory "$memory_mb" \
  --cores "$cores" \
  --net0 "virtio,bridge=${bridge}" \
  --serial0 socket \
  --vga serial0 \
  --agent enabled=1 \
  --ostype l26 \
  --scsihw virtio-scsi-pci

qm importdisk "$template_id" "$custom_image" "$storage"

disk_volume="$(pvesm list "$storage" --vmid "$template_id" | awk -v id="$template_id" 'NR > 1 && $1 ~ ("vm-" id "-disk-") { print $1; exit }')"
if [[ -z "$disk_volume" ]]; then
  qm destroy "$template_id" --purge 1
  die "could not find imported disk volume for VMID $template_id on storage $storage"
fi

qm set "$template_id" --scsi0 "${disk_volume},discard=on"
qm set "$template_id" --ide2 "${storage}:cloudinit"
qm set "$template_id" --boot c --bootdisk scsi0
qm set "$template_id" --ipconfig0 ip=dhcp --ciuser "$vm_user"
qm resize "$template_id" scsi0 "$disk_size"
qm template "$template_id"

cat <<EOF
Created Crabbox Proxmox template:
  templateId: $template_id
  name: $template_name
  storage: $storage
  user: $vm_user

Crabbox config:
  provider: proxmox
  proxmox:
    node: <your-proxmox-node>
    templateId: $template_id
    storage: $storage
    bridge: $bridge
    user: $vm_user
EOF
