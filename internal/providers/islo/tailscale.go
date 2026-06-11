package islo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	gosdk "github.com/islo-labs/go-sdk"
	islcore "github.com/islo-labs/go-sdk/core"
	core "github.com/openclaw/crabbox/internal/cli"
)

// The version and architecture-specific digests move together in code review
// because every opted-in sandbox executes these downloaded binaries.
const defaultIsloTailscaleVersion = "1.98.4"
const isloTailscaleRecoveryPendingExitCode = 75

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
: "${TS_STATE_DIR:?}"
mkdir -p "${TS_STATE_DIR}"
chmod 700 "${TS_STATE_DIR}"
TS_LOCK_FILE="${TS_STATE_DIR}/operation.lock"
TS_LOCKED=""
for _ in $(seq 1 720); do
  if (set -o noclobber; printf '%s\n' "$$" >"${TS_LOCK_FILE}") 2>/dev/null; then
    TS_LOCKED=1
    break
  fi
  lock_pid="$(cat "${TS_LOCK_FILE}" 2>/dev/null || true)"
  case "${lock_pid}" in
    ""|*[!0-9]*) rm -f "${TS_LOCK_FILE}"; continue ;;
  esac
  if ! kill -0 "${lock_pid}" 2>/dev/null; then rm -f "${TS_LOCK_FILE}"; continue; fi
  sleep 0.5
done
test "${TS_LOCKED}" = 1 || { echo "timed out waiting for tailscale operation lock" >&2; exit 75; }
TS_RUNTIME_DIR="/run/crabbox/tailscale/$(basename "${TS_STATE_DIR}")"
mkdir -p "${TS_RUNTIME_DIR}"
chmod 700 "${TS_RUNTIME_DIR}"
rm -f "${TS_RUNTIME_DIR}"/auth.*
TS_AUTH_FILE=""
if [ -n "${TS_AUTHKEY}" ]; then
  TS_AUTH_FILE="$(mktemp "${TS_RUNTIME_DIR}/auth.XXXXXX")"
  printf '%s' "${TS_AUTHKEY}" >"${TS_AUTH_FILE}"
fi
unset TS_AUTHKEY
TS_INSTALL_DIR="$(mktemp -d "${TS_STATE_DIR}/install.XXXXXX")"
trap 'if [ -n "${TS_AUTH_FILE}" ]; then rm -f "${TS_AUTH_FILE}"; fi; rm -rf "${TS_INSTALL_DIR}"; if [ "${TS_LOCKED}" = 1 ]; then rm -f "${TS_LOCK_FILE}"; fi' EXIT
case "$(uname -m)" in
  x86_64) A=amd64; TS_SHA256=` + defaultIsloTailscaleAMD64SHA256 + ` ;;
  aarch64|arm64) A=arm64; TS_SHA256=` + defaultIsloTailscaleARM64SHA256 + ` ;;
  *) echo "unsupported arch $(uname -m)" >&2; exit 3 ;;
