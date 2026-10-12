package proxmox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProxmoxPreparedCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		desktop, browser     bool
		desktopID, browserID int
		target, env, want    string
	}{
		{name: "headless", target: "linux"},
		{name: "desktop disabled", desktop: true, target: "linux", want: "desktopTemplateId"},
		{name: "browser disabled", browser: true, target: "linux", want: "browserTemplateId"},
		{name: "wrong template", desktop: true, desktopID: 9401, target: "linux", want: "desktopTemplateId"},
		{name: "desktop", desktop: true, desktopID: 9400, target: "linux"},
		{name: "browser only", browser: true, browserID: 9400, target: "linux"},
		{name: "both", desktop: true, browser: true, desktopID: 9400, browserID: 9400, target: "linux"},
		{name: "windows", desktop: true, desktopID: 9400, target: "windows", want: "target=linux"},
		{name: "wayland", desktop: true, desktopID: 9400, target: "linux", env: "wayland", want: "XFCE/X11"},
		{name: "gnome", desktop: true, desktopID: 9400, target: "linux", env: "gnome", want: "XFCE/X11"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := core.Config{TargetOS: tc.target, Desktop: tc.desktop, Browser: tc.browser, DesktopEnv: tc.env, Proxmox: core.ProxmoxConfig{TemplateID: 9400, DesktopTemplateID: tc.desktopID, BrowserTemplateID: tc.browserID}}
			err := (Provider{}).ValidateRequestedCapabilities(cfg)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err=%v want=%q", err, tc.want)
			}
			features := core.ProviderFeaturesForConfig(Provider{}, cfg)
			if features.Has(core.FeatureDesktop) != (tc.desktopID == 9400 && tc.target == "linux") || features.Has(core.FeatureBrowser) != (tc.browserID == 9400 && tc.target == "linux") {
				t.Fatalf("features=%v", features)
			}
		})
	}
	if (Provider{}).Spec().Features.Has(core.FeatureDesktop) {
		t.Fatal("unconfigured catalog advertises desktop")
	}
}

func TestProxmoxDesktopAdapterReplay(t *testing.T) {
	_, client, _ := fixedProxmoxFixture(t)
	cfg := controllerTestConfig()
	setControllerTestEnv(t, cfg)
	t.Setenv("CRABBOX_PROXMOX_DESKTOP_TEMPLATE_ID", "9400")
	t.Setenv("CRABBOX_PROXMOX_BROWSER_TEMPLATE_ID", "9400")
	const leaseID = "cbx_0123456789ab"
	args := []string{"warmup", "--provider", "proxmox", "--keep=true", "--lease-id", leaseID, "--slug", "desktop-box", "--desktop", "--browser"}
	for range 2 {
		if _, err := runControllerTestCommand(t, args...); err != nil {
			t.Fatal(err)
		}
	}
	if client.fixedCreates != 1 {
		t.Fatalf("clones=%d", client.fixedCreates)
	}
	for _, labels := range []map[string]string{client.servers[0].Labels, readFixedProxmoxClaim(t, leaseID).Labels} {
		for key, want := range map[string]string{"desktop": "true", "browser": "true", "desktop_env": "xfce"} {
			if labels[key] != want {
				t.Fatalf("%s=%q want %q", key, labels[key], want)
			}
		}
	}
	out, err := runControllerTestCommand(t, "inspect", "--provider", "proxmox", "--id", leaseID, "--json")
	if err != nil || !strings.Contains(out, `"desktop": "true"`) && !strings.Contains(out, `"desktop":"true"`) {
		t.Fatalf("inspect err=%v out=%s", err, out)
	}
	if _, err := runControllerTestCommand(t, args[:len(args)-2]...); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("incompatible replay err=%v", err)
	}
	if client.fixedCreates != 1 {
		t.Fatal("incompatible replay cloned again")
	}
}

