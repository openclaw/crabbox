#!/bin/bash
set +x
set -euo pipefail
export PATH="/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
# Match recipes/devtools/v1's Linux Node LTS baseline.
node_version=24.19.0
case "$(uname -m)" in
  x86_64) node_arch=x64; node_sha=d1b5e999db158c62fe8f7267a4476b035d8bd93b1a605bac24a3f0dd166e3316 ;;
  arm64) node_arch=arm64; node_sha=8294b7aa9b03997481c06babf1e8b270c859358f27da57a11509afe537ac381d ;;
  *) echo 'unsupported macOS Node architecture' >&2; exit 1 ;;
esac
if node --version >/dev/null 2>&1 && npm --version >/dev/null 2>&1; then
  exit 0
fi
install -d -m 0755 /usr/local/bin /usr/local/lib/crabbox
node_prefix="/usr/local/lib/crabbox/node-v$node_version-darwin-$node_arch"
node_lock=/usr/local/lib/crabbox/.node-install-lock
node_wait=300
while ! mkdir "$node_lock" 2>/dev/null; do
  [ "$node_wait" -gt 0 ] || { echo 'macOS Node installer is busy' >&2; exit 1; }
  sleep 1
  node_wait=$((node_wait - 1))
done
node_staging=
trap 'if [ -n "$node_staging" ]; then rm -rf "$node_staging"; fi; rmdir "$node_lock"' EXIT
trap 'exit 1' HUP INT TERM
if node --version >/dev/null 2>&1 && npm --version >/dev/null 2>&1; then
  exit 0
fi
node_staging="$(mktemp -d /usr/local/lib/crabbox/.node.XXXXXX)"
node_archive="node-v$node_version-darwin-$node_arch.tar.gz"
curl -q --proto '=https' --tlsv1.2 -fsSL --connect-timeout 10 --max-time 300 \
  --retry 3 --output "$node_staging/$node_archive" "https://nodejs.org/dist/v$node_version/$node_archive"
(cd "$node_staging"; printf '%s  %s\n' "$node_sha" "$node_archive" | shasum -a 256 -c -)
mkdir "$node_staging/runtime"
tar -xzf "$node_staging/$node_archive" -C "$node_staging/runtime" --strip-components=1
test "$("$node_staging/runtime/bin/node" --version)" = "v$node_version"
PATH="$node_staging/runtime/bin:$PATH" "$node_staging/runtime/bin/npm" --version >/dev/null
chmod 0755 "$node_staging/runtime"
# The version slot is managed by this installer; publish only verified bytes.
rm -rf "$node_prefix"
mv "$node_staging/runtime" "$node_prefix"
test "$("$node_prefix/bin/node" --version)" = "v$node_version"
for tool in node npm npx; do
  ln -sfn "$node_prefix/bin/$tool" "/usr/local/bin/$tool"
done
node --version
npm --version