esac
TS_ARCHIVE="${TS_INSTALL_DIR}/tailscale.tgz"
wget -q -O "${TS_ARCHIVE}" "https://pkgs.tailscale.com/stable/tailscale_` + defaultIsloTailscaleVersion + `_${A}.tgz"
` + isloTailscaleVerifyArchive + `
TS_EXTRACT_DIR="${TS_INSTALL_DIR}/extract"
mkdir -p "${TS_EXTRACT_DIR}"
tar -xzf "${TS_ARCHIVE}" -C "${TS_EXTRACT_DIR}" --strip-components=1
TS_BIN_DIR="${TS_STATE_DIR}/bin"
rm -rf "${TS_BIN_DIR}"
mv "${TS_EXTRACT_DIR}" "${TS_BIN_DIR}"
TS_SOCKET="${TS_STATE_DIR}/tailscaled.sock"
TS_STATE_FILE="${TS_STATE_DIR}/tailscaled.state"
tailscale_ip_if_ready() {
  status_json="$("${TS_BIN_DIR}/tailscale" --socket="${TS_SOCKET}" status --json 2>/dev/null || true)"
  printf '%s' "${status_json}" | grep -Eq '"BackendState"[[:space:]]*:[[:space:]]*"Running"' || return 1
  "${TS_BIN_DIR}/tailscale" --socket="${TS_SOCKET}" ip -4 2>/dev/null | head -n1
}
tailscale_backend_state() {
  "${TS_BIN_DIR}/tailscale" --socket="${TS_SOCKET}" status --json 2>/dev/null |
    sed -n 's/.*"BackendState"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' |
    head -n1
}
if [ ! -S "${TS_SOCKET}" ] || ! "${TS_BIN_DIR}/tailscale" --socket="${TS_SOCKET}" status --json >/dev/null 2>&1; then
  # Userspace ingress forwards tailnet TCP to 127.0.0.1:<port>. Keep the
  # unauthenticated outbound proxy on another loopback address.
  rm -f "${TS_SOCKET}"
  setsid "${TS_BIN_DIR}/tailscaled" --tun=userspace-networking --state="${TS_STATE_FILE}" \
    --socket="${TS_SOCKET}" --socks5-server=127.0.0.2:1055 \
    --outbound-http-proxy-listen=127.0.0.2:1055 \
    >"${TS_STATE_DIR}/tailscaled.log" 2>&1 </dev/null &
  for _ in $(seq 1 30); do [ -S "${TS_SOCKET}" ] && break; sleep 0.5; done
fi
ts_ip=""
backend_state=""
for _ in $(seq 1 120); do
  backend_state="$(tailscale_backend_state || true)"
  ts_ip="$(tailscale_ip_if_ready || true)"
  if [ -n "${ts_ip}" ]; then break; fi
  case "${backend_state}" in
    NeedsLogin|NeedsMachineAuth|Stopped|NoState) break ;;
  esac
  sleep 1
done
if [ -z "${ts_ip}" ]; then
  if [ -z "${TS_AUTH_FILE}" ]; then
    case "${backend_state}" in
      Starting|Running) echo "tailscale recovery is still starting" >&2; exit 75 ;;
      Stopped) : ;;
      *) echo "tailscale state unavailable and no auth key provided" >&2; exit 4 ;;
    esac
  fi
  set -- --hostname="${TS_HOST}" --accept-dns=false --shields-up=false --timeout=120s
  if [ -n "${TS_AUTH_FILE}" ]; then set -- "$@" --auth-key="file:${TS_AUTH_FILE}"; fi
  if [ -n "${TS_TAGS}" ]; then set -- "$@" --advertise-tags="${TS_TAGS}"; fi
  if [ -n "${TS_LOGIN_SERVER}" ]; then set -- "$@" --login-server="${TS_LOGIN_SERVER}"; fi
  if [ -n "${TS_EXIT_NODE}" ]; then
    set -- "$@" --exit-node="${TS_EXIT_NODE}"
    if [ "${TS_EXIT_NODE_ALLOW_LAN}" = "true" ]; then set -- "$@" --exit-node-allow-lan-access; fi
  fi
  "${TS_BIN_DIR}/tailscale" --socket="${TS_SOCKET}" up "$@"
  for _ in $(seq 1 24); do
    ts_ip="$(tailscale_ip_if_ready || true)"
    if [ -n "${ts_ip}" ]; then break; fi
    sleep 5
  done
fi
test -n "${ts_ip}"
echo "CRABBOX_TS_IP=${ts_ip}"
`

const isloTailscaleHealthCheck = `
set -e
: "${TS_STATE_DIR:?}"
TS_LOCK_FILE="${TS_STATE_DIR}/operation.lock"
if [ -e "${TS_LOCK_FILE}" ]; then
  lock_pid="$(cat "${TS_LOCK_FILE}" 2>/dev/null || true)"
  case "${lock_pid}" in
    ""|*[!0-9]*) rm -f "${TS_LOCK_FILE}" ;;
    *) if kill -0 "${lock_pid}" 2>/dev/null; then
         echo "tailscale recovery is in progress" >&2
         exit 75
       else
         rm -f "${TS_LOCK_FILE}"
       fi ;;
  esac