func TestProxmoxPreparedHeadlessUnchanged(t *testing.T) {
	cfg := controllerTestConfig()
	cfg.Proxmox.DesktopTemplateID, cfg.Proxmox.BrowserTemplateID = 9400, 9400
	setup, checks := preparedTemplateBootstrap(cfg)
	if setup != "" || checks != "" {
		t.Fatal("headless bootstrap acquired desktop work")
	}
}

// Execute the generated guest probe with fake service/process/socket commands;
// no local services or credentials are consulted.
func TestProxmoxPreparedReadiness(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("guest probes require POSIX shell")
	}
	for _, problem := range []string{"", "space-argument", "service", "xfce", "display", "permission", "password", "public-vnc", "no-vnc", "no-auth", "browser", "wrapper"} {
		t.Run(problem, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			write := func(path, content string, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(path, []byte(content), mode); err != nil {
					t.Fatal(err)
				}
			}
			cfg := controllerTestConfig()
			cfg.Desktop, cfg.Browser = true, true
			_, checks := preparedTemplateBootstrap(cfg)
			checks = strings.ReplaceAll(checks, "/var/lib/crabbox", dir)
			checks = strings.ReplaceAll(checks, "/usr/local/bin/crabbox-browser", filepath.Join(bin, "crabbox-browser"))
			write(filepath.Join(dir, "vnc.password"), "fixture1", 0600)
			write(filepath.Join(dir, "vnc.pass"), "fixture1", 0600)
			write(filepath.Join(dir, "browser.env"), "BROWSER="+filepath.Join(bin, "crabbox-browser")+"\nCHROME_BIN="+filepath.Join(bin, "crabbox-browser")+"\n", 0600)
			scripts := map[string]string{
				"systemctl":       `[ "$PROBLEM" != service ]`,
				"xdpyinfo":        `[ "$PROBLEM" != display ]`,
				"stat":            `[ "$PROBLEM" != permission ] && echo '600:crabbox' || echo '644:crabbox'`,
				"tigervncpasswd":  `[ "$PROBLEM" != password ] || exit 1; cat`,
				"crabbox-browser": `[ "$PROBLEM" != browser ]`,
				"pgrep": `if [ "$PROBLEM" = xfce ] && [ "$2" = xfce4-session ]; then exit 1; fi
if [ "$1" = -a ]; then
 if [ "$PROBLEM" = no-auth ]; then echo '1 Xtigervnc :99 -SecurityTypes None'; elif [ "$PROBLEM" = space-argument ]; then echo '1 Xtigervnc :99 -SecurityTypes VncAuth -PasswordFile ` + dir + `/vnc.pass'; else echo '1 Xtigervnc :99 -SecurityTypes VncAuth -PasswordFile=` + dir + `/vnc.pass'; fi
fi`,
				"ss": `[ "$PROBLEM" != no-vnc ] || exit 0
 echo 'LISTEN 0 10 127.0.0.1:5900 0.0.0.0:*'
 [ "$PROBLEM" != public-vnc ] || echo 'LISTEN 0 10 0.0.0.0:5900 0.0.0.0:*'
 exit 0`,
			}
			for name, script := range scripts {
				write(filepath.Join(bin, name), "#!/bin/bash\n"+script+"\n", 0700)
			}
			if problem == "wrapper" {
				write(filepath.Join(dir, "browser.env"), "BROWSER=/bin/true\nCHROME_BIN=/bin/true\n", 0600)
			}
			command := exec.Command("bash", "-c", "set -euo pipefail\n"+checks)
			command.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "PROBLEM="+problem)
			output, err := command.CombinedOutput()
			if problem == "" || problem == "space-argument" {
				if err != nil {
					t.Fatalf("success probe: %v %s", err, output)
				}
				return
			}
			if err == nil || !strings.Contains(string(output), "Proxmox template missing or invalid") {
				t.Fatalf("problem=%s err=%v output=%s", problem, err, output)
			}
		})
	}
}

