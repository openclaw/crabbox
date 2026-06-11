package islo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"regexp"
	"strings"

	gosdk "github.com/islo-labs/go-sdk"
)

// The version and architecture-specific digests move together in code review
// because every opted-in sandbox executes these downloaded binaries.
const defaultIsloTailscaleVersion = "1.98.4"

const (
	defaultIsloTailscaleAMD64SHA256 = "e6c08a8ee7e63e69aaf1b62ecd12672b3883fbcd2a176bf6cfa42a15fdce0b6b"
	defaultIsloTailscaleARM64SHA256 = "3cb068eb1368b6bb218d0ef0aa0a7a679a7156b7c979e2279cc2c2321b5f05c7"
)

// isloTailscaleIPRe extracts the explicit tailnet marker even when the Islo SSE
// stream concatenates adjacent stdout event payloads without line separators.
var isloTailscaleIPRe = regexp.MustCompile(`CRABBOX_TS_IP=([0-9.]+)`)

const isloTailscaleVerifyArchive = `
command -v sha256sum >/dev/null 2>&1 || { echo "sha256sum is required" >&2; exit 3; }
printf '%s  %s\n' "${TS_SHA256}" "${TS_ARCHIVE}" | sha256sum -c - >/dev/null
`

// isloTailscaleBringUp is the in-sandbox script crabbox runs over the islo exec
// stream. Islo is a delegated-run provider with no SSH lease, so crabbox cannot
// reuse the SSH runner-bootstrap that Tailscale-capable VM providers use. This
// script is the equivalent for the exec plane:
//
//   - downloads the pinned static Tailscale build (the sandbox image ships
//     wget but not curl, and has no systemd to run the packaged unit);
//   - starts tailscaled in userspace-networking mode. This is deliberate:
//     kernel mode rewrites the sandbox routing table, which severs the islo
//     exec transport the command output streams over (observed as the session
//     being SIGTERM'd mid-run). Userspace mode never touches host routing, so
//     the exec channel survives and the node still joins the tailnet;
//   - runs `tailscale up` with the pond-scoped advertise tags;
//   - prints the assigned 100.x address on its own CRABBOX_TS_IP= line.
//
// It is idempotent: a second warmup that reuses the sandbox re-uses the
// existing daemon and state. It never uses `pkill -f tailscaled`, which would
// match this very shell's command line and kill the session.
const isloTailscaleBringUp = `
set -e
umask 077
TS_AUTH_FILE="$(mktemp /tmp/crabbox-ts-auth.XXXXXX)"
printf '%s' "${TS_AUTHKEY}" >"${TS_AUTH_FILE}"
unset TS_AUTHKEY
trap 'rm -f "${TS_AUTH_FILE}"; rm -rf "${TS_EXTRACT_DIR:-}"' EXIT
cd /tmp
case "$(uname -m)" in
  x86_64) A=amd64; TS_SHA256=` + defaultIsloTailscaleAMD64SHA256 + ` ;;
  aarch64|arm64) A=arm64; TS_SHA256=` + defaultIsloTailscaleARM64SHA256 + ` ;;
  *) echo "unsupported arch $(uname -m)" >&2; exit 3 ;;
esac
TS_ARCHIVE=/tmp/ts.tgz
wget -q -O "${TS_ARCHIVE}" "https://pkgs.tailscale.com/stable/tailscale_` + defaultIsloTailscaleVersion + `_${A}.tgz"
` + isloTailscaleVerifyArchive + `
TS_EXTRACT_DIR="/tmp/ts.extract.$$"
rm -rf "${TS_EXTRACT_DIR}"
mkdir -p "${TS_EXTRACT_DIR}"
tar -xzf "${TS_ARCHIVE}" -C "${TS_EXTRACT_DIR}" --strip-components=1
rm -rf /tmp/ts
mv "${TS_EXTRACT_DIR}" /tmp/ts
TS_EXTRACT_DIR=""
: "${TS_STATE_DIR:?}"
mkdir -p "${TS_STATE_DIR}"
chmod 700 "${TS_STATE_DIR}"
TS_SOCKET="${TS_STATE_DIR}/tailscaled.sock"
if ! /tmp/ts/tailscale --socket="${TS_SOCKET}" status >/dev/null 2>&1; then
  # Userspace ingress forwards tailnet TCP to 127.0.0.1:<port>. Keep the
  # unauthenticated outbound proxy on another loopback address.
  setsid /tmp/ts/tailscaled --tun=userspace-networking --statedir="${TS_STATE_DIR}" \
    --socket="${TS_SOCKET}" --socks5-server=127.0.0.2:1055 \
    --outbound-http-proxy-listen=127.0.0.2:1055 \
    >"${TS_STATE_DIR}/tailscaled.log" 2>&1 </dev/null &
  for _ in $(seq 1 30); do [ -S "${TS_SOCKET}" ] && break; sleep 0.5; done
fi
set -- --auth-key="file:${TS_AUTH_FILE}" --hostname="${TS_HOST}" --accept-dns=false --shields-up=false --timeout=120s
if [ -n "${TS_TAGS}" ]; then set -- "$@" --advertise-tags="${TS_TAGS}"; fi
if [ -n "${TS_LOGIN_SERVER}" ]; then set -- "$@" --login-server="${TS_LOGIN_SERVER}"; fi
if [ -n "${TS_EXIT_NODE}" ]; then
  set -- "$@" --exit-node="${TS_EXIT_NODE}"
  if [ "${TS_EXIT_NODE_ALLOW_LAN}" = "true" ]; then set -- "$@" --exit-node-allow-lan-access; fi
fi
/tmp/ts/tailscale --socket="${TS_SOCKET}" up "$@"
ts_ip=""
for _ in $(seq 1 24); do
  ts_ip="$(/tmp/ts/tailscale --socket="${TS_SOCKET}" ip -4 2>/dev/null | head -n1 || true)"
  if [ -n "${ts_ip}" ]; then break; fi
  sleep 5
done
test -n "${ts_ip}"
echo "CRABBOX_TS_IP=${ts_ip}"
`