fi
TS_SOCKET="${TS_STATE_DIR}/tailscaled.sock"
test -S "${TS_SOCKET}"
TS_BIN_DIR="${TS_STATE_DIR}/bin"
status_json="$("${TS_BIN_DIR}/tailscale" --socket="${TS_SOCKET}" status --json 2>/dev/null)"
printf '%s' "${status_json}" | grep -Eq '"BackendState"[[:space:]]*:[[:space:]]*"Running"'
ts_ip="$("${TS_BIN_DIR}/tailscale" --socket="${TS_SOCKET}" ip -4 2>/dev/null | head -n1)"
test -n "${ts_ip}"
echo "CRABBOX_TS_IP=${ts_ip}"
`

type isloTailscaleSettings struct {
	Hostname    string
	Tags        []string
	LoginServer string
	ExitNode    string
	ExitNodeLAN bool
}

func (b *isloBackend) configuredTailscaleSettings(leaseID, slug string) isloTailscaleSettings {
	return isloTailscaleSettings{
		Hostname:    isloTailscaleHostname(b.cfg, leaseID, slug),
		Tags:        append([]string(nil), b.cfg.Tailscale.Tags...),
		LoginServer: strings.TrimSpace(os.Getenv("TS_CONTROL_URL")),
		ExitNode:    strings.TrimSpace(b.cfg.Tailscale.ExitNode),
		ExitNodeLAN: b.cfg.Tailscale.ExitNodeAllowLANAccess,
	}
}

func (b *isloBackend) claimedTailscaleSettings(claim core.LeaseClaim, leaseID, slug string) isloTailscaleSettings {
	if claim.TailscaleHostname != "" {
		return isloTailscaleSettings{
			Hostname:    claim.TailscaleHostname,
			Tags:        append([]string(nil), claim.TailscaleTags...),
			LoginServer: claim.TailscaleLoginURL,
			ExitNode:    claim.TailscaleExitNode,
			ExitNodeLAN: claim.TailscaleExitLAN,
		}
	}
	restart := *b
	restart.cfg.Pond = claim.Pond
	appendDirectPondTailscaleTag(&restart.cfg)
	return restart.configuredTailscaleSettings(leaseID, slug)
}

func isloClaimTailscaleEnrolled(claim core.LeaseClaim) bool {
	return claim.TailscaleIPv4 != "" ||
		claim.TailscaleFQDN != "" ||
		claim.TailscaleHostname != "" ||
		claim.Labels["tailscale"] == "true"
}

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
	settings := b.configuredTailscaleSettings(leaseID, slug)
	if err := b.runTailscaleBringUp(ctx, client, sandboxName, leaseID, settings); err != nil {
		return err
	}
	return updateLeaseClaimTailscaleSettings(leaseID, settings.Hostname, settings.Tags, settings.LoginServer, settings.ExitNode, settings.ExitNodeLAN)
}

func (b *isloBackend) runTailscaleBringUp(ctx context.Context, client isloAPI, sandboxName, leaseID string, settings isloTailscaleSettings) error {
	authKey := strings.TrimSpace(b.cfg.Tailscale.AuthKey)
	tags := strings.Join(settings.Tags, ",")
	stateDir := isloTailscaleStateDir(leaseID)

	req := &gosdk.ExecRequest{
		Command: []string{"bash", "-lc", isloTailscaleBringUp},
		User:    stringValue(isloAdminUser),
	}
	req.Env = map[string]*string{}
	for k, v := range map[string]string{
		"TS_AUTHKEY":             authKey,
		"TS_HOST":                settings.Hostname,
		"TS_TAGS":                tags,
		"TS_LOGIN_SERVER":        settings.LoginServer,
		"TS_EXIT_NODE":           settings.ExitNode,
		"TS_EXIT_NODE_ALLOW_LAN": fmt.Sprint(settings.ExitNodeLAN),
		"TS_STATE_DIR":           stateDir,
	} {
		v := v
		req.Env[k] = &v
	}

	fmt.Fprintf(b.rt.Stderr, "islo: joining tailnet (hostname=%s tags=%s)\n", settings.Hostname, blank(tags, "<none>"))
	var out bytes.Buffer
	code, err := client.ExecStream(ctx, sandboxName, req, &out, b.rt.Stderr)
	if err != nil {
		return exit(1, "islo tailscale bring-up: %v", err)
	}
	if code != 0 {
		if code == isloTailscaleRecoveryPendingExitCode {
			return fmt.Errorf("%w: saved-state recovery is still starting", core.ErrTailnetPeerValidationUnavailable)
		}
		return exit(1, "islo tailscale bring-up exited %d", code)
	}
	m := isloTailscaleIPRe.FindStringSubmatch(out.String())
	if m == nil || m[1] == "" {
		return exit(1, "islo tailscale bring-up: no tailnet IPv4 reported")
	}
	fmt.Fprintf(b.rt.Stderr, "islo: joined tailnet ip=%s\n", m[1])
	return updateLeaseClaimTailscale(leaseID, m[1], "")
}

func (b *isloBackend) ensureLeaseTailscale(ctx context.Context, client isloAPI, sandboxName, slug, leaseID string, repair bool) (core.TailscaleMetadata, error) {
	claim, ok, err := resolveLeaseClaim(leaseID)
	if err != nil {
		return core.TailscaleMetadata{}, err
	}
	if !ok {
		return core.TailscaleMetadata{}, nil
	}
	if !isloClaimTailscaleEnrolled(claim) {
		return core.TailscaleMetadata{}, nil
	}
	sandbox, sandboxErr := client.GetSandbox(ctx, sandboxName)
	if sandboxErr != nil {
		if isloSandboxGoneError(sandboxErr) {
			if err := clearLeaseClaimTailscale(leaseID); err != nil {
				return core.TailscaleMetadata{}, err
			}
			return core.TailscaleMetadata{}, fmt.Errorf("%w: sandbox %s no longer exists", core.ErrTailnetPeerUnavailable, sandboxName)
		}
		return core.TailscaleMetadata{}, fmt.Errorf("%w: get sandbox: %v", core.ErrTailnetPeerValidationUnavailable, sandboxErr)
	}
	if sandbox == nil || isloStatusTerminal(sandbox.GetStatus()) {
		if err := clearLeaseClaimTailscale(leaseID); err != nil {
			return core.TailscaleMetadata{}, err
		}
		return core.TailscaleMetadata{}, fmt.Errorf("%w: sandbox %s is %s", core.ErrTailnetPeerUnavailable, sandboxName, blank(sandboxStatus(sandbox), "missing"))
	}
	if !isloStatusReady(sandbox.GetStatus()) {
		return core.TailscaleMetadata{}, fmt.Errorf("%w: sandbox %s is %s", core.ErrTailnetPeerUnavailable, sandboxName, sandbox.GetStatus())
	}
	stateDir := isloTailscaleStateDir(leaseID)
	req := &gosdk.ExecRequest{
		Command: []string{"bash", "-lc", isloTailscaleHealthCheck},
		Env:     map[string]*string{"TS_STATE_DIR": stringValue(stateDir)},
		User:    stringValue(isloAdminUser),
	}
	var out bytes.Buffer
	code, checkErr := client.ExecStream(ctx, sandboxName, req, &out, b.rt.Stderr)
	if checkErr != nil {
		return core.TailscaleMetadata{}, fmt.Errorf("%w: %v", core.ErrTailnetPeerValidationUnavailable, checkErr)
	}
	if code == 0 {
		if match := isloTailscaleIPRe.FindStringSubmatch(out.String()); match != nil && match[1] != "" {
			if err := updateLeaseClaimTailscale(leaseID, match[1], claim.TailscaleFQDN); err != nil {
				return core.TailscaleMetadata{}, err
			}
			return core.TailscaleMetadata{Enabled: true, IPv4: match[1], FQDN: claim.TailscaleFQDN, State: "ready"}, nil
		}
		return core.TailscaleMetadata{}, fmt.Errorf("%w: health check returned no tailnet address", core.ErrTailnetPeerValidationUnavailable)
	}
	if code == isloTailscaleRecoveryPendingExitCode {
		return core.TailscaleMetadata{}, fmt.Errorf("%w: tailnet recovery is in progress", core.ErrTailnetPeerValidationUnavailable)
	}
	if !repair {
		if err := clearLeaseClaimTailscale(leaseID); err != nil {
			return core.TailscaleMetadata{}, err
		}
		return core.TailscaleMetadata{}, fmt.Errorf("%w: health check reported tailnet unavailable", core.ErrTailnetPeerUnavailable)
	}
	restart := *b
	restart.cfg.Tailscale.Enabled = true
	settings := restart.claimedTailscaleSettings(claim, leaseID, blank(claim.Slug, slug))
	restartErr := restart.runTailscaleBringUp(ctx, client, sandboxName, leaseID, settings)
	if restartErr == nil {
		if err := updateLeaseClaimTailscaleSettings(leaseID, settings.Hostname, settings.Tags, settings.LoginServer, settings.ExitNode, settings.ExitNodeLAN); err != nil {
			return core.TailscaleMetadata{}, err
		}
		updated, _, readErr := resolveLeaseClaim(leaseID)
		if readErr != nil {
			return core.TailscaleMetadata{}, readErr
		}
		return core.TailscaleMetadata{Enabled: true, IPv4: updated.TailscaleIPv4, FQDN: updated.TailscaleFQDN, State: "ready"}, nil
	}
	if errors.Is(restartErr, core.ErrTailnetPeerValidationUnavailable) {
		return core.TailscaleMetadata{}, restartErr
	}
	if err := clearLeaseClaimTailscale(leaseID); err != nil {
		return core.TailscaleMetadata{}, err
	}
	return core.TailscaleMetadata{}, fmt.Errorf("%w: restart failed: %v", core.ErrTailnetPeerUnavailable, restartErr)
}

func isloSandboxGoneError(err error) bool {
	var notFound *gosdk.NotFoundError
	if errors.As(err, &notFound) {
		return true
	}
	var apiErr *islcore.APIError
	return errors.As(err, &apiErr) && (apiErr.StatusCode == 404 || apiErr.StatusCode == 410)
}

func sandboxStatus(sandbox *gosdk.SandboxResponse) string {
	if sandbox == nil {
		return ""
	}
	return sandbox.GetStatus()
}

func (b *isloBackend) ValidateTailnetPeer(ctx context.Context, leaseID string) (core.TailscaleMetadata, error) {
	claim, ok, err := resolveLeaseClaim(leaseID)
	if err != nil {
		return core.TailscaleMetadata{}, err
	}
	if !ok {
		return core.TailscaleMetadata{}, exit(4, "islo lease claim %s not found", leaseID)
	}
	client, err := newIsloClient(b.cfg, b.rt)
	if err != nil {
		return core.TailscaleMetadata{}, fmt.Errorf("%w: %v", core.ErrTailnetPeerValidationUnavailable, err)
	}
	name := strings.TrimPrefix(leaseID, isloLeasePrefix)
	return b.ensureLeaseTailscale(ctx, client, name, claim.Slug, leaseID, false)
}

func isloTailscaleStateDir(leaseID string) string {
	sum := sha256.Sum256([]byte(leaseID))
	return fmt.Sprintf("/var/lib/crabbox/tailscale/%x", sum[:8])
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
