#!/usr/bin/env bash
set -euo pipefail

echo devtools-smoke-ok
uname -a
command -v git
command -v gh
command -v jq
command -v rg
command -v fd
command -v python3
command -v node
command -v npm
command -v corepack
command -v pnpm
command -v trufflehog
trufflehog --no-update --version
command -v docker
node --version
node -e 'if (Number(process.versions.node.split(".")[0]) < 24) throw new Error(`Node.js 24 or newer is required, found ${process.version}`)'
corepack --version
pnpm --version

docker_group_member() {
  if id -nG 2>/dev/null | tr ' ' '\n' | grep -qx docker; then
    return 0
  fi
  local current_user docker_entry docker_members member
  current_user="$(whoami)"
  docker_entry="$(getent group docker 2>/dev/null || true)"
  [[ -n "$docker_entry" ]] || return 1
  docker_members="${docker_entry#*:*:*:}"
  local IFS=','
  local -a docker_member_list
  read -ra docker_member_list <<<"$docker_members"
  for member in "${docker_member_list[@]}"; do
    [[ "$member" == "$current_user" ]] && return 0
  done
  return 1
}

docker_probe='docker version && docker compose version && docker image inspect hello-world ubuntu:24.04 node:24-bookworm >/dev/null'
if ! sh -c "$docker_probe"; then
  if command -v sg >/dev/null 2>&1 && docker_group_member; then
    sg docker -c "$docker_probe"
  else
    sudo sh -c "$docker_probe"
  fi
fi

test -d /var/cache/crabbox/pnpm
test -f /var/lib/crabbox/image-ready
