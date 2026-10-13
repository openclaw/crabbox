package cli

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// GuestLeaseExpiryFields is the nonsecret bootstrap/refresh contract. Keep only
// controls automatic release after run; it never changes the expiry decision.
type GuestLeaseExpiryFields struct {
	Crabbox   string
	State     string
	Keep      string
	ExpiresAt string
}

func GuestLeaseExpiryFromLabels(labels map[string]string) GuestLeaseExpiryFields {
	return GuestLeaseExpiryFields{labels["crabbox"], labels["state"], labels["keep"], labels["expires_at"]}
}

// WithGuestLeaseExpiry binds file-backed bootstrap to the actual allocation.
func WithGuestLeaseExpiry(cfg Config, labels map[string]string) Config {
	fields := GuestLeaseExpiryFromLabels(labels)
	cfg.guestLeaseExpiry = &fields
	return cfg
}

// guestExpiryAdapter enumerates shared-cloud-init Linux providers. Metadata and
// reclaim differences stay here rather than leaking into the decision policy.
func guestExpiryAdapter(cfg Config) string {
	if cfg.TargetOS != "" && cfg.TargetOS != targetLinux {
		return ""
	}
	if p, err := ProviderFor(cfg.Provider); err == nil && ShouldUseCoordinator(cfg, p.Spec()) {
		return ""
	}
	switch cfg.Provider {
	case "gcp", "aws", "azure":
		return cfg.Provider
	case "hetzner", "digitalocean", "vultr", "linode", "ovh", "scaleway", "nebius", "lambda", "tencentcloud", "incus", "multipass", "firecracker":
		return "file"
	}
	return ""
}

// POSIX shell counterpart of shouldCleanupServer. Tests execute the same table
// through both implementations. Inputs are data, never evaluated as shell code.
const guestExpiryDecision = `expiry_due() {
  [ "$crabbox" = true ] || return 1
  case "$state" in
    failed|released|expired) return 0 ;;
  esac
  case "$expires_at" in
    ''|*[!0-9]*)
      case "$expires_at" in
        ????-??-??T??:??:??*Z|????-??-??T??:??:??*+??:??|????-??-??T??:??:??*-??:??)
          expires_at=$(date -u -d "$expires_at" +%s 2>/dev/null) || return 1 ;;
        *) return 1 ;;
      esac ;;
  esac
  expires_at=$(printf '%s' "$expires_at" | sed 's/^0*//')
  # Bound arithmetic to a safe signed range, including the grace addition.
  [ -n "$expires_at" ] && [ "${#expires_at}" -le 12 ] && [ "$expires_at" -gt 0 ] || return 1
  case "$state" in
    running|provisioning) [ "$now" -gt "$((expires_at + 3600))" ] ;;
    *) [ "$now" -gt "$expires_at" ] ;;
  esac
}
`

const guestExpiryRequest = `request() { curl --connect-timeout 5 --max-time 15 -fsS "$@"; }
`

const guestExpiryGCPSource = `metadata() {
  request -H 'Metadata-Flavor: Google' "http://metadata.google.internal/computeMetadata/v1/$1"
}
project="$(metadata project/project-id)" || exit 0
name="$(metadata instance/name)" || exit 0
zone_path="$(metadata instance/zone)" || exit 0
zone="${zone_path##*/}"
token_json="$(metadata instance/service-accounts/default/token)" || exit 0
token="$(printf '%s' "$token_json" | jq -er '.access_token | select(type == "string" and length > 0)')" || exit 0
[ -n "$project" ] && [ -n "$name" ] && [ -n "$zone" ] || exit 0
instance="$(request -H "Authorization: Bearer $token" "https://compute.googleapis.com/compute/v1/projects/$project/zones/$zone/instances/$name")" || exit 0
label() { printf '%s' "$instance" | jq -er --arg key "$1" '.labels[$key] // "" | select(type == "string")'; }
provider="$(label provider)" || exit 0
[ -z "$provider" ] || [ "$provider" = gcp ] || exit 0
`
const guestExpiryAWSSource = `metadata_token="$(request -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token)" || exit 0
[ -n "$metadata_token" ] || exit 0
label() {
  request -H "X-aws-ec2-metadata-token: $metadata_token" "http://169.254.169.254/latest/meta-data/tags/instance/$1"
}
`
const guestExpiryAzureSource = `instance="$(request -H 'Metadata: true' --noproxy '*' 'http://169.254.169.254/metadata/instance?api-version=2021-02-01')" || exit 0
label() {
  printf '%s' "$instance" | jq -er --arg key "$1" '[.compute.tagsList[] | select(.name == $key) | .value] | if length == 1 and (.[0] | type == "string") then .[0] else error("missing or ambiguous tag") end'
}
`
const guestExpiryFileSource = `file=/etc/crabbox/lease-expiry
[ -f "$file" ] && [ ! -L "$file" ] || exit 0
[ "$(stat -c '%u:%a' "$file")" = '0:600' ] || exit 0
file_fields="$(cat "$file")" || exit 0
label() {
  printf '%s\n' "$file_fields" | awk -F= -v key="$1" '$1 == key { count++; value=substr($0, length(key)+2) } END { if (count != 1) exit 1; print value }'
}
`