func TestProxmoxTemplateCapabilityScope(t *testing.T) {
	cfg := controllerTestConfig()
	before := controllerTestScope(t, cfg)
	cfg.Proxmox.DesktopTemplateID = 9400
	if controllerTestScope(t, cfg) == before {
		t.Fatal("desktop enablement did not change immutable scope")
	}
	before = controllerTestScope(t, cfg)
	cfg.Proxmox.BrowserTemplateID = 9400
	if controllerTestScope(t, cfg) == before {
		t.Fatal("browser enablement did not change immutable scope")
	}
}

func TestProxmoxDesktopLabelTamperingConflicts(t *testing.T) {
	backend, client, req := fixedProxmoxFixture(t)
	backend.Cfg.Desktop, backend.Cfg.Browser = true, true
	backend.Cfg.Proxmox.DesktopTemplateID, backend.Cfg.Proxmox.BrowserTemplateID = 9400, 9400
	if _, err := backend.Acquire(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	client.servers[0].Labels["desktop"] = "false"
	if _, err := backend.Acquire(t.Context(), req); err == nil || !strings.Contains(err.Error(), "lease_id_conflict") {
		t.Fatalf("capability tampering accepted: %v", err)
	}
}

func TestProxmoxPreparedDesktopSurvivesReboot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("guest bootstrap requires POSIX shell")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := controllerTestConfig()
	cfg.Desktop = true
	setup, checks := preparedTemplateBootstrap(cfg)
	setup = strings.ReplaceAll(setup, "/var/lib/crabbox", dir)
	checks = strings.ReplaceAll(checks, "/var/lib/crabbox", dir)
	mock := `#!/bin/bash
set -eu
case "${0##*/}" in
  startxfce4|Xtigervnc|xdpyinfo|chown) exit 0 ;;
  id) echo crabbox ;;
  install) /bin/mkdir -p "${@: -1}" ;;
  openssl) echo fixture-password ;;
  tigervncpasswd) /bin/cat ;;
  stat) echo '600:crabbox' ;;
  mv) shift; /bin/mv "$@" ;;
  runuser) shift 3; exec "$@" ;;
  pgrep)
    if [ "$1" = -a ]; then echo "1 Xtigervnc :99 -SecurityTypes VncAuth -PasswordFile=$FIXTURE_DIR/vnc.pass"; fi ;;
  ss) echo 'LISTEN 0 10 127.0.0.1:5900 0.0.0.0:*' ;;
  systemctl)
    command="$1"; shift
    case "$command" in
      show) echo crabbox ;;
      stop) /bin/rm -f "$FIXTURE_DIR/active" ;;
      enable)
        [ -s "$FIXTURE_DIR/vnc.password" ] && [ -s "$FIXTURE_DIR/vnc.pass" ]
        printf '%s\n' "$@" >"$FIXTURE_DIR/enabled" ;;
      restart) touch "$FIXTURE_DIR/active" ;;
      reboot)
        /bin/rm -f "$FIXTURE_DIR/active"
        if grep -qx crabbox-xvfb.service "$FIXTURE_DIR/enabled" 2>/dev/null && grep -qx crabbox-desktop.service "$FIXTURE_DIR/enabled" 2>/dev/null; then touch "$FIXTURE_DIR/active"; fi ;;
      is-active) test -f "$FIXTURE_DIR/active" ;;
      *) exit 2 ;;
    esac ;;
  *) exit 2 ;;
esac
`
	for _, name := range []string{"startxfce4", "Xtigervnc", "xdpyinfo", "chown", "id", "install", "openssl", "tigervncpasswd", "stat", "mv", "runuser", "pgrep", "ss", "systemctl"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(mock), 0700); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("bash", "-c", "set -euo pipefail\n"+setup+"\nsystemctl reboot\n"+checks)
	command.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FIXTURE_DIR="+dir)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("prepared desktop failed readiness after reboot: %v\n%s", err, output)
	}
}