// maybeJoinTailscale brings an islo sandbox onto the configured tailnet when
// the lease was created with --tailscale. It is a no-op otherwise, so plain
// url-bridge islo ponds are unchanged. On success it records the tailnet IPv4
// on the lease claim so `pond peers` reports the member on the tailnet plane.
func (b *isloBackend) maybeJoinTailscale(ctx context.Context, client isloAPI, sandboxName, slug, leaseID string) error {
	if !b.cfg.Tailscale.Enabled {
		return nil
	}
	if err := b.validateTailscaleConfig(); err != nil {
		return err
	}
	authKey := strings.TrimSpace(b.cfg.Tailscale.AuthKey)
	hostname := isloTailscaleHostname(b.cfg, leaseID, slug)
	tags := strings.Join(b.cfg.Tailscale.Tags, ",")
	loginServer := strings.TrimSpace(os.Getenv("TS_CONTROL_URL"))
	exitNode := strings.TrimSpace(b.cfg.Tailscale.ExitNode)
	allowLAN := fmt.Sprint(b.cfg.Tailscale.ExitNodeAllowLANAccess)
	stateDir := isloTailscaleStateDir(leaseID)

	req := &gosdk.ExecRequest{Command: []string{"bash", "-lc", isloTailscaleBringUp}}
	req.Env = map[string]*string{}
	for k, v := range map[string]string{
		"TS_AUTHKEY":             authKey,
		"TS_HOST":                hostname,
		"TS_TAGS":                tags,
		"TS_LOGIN_SERVER":        loginServer,
		"TS_EXIT_NODE":           exitNode,
		"TS_EXIT_NODE_ALLOW_LAN": allowLAN,
		"TS_STATE_DIR":           stateDir,
	} {
		v := v
		req.Env[k] = &v
	}

	fmt.Fprintf(b.rt.Stderr, "islo: joining tailnet (hostname=%s tags=%s)\n", hostname, blank(tags, "<none>"))
	var out bytes.Buffer
	code, err := client.ExecStream(ctx, sandboxName, req, &out, b.rt.Stderr)
	if err != nil {
		return exit(1, "islo tailscale bring-up: %v", err)
	}
	if code != 0 {
		return exit(1, "islo tailscale bring-up exited %d", code)
	}
	m := isloTailscaleIPRe.FindStringSubmatch(out.String())
	if m == nil || m[1] == "" {
		return exit(1, "islo tailscale bring-up: no tailnet IPv4 reported")
	}
	fmt.Fprintf(b.rt.Stderr, "islo: joined tailnet ip=%s\n", m[1])
	return updateLeaseClaimTailscale(leaseID, m[1], "")
}

func isloTailscaleStateDir(leaseID string) string {
	sum := sha256.Sum256([]byte(leaseID))
	return fmt.Sprintf("/tmp/crabbox-ts-%x", sum[:8])
}

func (b *isloBackend) validateTailscaleConfig() error {
	if !b.cfg.Tailscale.Enabled || strings.TrimSpace(b.cfg.Tailscale.AuthKey) != "" {
		return nil
	}
	return exit(2, "provider=islo: --tailscale requires a node auth key in $%s", blank(b.cfg.Tailscale.AuthKeyEnv, "CRABBOX_TAILSCALE_AUTH_KEY"))
}

// isloTailscaleHostname resolves the tailnet hostname for a sandbox from the
// configured template, substituting all shared tokens and sanitizing the result.
func isloTailscaleHostname(cfg Config, leaseID, slug string) string {
	template := strings.TrimSpace(cfg.Tailscale.Hostname)
	if template == "" {
		template = cfg.Tailscale.HostnameTemplate
	}
	return renderTailscaleHostname(template, leaseID, slug, isloProvider)
}
