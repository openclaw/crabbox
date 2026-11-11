package cli

import "path"

// Keep one artifact per remote user. Each invocation pins its own inode before
// verification, so replacement cannot change an active run's control binary.
const nativeRuntimeCacheRootScript = `set -eu
umask 077
cache="/tmp/crabbox-runtime-cache-$(id -u)"
if [ ! -e "$cache" ] && [ ! -L "$cache" ]; then mkdir -m 700 -- "$cache" || [ -d "$cache" ]; fi
[ -d "$cache" ] && [ ! -L "$cache" ] && [ -O "$cache" ] && [ "$(stat -c %a -- "$cache")" = 700 ] || exit 74
`

func nativeRuntimeCacheProbeScript(r *remoteNativeRuntime) string {
	directory := shellQuote(path.Dir(r.path))
	return nativeRuntimeCacheRootScript + `miss() { printf 'CBX-CACHE-MISS\n'; exit 0; }
[ -f "$cache/artifact" ] && [ ! -L "$cache/artifact" ] && [ -O "$cache/artifact" ] || miss
directory=` + directory + `
mkdir -m 700 -- "$directory" || exit 74
printf %s ` + shellQuote(r.nonce) + ` >"$directory/.nonce"
if ln -- "$cache/artifact" "$directory/crabbox" &&
   [ -f "$directory/crabbox" ] && [ ! -L "$directory/crabbox" ] && [ -O "$directory/crabbox" ] &&
   [ "$(stat -c %a -- "$directory/crabbox")" = 500 ]; then
  digest=
  if command -v sha256sum >/dev/null 2>&1; then digest=$(sha256sum -- "$directory/crabbox")
  elif command -v shasum >/dev/null 2>&1; then digest=$(shasum -a 256 -- "$directory/crabbox"); fi
  if [ "${digest%% *}" = ` + shellQuote(r.identity.SHA256) + ` ]; then
    ` + r.command("identity") + `
    exit $?
  fi
fi
` + nativeRuntimeRemoveScript(r.nonce) + `
miss
`
}

func nativeRuntimeCachePublishScript(r *remoteNativeRuntime) string {
	return nativeRuntimeCacheRootScript +
		"ln -- " + shellQuote(r.path) + " " + shellQuote(path.Dir(r.path)+"/cache.part") + "\n" +
		"mv -fT -- " + shellQuote(path.Dir(r.path)+"/cache.part") + " \"$cache/artifact\"\n"
}
