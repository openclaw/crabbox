set -euo pipefail
[[ "$(id -u)" != 0 ]] || { echo 'developer image smoke requires the runtime user, not root' >&2; exit 1; }
export COREPACK_ENABLE_NETWORK=0 npm_config_offline=true
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

smoke_dir="$(mktemp -d "${TMPDIR:-/tmp}/crabbox-smoke.XXXXXXXX")"
cache_probe=""
browser_pid=""
cleanup_smoke() {
  if [[ -n "$browser_pid" ]]; then
    kill "$browser_pid" 2>/dev/null || true
    wait "$browser_pid" 2>/dev/null || true
  fi
  rm -f -- "${cache_probe:-}"
  rm -rf -- "$smoke_dir"
}
trap cleanup_smoke EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
printf 'int main(void) { return 0; }\n' >"$smoke_dir/main.c"
cc "$smoke_dir/main.c" -o "$smoke_dir/compiler"
"$smoke_dir/compiler"
python3 -I -c 'import sqlite3, ssl; assert sqlite3.connect(":memory:").execute("select 2 + 2").fetchone()[0] == 4; assert ssl.create_default_context().get_ca_certs()'
printf '%s\n' '{"private":true,"scripts":{"check":"node -e \"require('\''node:assert/strict'\'').equal(2 + 2, 4)\""}}' >"$smoke_dir/package.json"
(
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

if [[ "${CRABBOX_LINUX_BROWSER:-1}" == 1 ]]; then
  timeout 30 crabbox-browser --headless --disable-gpu --disable-background-networking \
    --disable-component-update --disable-sync --user-data-dir="$smoke_dir/browser" \
    --dump-dom 'data:text/html,<p>crabbox-browser-smoke</p>' >"$smoke_dir/browser.html"
  grep -q '<p>crabbox-browser-smoke</p>' "$smoke_dir/browser.html"
fi
if [[ "${CRABBOX_LINUX_DESKTOP_TOOLS:-1}" == 1 ]]; then
  export DISPLAY="${DISPLAY:-$(sed -nE 's/^DISPLAY=(:[0-9]+)$/\1/p' /var/lib/crabbox/desktop.env)}"
  test -n "$DISPLAY"
  xset q >/dev/null
  xdotool getdisplaygeometry
  scrot "$smoke_dir/desktop.png"
  ffprobe -v error -select_streams v:0 -show_entries stream=width,height -of csv=p=0 "$smoke_dir/desktop.png" |
    grep -Eq '^[1-9][0-9]*,[1-9][0-9]*$'
  if [[ "${CRABBOX_LINUX_BROWSER:-1}" == 1 ]]; then
    browser_title="crabbox-render-$(basename "$smoke_dir" | tr -cd 'a-zA-Z0-9')"
    printf '<title>%s</title><style>html,body{margin:0;height:100%%;background:rgb(25,163,91)}</style>\n' \
      "$browser_title" >"$smoke_dir/render.html"
    # The timeout owns only this isolated browser, including its child processes.
    timeout --kill-after=5 30 crabbox-browser --disable-gpu --disable-background-networking \
      --disable-component-update --disable-sync --force-color-profile=srgb \
      --window-size=640,480 --window-position=0,0 --user-data-dir="$smoke_dir/visible-browser" \
      --app="file://$smoke_dir/render.html" \
      >"$smoke_dir/visible-browser.log" 2>&1 &
    browser_pid=$!
    rendered=0
    for attempt in {1..15}; do
      kill -0 "$browser_pid" 2>/dev/null || break
      browser_window="$(xdotool search --onlyvisible --limit 1 --name "$browser_title" 2>/dev/null || true)"
      if [[ "$browser_window" =~ ^[0-9]+$ ]] &&
        timeout 5 ffmpeg -nostdin -v error -y -f x11grab -draw_mouse 0 -window_id "$browser_window" \
          -i "$DISPLAY" -frames:v 1 -vf 'crop=16:16:(iw-16)/2:(ih-16)/2,format=rgb24' \
          -f rawvideo "$smoke_dir/browser.rgb" &&
        node -e 'const fs=require("node:fs"); if (!fs.readFileSync(process.argv[1]).equals(Buffer.alloc(768, Buffer.from([25,163,91])))) process.exit(1)' "$smoke_dir/browser.rgb"; then
        rendered=1
        break
      fi
      sleep 1
    done
    [[ "$rendered" == 1 ]] || { echo 'browser did not render the local fixture on the selected display' >&2; exit 1; }
  fi
fi
echo devtools-smoke-ok