func guestExpiryScript(adapter string) string {
	source := guestExpiryFileSource
	reclaim := "systemctl poweroff\n"
	switch adapter {
	case "gcp":
		source = guestExpiryRequest + guestExpiryGCPSource
		reclaim = `request -X DELETE -H "Authorization: Bearer $token" "https://compute.googleapis.com/compute/v1/projects/$project/zones/$zone/instances/$name" >/dev/null` + "\n"
	case "aws":
		source = guestExpiryRequest + guestExpiryAWSSource
	case "azure":
		source = guestExpiryRequest + guestExpiryAzureSource
	}
	return "#!/bin/sh\nset -eu\n" + guestExpiryDecision + source + `crabbox="$(label crabbox)" || exit 0
state="$(label state)" || exit 0
state="$(printf '%s' "$state" | tr '[:upper:]' '[:lower:]')" || exit 0
expires_at="$(label expires_at)" || exit 0
now="$(date -u +%s)"
expiry_due || exit 0
logger -t crabbox-expiry-guard 'reclaiming expired direct lease' || true
` + reclaim
}

func guestExpiryFile(fields GuestLeaseExpiryFields) (string, error) {
	if fields.Crabbox != "true" {
		return "", fmt.Errorf("guest lease expiry requires crabbox=true")
	}
	if fields.Keep != "true" && fields.Keep != "false" {
		return "", fmt.Errorf("invalid guest lease keep value")
	}
	switch fields.State {
	case "leased", "ready", "active", "running", "provisioning", "failed", "released", "expired":
	default:
		return "", fmt.Errorf("invalid guest lease state")
	}
	expires, ok := parseLeaseLabelTime(fields.ExpiresAt)
	if !ok || expires.Unix() <= 0 || expires.Unix() > 999999999999 {
		return "", fmt.Errorf("invalid guest lease expiry")
	}
	return fmt.Sprintf("crabbox=true\nstate=%s\nkeep=%s\nexpires_at=%d\n", fields.State, fields.Keep, expires.Unix()), nil
}

func cloudInitGuestExpiryGuardFiles(cfg Config) string {
	adapter := guestExpiryAdapter(cfg)
	if adapter == "" {
		return ""
	}
	files := guestExpiryCloudFile("/usr/local/sbin/crabbox-expiry-guard", "0755", guestExpiryScript(adapter)) +
		guestExpiryCloudFile("/etc/systemd/system/crabbox-expiry-guard.service", "0644", `[Unit]
Description=Crabbox direct lease expiry guard
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=root
ExecStart=/usr/local/sbin/crabbox-expiry-guard
`) + guestExpiryCloudFile("/etc/systemd/system/crabbox-expiry-guard.timer", "0644", `[Unit]
Description=Run Crabbox direct lease expiry guard

[Timer]
OnBootSec=2min
OnUnitActiveSec=2min
Persistent=true

[Install]
WantedBy=timers.target
`)
	if adapter == "file" && cfg.guestLeaseExpiry != nil {
		if content, err := guestExpiryFile(*cfg.guestLeaseExpiry); err == nil {
			files += guestExpiryCloudFile("/etc/crabbox/lease-expiry", "0600", content)
		}
	}
	return strings.TrimRight(files, "\n")
}

