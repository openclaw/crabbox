package islo

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gosdk "github.com/islo-labs/go-sdk"
	core "github.com/openclaw/crabbox/internal/cli"
)

func TestIsloTailscaleHostname(t *testing.T) {
	cases := []struct {
		name string
		cfg  func(*Config)
		slug string
		want string
	}{
		{
			name: "default template",
			cfg:  func(c *Config) {},
			slug: "node-1",
			want: "crabbox-node-1",
		},
		{
			name: "explicit hostname wins over template",
			cfg:  func(c *Config) { c.Tailscale.Hostname = "Build-Box" },
			slug: "node-1",
			want: "build-box",
		},
		{
			name: "template with provider token, sanitized",
			cfg:  func(c *Config) { c.Tailscale.HostnameTemplate = "{provider}_{slug}!" },
			slug: "API gw",
			want: "islo-api-gw",
		},
		{
			name: "template with lease id token",
			cfg:  func(c *Config) { c.Tailscale.HostnameTemplate = "{provider}-{id}" },
			slug: "node-1",
			want: "islo-isb-crabbox-test",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			cfg.Tailscale.HostnameTemplate = "crabbox-{slug}"
			tc.cfg(&cfg)
			if got := isloTailscaleHostname(cfg, "isb_crabbox-test", tc.slug); got != tc.want {
				t.Fatalf("hostname = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsloTailscaleIPRegex(t *testing.T) {
	out := "/tmp/ts.tgz: OKCRABBOX_TS_IP=100.105.127.13trailing"
	m := isloTailscaleIPRe.FindStringSubmatch(out)
	if m == nil || m[1] != "100.105.127.13" {
		t.Fatalf("expected to parse 100.105.127.13, got %v", m)
	}
	if isloTailscaleIPRe.FindStringSubmatch("no ip here") != nil {
		t.Fatalf("expected no match")
	}
}

func TestIsloTailscaleStateDirIsBounded(t *testing.T) {
	leaseID := "isb_crabbox-" + strings.Repeat("long-repository-name-", 20)
	stateDir := isloTailscaleStateDir(leaseID)
	socketPath := filepath.Join(stateDir, "tailscaled.sock")
	if len(socketPath) > 107 {
		t.Fatalf("socket path is %d bytes, want <=107: %q", len(socketPath), socketPath)
	}
	if stateDir != isloTailscaleStateDir(leaseID) {
		t.Fatal("state directory is not deterministic")
	}
	if stateDir == isloTailscaleStateDir(leaseID+"-other") {
		t.Fatal("different lease IDs share a state directory")
	}
	if !strings.HasPrefix(stateDir, "/var/lib/crabbox/tailscale/") {
		t.Fatalf("state directory must survive sandbox pauses: %q", stateDir)
	}
}

func TestIsloTailscaleBringUpScriptIncludesUserspaceProxyAndOptionalFlags(t *testing.T) {
	for _, want := range []string{
		"--tun=userspace-networking",
		"--socks5-server=127.0.0.2:1055",
		"--outbound-http-proxy-listen=127.0.0.2:1055",
		"--state=mem:",
		`if [ "${TS_HAS_AUTH}" != "true" ]; then`,
		`Starting|Running)`,
		`TS_AUTH_VALUE="${TS_AUTHKEY}"`,
		`TS_INSTALL_DIR="$(mktemp -d "${TS_STATE_DIR}/install.XXXXXX")"`,
		`TS_LOCK_FILE="${TS_STATE_DIR}/operation.lock"`,
		`TS_PID_FILE="${TS_STATE_DIR}/tailscaled.pid"`,
		`set -o noclobber`,
		`kill -0 "${lock_pid}"`,
		`*tailscaled*"--socket=${TS_SOCKET}"*`,
		`kill "${old_pid}"`,
		`kill -KILL "${old_pid}"`,
		`printf '%s\n' "$!" >"${TS_PID_FILE}"`,
		`TS_ARCHIVE="${TS_INSTALL_DIR}/tailscale.tgz"`,
		`if [ ! -S "${TS_SOCKET}" ] || ! "${TS_BIN_DIR}/tailscale" --socket="${TS_SOCKET}" status --json >/dev/null 2>&1; then`,
		"for _ in $(seq 1 120)",
		"exit 75",
		`--auth-key="file:/dev/stdin"`,
		`printf '%s' "${TS_AUTH_VALUE}" | "${TS_BIN_DIR}/tailscale" --socket="${TS_SOCKET}" up "$@"`,
		`Stopped) : ;;`,
		"unset TS_AUTHKEY",
		"unset TS_AUTH_VALUE",
		"--shields-up=true",
		defaultIsloTailscaleAMD64SHA256,
		defaultIsloTailscaleARM64SHA256,
		"sha256sum -c -",
		`--login-server="${TS_LOGIN_SERVER}"`,
		`--exit-node="${TS_EXIT_NODE}"`,
		"--exit-node-allow-lan-access",
		`"${TS_BIN_DIR}/tailscale" --socket="${TS_SOCKET}" up "$@"`,
	} {
		if !strings.Contains(isloTailscaleBringUp, want) {
			t.Fatalf("bring-up script missing %q", want)
		}
	}
	if strings.Contains(isloTailscaleBringUp, `--authkey="${TS_AUTHKEY}"`) {
		t.Fatal("bring-up script must not expose the auth key in tailscale argv")
	}
	if strings.Contains(isloTailscaleBringUp, "pkill") {
		t.Fatal("bring-up script must target only the daemon for this lease")
	}
	if strings.Contains(isloTailscaleBringUp, "/auth.") || strings.Contains(isloTailscaleBringUp, "TS_AUTH_FILE") {
		t.Fatal("bring-up script must not persist a snapshot-clonable auth key file")
	}
	if strings.Contains(isloTailscaleBringUp, "tailscaled.state") {
		t.Fatal("bring-up script must not persist snapshot-clonable node identity")
	}
	if strings.Contains(isloTailscaleBringUp, `Starting|Running|NeedsMachineAuth`) || strings.Contains(isloTailscaleBringUp, `Starting|Running|"")`) {
		t.Fatal("machine approval and empty backend states must fail closed")
	}
	if strings.Index(isloTailscaleBringUp, "unset TS_AUTHKEY") > strings.Index(isloTailscaleBringUp, `setsid "${TS_BIN_DIR}/tailscaled"`) {
		t.Fatal("bring-up script must unset the auth key before starting tailscaled")
	}
	if strings.Contains(isloTailscaleBringUp, "--socks5-server=127.0.0.1:") || strings.Contains(isloTailscaleBringUp, "--outbound-http-proxy-listen=127.0.0.1:") {
		t.Fatal("outbound proxies must not bind the loopback address used for tailnet ingress")
	}
	if strings.Contains(isloTailscaleBringUp, "/tmp/ts") {
		t.Fatal("root bootstrap artifacts must stay out of workload-writable /tmp paths")
	}
	verifyAt := strings.Index(isloTailscaleBringUp, "sha256sum -c -")
	extractAt := strings.Index(isloTailscaleBringUp, `tar -xzf "${TS_ARCHIVE}"`)
	if verifyAt < 0 || extractAt < 0 || verifyAt > extractAt {
		t.Fatal("bring-up script must verify the archive before extraction")
	}
	for name, script := range map[string]string{"bring-up": isloTailscaleBringUp, "health": isloTailscaleHealthCheck} {
		if !strings.Contains(script, `"BackendState"`) || !strings.Contains(script, `"Running"`) {
			t.Fatalf("%s script must require a running Tailscale backend", name)
		}
	}
	if !strings.Contains(isloTailscaleHealthCheck, `TS_LOCK_FILE="${TS_STATE_DIR}/operation.lock"`) ||
		!strings.Contains(isloTailscaleHealthCheck, `kill -0 "${lock_pid}"`) ||
		!strings.Contains(isloTailscaleHealthCheck, "exit 75") {
		t.Fatal("health check must validate the recovery lock owner")
	}
}

func TestIsloTailscaleBringUpScriptIsValidBash(t *testing.T) {
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(isloTailscaleBringUp)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bring-up script syntax: %v\n%s", err, out)
	}
}

func TestEnsureLeaseTailscaleRevalidatesAsRoot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-node-a"
	if err := claimLeaseForRepoProvider(leaseID, "node-a", isloProvider, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscale(leaseID, "100.64.7.7", ""); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{execOut: "CRABBOX_TS_IP=100.64.7.8"}
	backend := &isloBackend{rt: Runtime{Stderr: io.Discard}}

	meta, err := backend.ensureLeaseTailscale(context.Background(), client, "crabbox-node-a", "node-a", leaseID, false)
	if err != nil {
		t.Fatal(err)
	}
	if meta.IPv4 != "100.64.7.8" || meta.State != "ready" {
		t.Fatalf("metadata=%#v", meta)
	}
	if len(client.execRequests) != 1 || client.execRequests[0].GetUser() == nil || *client.execRequests[0].GetUser() != isloAdminUser {
		t.Fatalf("health request must run as root: %#v", client.execRequests)
	}
}

func TestEnsureLeaseTailscaleResumesPausedSandboxBeforeRepair(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-node-a"
	if err := claimLeaseForRepoProvider(leaseID, "node-a", isloProvider, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscale(leaseID, "100.64.7.7", ""); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{
		getSandbox: &gosdk.SandboxResponse{Name: "crabbox-node-a", Status: "paused"},
		execOut:    "CRABBOX_TS_IP=100.64.7.8",
	}
	backend := &isloBackend{rt: Runtime{Stderr: io.Discard}}

	meta, err := backend.ensureLeaseTailscale(context.Background(), client, "crabbox-node-a", "node-a", leaseID, true)
	if err != nil {
		t.Fatal(err)
	}
	if client.resumeCalls != 1 || meta.IPv4 != "100.64.7.8" {
		t.Fatalf("resume calls=%d metadata=%#v", client.resumeCalls, meta)
	}
}

func TestIsloStatusDoesNotEnrollPlainLeaseFromAmbientConfig(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-node-a"
	if err := claimLeaseForRepoProvider(leaseID, "node-a", isloProvider, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{}
	restore := swapNewIsloClient(client)
	defer restore()
	backend := &isloBackend{
		cfg: Config{
			Islo:      IsloConfig{APIKey: "test"},
			Tailscale: core.TailscaleConfig{Enabled: true, AuthKey: "tskey-secret"},
		},
		rt: Runtime{Stderr: io.Discard},
	}

	if _, err := backend.Status(context.Background(), StatusRequest{ID: leaseID}); err != nil {
		t.Fatal(err)
	}
	if len(client.execRequests) != 0 {
		t.Fatalf("status enrolled a plain lease: %#v", client.prepareCommands)
	}
	claim, ok, err := resolveLeaseClaim(leaseID)
	if err != nil || !ok {
		t.Fatalf("resolve claim ok=%t err=%v", ok, err)
	}
	if isloClaimTailscaleEnrolled(claim) {
		t.Fatalf("status persisted unexpected Tailscale enrollment: %#v", claim)
	}
}

func TestEnsureLeaseTailscaleClearsDeadClaimWithoutAuthKey(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-node-a"
	if err := claimLeaseForRepoProvider(leaseID, "node-a", isloProvider, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscale(leaseID, "100.64.7.7", ""); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{execCode: 1}
	backend := &isloBackend{rt: Runtime{Stderr: io.Discard}}

	if _, err := backend.ensureLeaseTailscale(context.Background(), client, "crabbox-node-a", "node-a", leaseID, true); err == nil {
		t.Fatal("expected dead daemon error")
	}
	claim, ok, err := resolveLeaseClaim(leaseID)
	if err != nil || !ok {
		t.Fatalf("resolve claim ok=%t err=%v", ok, err)
	}
	if claim.TailscaleIPv4 != "" || claim.Labels["tailscale"] != "" {
		t.Fatalf("stale tailnet metadata not cleared: %#v", claim)
	}
}

func TestEnsureLeaseTailscaleReadOnlyValidationDoesNotRepair(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-node-a"
	if err := claimLeaseForRepoProvider(leaseID, "node-a", isloProvider, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscale(leaseID, "100.64.7.7", ""); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscaleSettings(leaseID, "node-a", nil, "", "", false); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{execCode: 1}
	backend := &isloBackend{rt: Runtime{Stderr: io.Discard}}

	if _, err := backend.ensureLeaseTailscale(context.Background(), client, "crabbox-node-a", "node-a", leaseID, false); !errors.Is(err, core.ErrTailnetPeerUnavailable) {
		t.Fatalf("expected unavailable tailnet, got %v", err)
	}
	if len(client.execRequests) != 1 {
		t.Fatalf("read-only validation attempted repair: requests=%d", len(client.execRequests))
	}
	claim, ok, err := resolveLeaseClaim(leaseID)
	if err != nil || !ok {
		t.Fatalf("resolve claim ok=%t err=%v", ok, err)
	}
	if claim.TailscaleIPv4 != "" || claim.TailscaleHostname != "node-a" {
		t.Fatalf("read-only validation should clear endpoint but preserve enrollment: %#v", claim)
	}
}

func TestEnsureLeaseTailscaleBoundsHealthExec(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-node-a"
	if err := claimLeaseForRepoProvider(leaseID, "node-a", isloProvider, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscale(leaseID, "100.64.7.7", ""); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{
		execOut:             "CRABBOX_TS_IP=100.64.7.7",
		execDeadlineCommand: `"BackendState"`,
	}
	backend := &isloBackend{rt: Runtime{Stderr: io.Discard}}
	started := time.Now()

	if _, err := backend.ensureLeaseTailscale(context.Background(), client, "crabbox-node-a", "node-a", leaseID, false); err != nil {
		t.Fatal(err)
	}
	if client.execDeadline.IsZero() {
		t.Fatal("health exec has no deadline")
	}
	remaining := client.execDeadline.Sub(started)
	if remaining <= 0 || remaining > isloTailscaleHealthTimeout+time.Second {
		t.Fatalf("health deadline remaining=%s want <=%s", remaining, isloTailscaleHealthTimeout)
	}
}

func TestEnsureLeaseTailscaleReadOnlyValidationPreservesClaimDuringRecovery(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-node-a"
	if err := claimLeaseForRepoProvider(leaseID, "node-a", isloProvider, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscale(leaseID, "100.64.7.7", ""); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{execCode: isloTailscaleRecoveryPendingExitCode}
	backend := &isloBackend{rt: Runtime{Stderr: io.Discard}}

	if _, err := backend.ensureLeaseTailscale(context.Background(), client, "crabbox-node-a", "node-a", leaseID, false); !errors.Is(err, core.ErrTailnetPeerValidationUnavailable) {
		t.Fatalf("expected recovery-pending validation error, got %v", err)
	}
	claim, ok, err := resolveLeaseClaim(leaseID)
	if err != nil || !ok || claim.TailscaleIPv4 != "100.64.7.7" {
		t.Fatalf("recovery validation erased claim: ok=%t err=%v claim=%#v", ok, err, claim)
	}
}

func TestEnsureLeaseTailscalePreservesClaimWhenValidationCannotRun(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-node-a"
	if err := claimLeaseForRepoProvider(leaseID, "node-a", isloProvider, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscale(leaseID, "100.64.7.7", ""); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{execErr: errors.New("API unavailable")}
	backend := &isloBackend{rt: Runtime{Stderr: io.Discard}}

	if _, err := backend.ensureLeaseTailscale(context.Background(), client, "crabbox-node-a", "node-a", leaseID, false); !errors.Is(err, core.ErrTailnetPeerValidationUnavailable) {
		t.Fatalf("expected validation unavailable, got %v", err)
	}
	claim, ok, err := resolveLeaseClaim(leaseID)
	if err != nil || !ok {
		t.Fatalf("resolve claim ok=%t err=%v", ok, err)
	}
	if claim.TailscaleIPv4 != "100.64.7.7" {
		t.Fatalf("validation failure erased healthy claim: %#v", claim)
	}
}

func TestEnsureLeaseTailscalePreservesClaimWhileRecoveryStarts(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-node-a"
	if err := claimLeaseForRepoProvider(leaseID, "node-a", isloProvider, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscale(leaseID, "100.64.7.7", ""); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{execCodes: []int{1, isloTailscaleRecoveryPendingExitCode}}
	backend := &isloBackend{rt: Runtime{Stderr: io.Discard}}

	if _, err := backend.ensureLeaseTailscale(context.Background(), client, "crabbox-node-a", "node-a", leaseID, true); !errors.Is(err, core.ErrTailnetPeerValidationUnavailable) {
		t.Fatalf("expected recovery-pending validation error, got %v", err)
	}
	claim, ok, err := resolveLeaseClaim(leaseID)
	if err != nil || !ok || claim.TailscaleIPv4 != "100.64.7.7" {
		t.Fatalf("recovery timeout erased claim: ok=%t err=%v claim=%#v", ok, err, claim)
	}
}

func TestEnsureLeaseTailscaleClearsClaimForMissingSandbox(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-node-a"
	if err := claimLeaseForRepoProvider(leaseID, "node-a", isloProvider, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscale(leaseID, "100.64.7.7", ""); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{getSandboxGone: true}
	backend := &isloBackend{rt: Runtime{Stderr: io.Discard}}

	if _, err := backend.ensureLeaseTailscale(context.Background(), client, "crabbox-node-a", "node-a", leaseID, false); !errors.Is(err, core.ErrTailnetPeerUnavailable) {
		t.Fatalf("expected missing sandbox to be unavailable, got %v", err)
	}
	claim, ok, err := resolveLeaseClaim(leaseID)
	if err != nil || !ok {
		t.Fatalf("resolve claim ok=%t err=%v", ok, err)
	}
	if claim.TailscaleIPv4 != "" || claim.Labels["tailscale"] != "" {
		t.Fatalf("missing sandbox retained stale tailnet metadata: %#v", claim)
	}
}

func TestEnsureLeaseTailscaleReenrollsWithPersistedSettings(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-node-a"
	if err := claimLeaseForRepoProviderWithPond(leaseID, "node-a", isloProvider, "mesh-demo", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscale(leaseID, "100.64.7.7", ""); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscaleSettings(
		leaseID,
		"original-node",
		[]string{"tag:original"},
		"https://control.example.com",
		"exit.example.com",
		true,
	); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{
		execCodes: []int{1, 0},
		execOuts:  []string{"", "CRABBOX_TS_IP=100.64.7.8"},
	}
	backend := &isloBackend{
		cfg: Config{Tailscale: core.TailscaleConfig{
			AuthKey:  "tskey-recovery",
			Hostname: "ambient-node",
			Tags:     []string{"tag:ambient"},
			ExitNode: "ambient-exit.example.com",
		}},
		rt: Runtime{Stderr: io.Discard},
	}

	meta, err := backend.ensureLeaseTailscale(context.Background(), client, "crabbox-node-a", "node-a", leaseID, true)
	if err != nil {
		t.Fatal(err)
	}
	if meta.IPv4 != "100.64.7.8" || len(client.execRequests) != 2 {
		t.Fatalf("restart metadata=%#v requests=%d", meta, len(client.execRequests))
	}
	restartReq := client.execRequests[1]
	if got := *restartReq.Env["TS_AUTHKEY"]; got != "tskey-recovery" {
		t.Fatalf("re-enrollment auth key=%q", got)
	}
	for key, want := range map[string]string{
		"TS_HOST":                "original-node",
		"TS_TAGS":                "tag:original",
		"TS_LOGIN_SERVER":        "https://control.example.com",
		"TS_EXIT_NODE":           "exit.example.com",
		"TS_EXIT_NODE_ALLOW_LAN": "true",
	} {
		if got := *restartReq.Env[key]; got != want {
			t.Fatalf("restart %s=%q want %q", key, got, want)
		}
	}
}

func TestEnsureLeaseTailscaleRejoinsFromSavedEnrollmentSettings(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "isb_crabbox-node-a"
	if err := claimLeaseForRepoProvider(leaseID, "node-a", isloProvider, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := updateLeaseClaimTailscaleSettings(leaseID, "saved-node", []string{"tag:saved"}, "", "", false); err != nil {
		t.Fatal(err)
	}
	client := &fakeIsloSyncClient{
		execCodes: []int{1, 0},
		execOuts:  []string{"", "CRABBOX_TS_IP=100.64.7.9"},
	}
	backend := &isloBackend{
		cfg: Config{Tailscale: core.TailscaleConfig{AuthKey: "tskey-new"}},
		rt:  Runtime{Stderr: io.Discard},
	}

	meta, err := backend.ensureLeaseTailscale(context.Background(), client, "crabbox-node-a", "node-a", leaseID, true)
	if err != nil {
		t.Fatal(err)
	}
	if meta.IPv4 != "100.64.7.9" || len(client.execRequests) != 2 {
		t.Fatalf("rejoin metadata=%#v requests=%d", meta, len(client.execRequests))
	}
	if got := *client.execRequests[1].Env["TS_HOST"]; got != "saved-node" {
		t.Fatalf("rejoin hostname=%q want saved-node", got)
	}
}

func TestIsloTailscaleArchiveVerification(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "tailscale.tgz")
	content := []byte("archive")
	if err := os.WriteFile(archive, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	run := func(expected string) error {
		cmd := exec.Command("bash", "-lc", isloTailscaleVerifyArchive)
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"TS_ARCHIVE=" + archive,
			"TS_SHA256=" + expected,
		}
		return cmd.Run()
	}
	if err := run(fmt.Sprintf("%x", sum)); err != nil {
		t.Fatalf("matching checksum rejected: %v", err)
	}
	if err := run(strings.Repeat("0", 64)); err == nil {
		t.Fatal("mismatched checksum accepted")
	}
}
