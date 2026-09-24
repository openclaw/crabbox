set -euo pipefail
: "${expected_node_major?Mint smoke renderer must set expected_node_major}"
[[ "$(id -u)" -ne 0 ]] || { echo 'developer image smoke requires a nonroot user' >&2; exit 1; }
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
node -e 'const major = process.versions.node.split(".")[0]; const expected = process.argv[1]; if (expected ? major !== expected : Number(major) < 24) throw new Error("Node.js " + (expected ? "major " + expected : "24 or newer") + " is required, found " + process.version)' -- "$expected_node_major"
corepack --version
if [[ -n "${expected_pnpm_version:-}" ]]; then
  (
    cd /
    version="$(COREPACK_ENABLE_NETWORK=0 pnpm --version)"
    [[ "$version" == "$expected_pnpm_version" ]] || {
      printf 'pnpm default mismatch: expected %s, found %s\n' "$expected_pnpm_version" "$version" >&2
      exit 1
    }
    corepack_version="$(COREPACK_ENABLE_NETWORK=0 corepack pnpm --version)"
    [[ "$version" == "$corepack_version" ]] || { echo 'ordinary pnpm does not match Corepack' >&2; exit 1; }
    printf '%s\n' "$version"
  )
else
  pnpm --version
fi
smoke_dir="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-smoke.XXXXXXXX")"
cache_probe=""
cleanup_smoke() {
  local result=$?
  trap - EXIT
  if [[ -n "$cache_probe" ]]; then
    rm -f -- "$cache_probe" || { [[ "$result" != 0 ]] || result=1; }
  fi
  rm -rf -- "$smoke_dir" || { [[ "$result" != 0 ]] || result=1; }
  exit "$result"
}
trap cleanup_smoke EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
export TMPDIR="$smoke_dir"

printf 'int main(void) { return 0; }\n' >"$smoke_dir/main.c"
cc "$smoke_dir/main.c" -o "$smoke_dir/compiler"
"$smoke_dir/compiler"
printf '%s\n' '{"private":true,"scripts":{"check":"node -e \"require('\''node:assert/strict'\'').equal(2 + 2, 4)\""}}' >"$smoke_dir/package.json"
(
  export COREPACK_ENABLE_NETWORK=0 npm_config_offline=true npm_config_cache="$smoke_dir/npm-cache"
  cd "$smoke_dir"
  npm --offline run check
  pnpm run check
)
for cache in /var/cache/crabbox/{pnpm,npm,corepack,docker}; do
  cache_probe="$(mktemp "$cache/.devtools-smoke.XXXXXXXX")"
  printf 'runtime-cache-ok\n' >"$cache_probe"
  grep -qx runtime-cache-ok "$cache_probe"
  rm -f -- "$cache_probe"
  cache_probe=""
done

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
docker_probe="$(cat <<'DOCKER'
set -eu
directory="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-docker-smoke.XXXXXXXX")"
project="crabbox-smoke-$(basename "$directory" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9')"
tag="$project:local"
cleanup() {
  status=$?
  trap - EXIT
  if test -f "$directory/compose.yaml"; then
    docker compose -p "$project" -f "$directory/compose.yaml" down --remove-orphans >/dev/null 2>&1 || { test "$status" != 0 || status=1; }
  fi
  docker image rm "$tag" >/dev/null 2>&1 || { test "$status" != 0 || status=1; }
  rm -rf -- "$directory" || { test "$status" != 0 || status=1; }
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
docker version
docker compose version
docker buildx version
docker image inspect hello-world ubuntu:24.04 node:24-bookworm >/dev/null
docker run --rm --pull=never --network=none hello-world
docker run --rm --pull=never --network=none node:24-bookworm node -e 'require("node:assert/strict").equal(2 + 2, 4)'
printf 'FROM scratch\nCOPY payload /payload\n' >"$directory/Dockerfile"
printf 'offline-build-ok\n' >"$directory/payload"
docker buildx build --builder default --pull=false --network=none --load --tag "$tag" "$directory"
cat >"$directory/compose.yaml" <<'COMPOSE'
services:
  check:
    image: ubuntu:24.04
    pull_policy: never
    network_mode: none
    command: ["sh", "-c", "printf 'offline-compose-ok\\n'"]
COMPOSE
docker compose -p "$project" -f "$directory/compose.yaml" run --rm --no-deps --pull never check
DOCKER
)"
# A newly added group is not visible to the current SSH process; refresh only that membership.
if docker version >/dev/null 2>&1; then
  sh -c "$docker_probe"
elif command -v sg >/dev/null 2>&1 && docker_group_member; then
  sg docker -c "$docker_probe"
else
  echo 'Docker is unavailable to the runtime user; verify the daemon and Docker group membership' >&2
  exit 1
fi

developer_archive_probe
echo devtools-smoke-ok
