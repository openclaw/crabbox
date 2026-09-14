package cli

import "strconv"

// The WSL transport already stages this script privately. Renewal needs only
// the existing gate and owner marker, not another launcher or child inspection.
func remoteWorkspaceOwnerWSL2Renew(req workspaceOwnerRemoteRequest) string {
	return `set -eu
umask 077
root="$HOME/.crabbox/workspace-owners"
state="$root/` + req.Key + `.owner"
token=` + shellQuote(req.Token) + `
ttl=` + strconv.FormatInt(workspaceOwnerTTLSeconds(req.TTL), 10) + `
exec 9>"$root/` + req.Key + `.gate"
if flock -x -w 5 -E 75 9; then :; else
  code=$?
  if [ "$code" -eq 75 ]; then printf BUSY; exit 0; fi
  printf AMBIGUOUS; exit 74
fi
read_state() {
  IFS= read -r version && IFS= read -r state_token && IFS= read -r expiry
}
if ! read_state <"$state"; then printf AMBIGUOUS; exit 74; fi
[ "$version" = v1 ] || { printf AMBIGUOUS; exit 74; }
case "$state_token" in ''|*[!0-9a-f]*) printf AMBIGUOUS; exit 74;; esac
[ "${#state_token}" -eq 64 ] || { printf AMBIGUOUS; exit 74; }
case "$expiry" in ''|*[!0-9]*) printf AMBIGUOUS; exit 74;; esac
[ "$state_token" = "$token" ] || { printf MISMATCH; exit 75; }
now=$(date +%s)
[ "$expiry" -gt "$now" ] || { printf EXPIRED; exit 75; }
tmp="$state.tmp.$$"
trap 'rm -f "$tmp"' EXIT
printf 'v1\n%s\n%s\n' "$token" "$((now + ttl))" >"$tmp"
mv "$tmp" "$state"
printf RENEWED
`
}
