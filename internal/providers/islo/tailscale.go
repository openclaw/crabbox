package islo

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	gosdk "github.com/islo-labs/go-sdk"
)

// defaultIsloTailscaleVersion pins the static Tailscale build downloaded into
// the sandbox. Override with CRABBOX_ISLO_TAILSCALE_VERSION when a newer build
// is required. We pin rather than chase "latest" so a warmup is reproducible.
const defaultIsloTailscaleVersion = "1.98.4"

// isloTailscaleVersionEnv lets operators override the pinned Tailscale build.
const isloTailscaleVersionEnv = "CRABBOX_ISLO_TAILSCALE_VERSION"

// isloTailscaleIPRe extracts the tailnet IPv4 the bring-up script reports back
// on its own line so we never have to parse the full `tailscale status`.
var isloTailscaleIPRe = regexp.MustCompile(`(?m)^CRABBOX_TS_IP=([0-9.]+)`)

// isloTailscaleHostnameInvalid matches anything outside a DNS label so we can
// collapse it to '-' before handing the hostname to `tailscale up`.
var isloTailscaleHostnameInvalid = regexp.MustCompile(`[^a-z0-9-]+`)

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
cd /tmp
case "$(uname -m)" in
  x86_64) A=amd64 ;;
  aarch64|arm64) A=arm64 ;;
  *) echo "unsupported arch $(uname -m)" >&2; exit 3 ;;
esac
if [ ! -x /tmp/ts/tailscaled ]; then
  wget -q -O /tmp/ts.tgz "https://pkgs.tailscale.com/stable/tailscale_${TS_VERSION}_${A}.tgz"
  rm -rf /tmp/ts; mkdir -p /tmp/ts
  tar -xzf /tmp/ts.tgz -C /tmp/ts --strip-components=1
fi
if ! /tmp/ts/tailscale --socket=/tmp/tsd.sock status >/dev/null 2>&1; then
  setsid /tmp/ts/tailscaled --tun=userspace-networking --statedir=/tmp/tsd \
    --socket=/tmp/tsd.sock --socks5-server=localhost:1055 \
    --outbound-http-proxy-listen=localhost:1055 \
    >/tmp/tsd.log 2>&1 </dev/null &
  for _ in $(seq 1 30); do [ -S /tmp/tsd.sock ] && break; sleep 0.5; done
fi
set -- --authkey="${TS_AUTHKEY}" --hostname="${TS_HOST}" --accept-dns=false --timeout=120s
if [ -n "${TS_TAGS}" ]; then set -- "$@" --advertise-tags="${TS_TAGS}"; fi
if [ -n "${TS_LOGIN_SERVER}" ]; then set -- "$@" --login-server="${TS_LOGIN_SERVER}"; fi
if [ -n "${TS_EXIT_NODE}" ]; then
  set -- "$@" --exit-node="${TS_EXIT_NODE}"
  if [ "${TS_EXIT_NODE_ALLOW_LAN}" = "true" ]; then set -- "$@" --exit-node-allow-lan-access; fi
fi
/tmp/ts/tailscale --socket=/tmp/tsd.sock up "$@"
ts_ip=""
for _ in $(seq 1 24); do
  ts_ip="$(/tmp/ts/tailscale --socket=/tmp/tsd.sock ip -4 2>/dev/null | head -n1 || true)"
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
	authKey := strings.TrimSpace(b.cfg.Tailscale.AuthKey)
	if authKey == "" {
		return exit(2, "provider=islo: --tailscale requires a node auth key in $%s", blank(b.cfg.Tailscale.AuthKeyEnv, "CRABBOX_TAILSCALE_AUTH_KEY"))
	}
	hostname := isloTailscaleHostname(b.cfg, slug)
	tags := strings.Join(b.cfg.Tailscale.Tags, ",")
	version := blank(strings.TrimSpace(os.Getenv(isloTailscaleVersionEnv)), defaultIsloTailscaleVersion)
	loginServer := strings.TrimSpace(os.Getenv("TS_CONTROL_URL"))
	exitNode := strings.TrimSpace(b.cfg.Tailscale.ExitNode)
	allowLAN := fmt.Sprint(b.cfg.Tailscale.ExitNodeAllowLANAccess)

	req := &gosdk.ExecRequest{Command: []string{"bash", "-lc", isloTailscaleBringUp}}
	req.Env = map[string]*string{}
	for k, v := range map[string]string{
		"TS_AUTHKEY":             authKey,
		"TS_HOST":                hostname,
		"TS_TAGS":                tags,
		"TS_VERSION":             version,
		"TS_LOGIN_SERVER":        loginServer,
		"TS_EXIT_NODE":           exitNode,
		"TS_EXIT_NODE_ALLOW_LAN": allowLAN,
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

// isloTailscaleHostname resolves the tailnet hostname for a sandbox from the
// configured template, substituting {slug}/{provider}, and sanitizes the result
// to a valid DNS label.
func isloTailscaleHostname(cfg Config, slug string) string {
	host := strings.TrimSpace(cfg.Tailscale.Hostname)
	if host == "" {
		tmpl := cfg.Tailscale.HostnameTemplate
		if tmpl == "" {
			tmpl = "crabbox-{slug}"
		}
		tmpl = strings.ReplaceAll(tmpl, "{slug}", slug)
		tmpl = strings.ReplaceAll(tmpl, "{provider}", isloProvider)
		host = tmpl
	}
	host = strings.ToLower(strings.TrimSpace(host))
	host = isloTailscaleHostnameInvalid.ReplaceAllString(host, "-")
	host = strings.Trim(host, "-")
	if host == "" {
		host = "crabbox-islo"
	}
	return host
}