func guestExpiryCloudFile(path, mode, content string) string {
	return fmt.Sprintf("  - path: %s\n    owner: root:root\n    permissions: '%s'\n    content: |\n%s\n", path, mode, "      "+strings.ReplaceAll(strings.TrimRight(content, "\n"), "\n", "\n      "))
}

func cloudInitGuestExpiryGuardBootstrap(cfg Config) string {
	if guestExpiryAdapter(cfg) == "" {
		return ""
	}
	return "    systemctl daemon-reload\n    systemctl enable --now crabbox-expiry-guard.timer"
}

// RefreshGuestLeaseExpiry is the extension hook for file-backed providers. It
// uses the existing authenticated SSH transport; the old deadline remains
// intact on failed writes, and readers see either complete version.
func RefreshGuestLeaseExpiry(ctx context.Context, target SSHTarget, fields GuestLeaseExpiryFields) error {
	if target.TargetOS != "" && target.TargetOS != targetLinux {
		return fmt.Errorf("guest expiry refresh requires Linux")
	}
	content, err := guestExpiryFile(fields)
	if err != nil {
		return err
	}
	checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	return refreshGuestLeaseExpirySSH(ctx, target, "sudo -n sh -c "+shellQuote(guestExpiryRefreshScript)+" crabbox-expiry "+shellQuote(checksum), content)
}

var refreshGuestLeaseExpirySSH = runSSHInputQuiet

const guestExpiryRefreshScript = `set -eu
[ ! -L /etc/crabbox ]
install -d -o root -g root -m 0755 /etc/crabbox
file=$(mktemp /etc/crabbox/.lease-expiry.XXXXXX)
trap 'rm -f -- "$file"' EXIT HUP INT TERM
cat >"$file"
# Never publish a truncated SSH input stream.
checksum=$(sha256sum "$file")
[ "${checksum%% *}" = "$1" ]
chown root:root "$file"
chmod 0600 "$file"
mv -fT -- "$file" /etc/crabbox/lease-expiry
trap - EXIT HUP INT TERM
`

// Keep arithmetic in the same range as the shell guard.
func guestExpiryTimeValid(t time.Time) bool {
	return t.Unix() > 0 && len(strconv.FormatInt(t.Unix(), 10)) <= 12
}

// Keep file-backed guards in sync with ordinary idle/state touches as well as
// explicit lifetime updates. Metadata-backed guards read the provider directly.
func touchLeaseWithGuestExpiry(ctx context.Context, backend LeaseTouchBackend, cfg Config, req TouchRequest) (Server, error) {
	server, err := backend.Touch(ctx, req)
	if err != nil || guestExpiryAdapter(cfg) != "file" {
		return server, err
	}
	target, err := resolveGuestLeaseExpiryTarget(ctx, backend, cfg, req.Lease)
	if err != nil {
		return server, err
	}
	if err := RefreshGuestLeaseExpiry(ctx, target, GuestLeaseExpiryFromLabels(server.Labels)); err != nil {
		return server, fmt.Errorf("refresh guest lease expiry: %w", err)
	}
	return server, nil
}

// Resolve before taking a claim mutation fence: provider resolution may read
// the claim. Both touch and extend use the same authenticated SSH route.
func resolveGuestLeaseExpiryTarget(ctx context.Context, backend Backend, cfg Config, lease LeaseTarget) (SSHTarget, error) {
	target := lease.SSH
	if target.Host == "" {
		resolver, ok := backend.(SSHLoginBackend)
		if !ok {
			return target, fmt.Errorf("provider cannot resolve guest expiry SSH target")
		}
		lease, resolveErr := resolver.Resolve(ctx, ResolveRequest{ID: lease.LeaseID, Options: leaseOptionsFromConfig(cfg), NoLocalStateMutations: true})
		if resolveErr != nil {
			return target, resolveErr
		}
		target = lease.SSH
	}
	return target, nil
}
