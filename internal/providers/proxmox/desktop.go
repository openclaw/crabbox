package proxmox

import (
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func (p Provider) ConfiguredFeatures(cfg core.Config) core.FeatureSet {
	features := append(core.FeatureSet{}, p.Spec().Features...)
	if cfg.TargetOS != "" && cfg.TargetOS != core.TargetLinux {
		return features
	}
	if cfg.Proxmox.TemplateID > 0 && cfg.Proxmox.DesktopTemplateID == cfg.Proxmox.TemplateID {
		features = append(features, core.FeatureDesktop)
	}
	if cfg.Proxmox.TemplateID > 0 && cfg.Proxmox.BrowserTemplateID == cfg.Proxmox.TemplateID {
		features = append(features, core.FeatureBrowser)
	}
	return features
}

func (p Provider) ValidateRequestedCapabilities(cfg core.Config) error {
	if !cfg.Desktop && !cfg.Browser {
		return nil
	}
	if cfg.TargetOS != core.TargetLinux {
		return core.Exit(2, "Proxmox prepared desktop/browser templates require target=linux")
	}
	if cfg.Desktop && core.NormalizedDesktopEnv(cfg.DesktopEnv) != "xfce" {
		return core.Exit(2, "Proxmox prepared desktops support XFCE/X11 only; use --desktop-env xfce (Wayland is unsupported)")
	}
	features := p.ConfiguredFeatures(cfg)
	if cfg.Desktop && !features.Has(core.FeatureDesktop) {
		return core.Exit(2, "Proxmox template %d is not enabled for desktop leases; prepare XFCE/TigerVNC and set proxmox.desktopTemplateId in trusted user config or CRABBOX_PROXMOX_DESKTOP_TEMPLATE_ID", cfg.Proxmox.TemplateID)
	}
	if cfg.Browser && !features.Has(core.FeatureBrowser) {
		return core.Exit(2, "Proxmox template %d is not enabled for browser leases; prepare crabbox-browser and set proxmox.browserTemplateId in trusted user config or CRABBOX_PROXMOX_BROWSER_TEMPLATE_ID", cfg.Proxmox.TemplateID)
	}
	return nil
}

// Packages, service units and the browser wrapper belong to the template.
// Acquisition only replaces per-clone credentials and starts prepared services.
func preparedTemplateBootstrap(cfg core.Config) (string, string) {
	if !cfg.Desktop && !cfg.Browser {
		return "", ""
	}
	user := core.ShellQuote(cfg.SSHUser)
	prelude := `template_require() {
  if ! "$@"; then
    echo "Proxmox template missing or invalid $template_component; repair the prepared template before retrying" >&2
    return 1
  fi
}
`
	var setup, checks strings.Builder
	setup.WriteString(prelude)
	checks.WriteString(prelude)
	if cfg.Desktop {
		setup.WriteString(`template_component='XFCE/X11 and TigerVNC tools'
for tool in startxfce4 Xtigervnc tigervncpasswd openssl ss pgrep xdpyinfo; do
  template_require command -v "$tool" >/dev/null
done
template_component='crabbox-xvfb.service and crabbox-desktop.service for the configured SSH user'
`)
		setup.WriteString("for service in crabbox-xvfb.service crabbox-desktop.service; do\n  template_require test \"$(systemctl show -p User --value \"$service\")\" = " + user + "\ndone\n")
		setup.WriteString(`template_component='safe credential directory'
template_require test ! -L /var/lib/crabbox
for file in vnc.password vnc.pass desktop.env; do
  template_require test ! -L "/var/lib/crabbox/$file"
done
systemctl stop crabbox-desktop.service crabbox-xvfb.service
`)
		setup.WriteString("install -d -m 0750 -o root -g \"$(id -gn " + user + ")\" /var/lib/crabbox\n")
		// Replace files atomically; never reuse a password copied from the template.
		setup.WriteString(`credential_dir="$(mktemp -d /var/lib/crabbox/.credentials.XXXXXX)"
trap 'rm -rf -- "$credential_dir"' EXIT
(umask 077; openssl rand -base64 18 >"$credential_dir/vnc.password")
head -c 8 "$credential_dir/vnc.password" | tigervncpasswd -f >"$credential_dir/vnc.pass"
printf 'CRABBOX_DESKTOP_ENV=xfce\nDISPLAY=:99\n' >"$credential_dir/desktop.env"
`)
		setup.WriteString("chown " + user + ":\"$(id -gn " + user + ")\" \"$credential_dir/\"*\n")
		setup.WriteString(`chmod 0600 "$credential_dir/vnc.password" "$credential_dir/vnc.pass"
chmod 0644 "$credential_dir/desktop.env"
for file in vnc.password vnc.pass desktop.env; do
  mv -fT -- "$credential_dir/$file" "/var/lib/crabbox/$file"
done
rm -rf -- "$credential_dir"
trap - EXIT
systemctl enable crabbox-xvfb.service crabbox-desktop.service
systemctl restart crabbox-xvfb.service crabbox-desktop.service
`)
		checks.WriteString(`template_component='XFCE/X11 and TigerVNC services'
template_require systemctl is-active --quiet crabbox-xvfb.service
template_require systemctl is-active --quiet crabbox-desktop.service
template_require pgrep -x Xtigervnc >/dev/null
template_require pgrep -x xfce4-session >/dev/null
template_component='X11 display :99'
template_require env DISPLAY=:99 xdpyinfo >/dev/null
template_component='private per-clone VNC credentials'
for file in /var/lib/crabbox/vnc.password /var/lib/crabbox/vnc.pass; do
  template_require test ! -L "$file"
  template_require test -s "$file"
`)
		checks.WriteString("  template_require test \"$(stat -c '%a:%U' \"$file\")\" = " + core.ShellQuote("600:"+cfg.SSHUser) + "\ndone\n")
		checks.WriteString(`template_require bash -o pipefail -c 'head -c 8 /var/lib/crabbox/vnc.password | tigervncpasswd -f | cmp -s - /var/lib/crabbox/vnc.pass'
template_component='VncAuth on TigerVNC display :99 using the per-clone password file'
vnc_command="$(pgrep -a -x Xtigervnc)"
template_require grep -Eq '(^| )[:](99)( |$)' <<<"$vnc_command"
template_require grep -Eq '(^| )-SecurityTypes VncAuth( |$)' <<<"$vnc_command"
template_require grep -Eq '(^| )-PasswordFile[ =]/var/lib/crabbox/vnc.pass( |$)' <<<"$vnc_command"
template_component='VNC listener bound exclusively to loopback port 5900'
listeners="$(ss -H -ltn 'sport = :5900')"
template_require awk 'BEGIN { found=0; bad=0 } { if ($4 == "127.0.0.1:5900") found=1; else if ($4 != "[::1]:5900") bad=1 } END { exit (!found || bad) }' <<<"$listeners"
`)
	}
	if cfg.Browser {
		browser := `template_component='browser.env and executable crabbox-browser wrapper'
template_require test -s /var/lib/crabbox/browser.env
. /var/lib/crabbox/browser.env
template_require test "${BROWSER:-}" = /usr/local/bin/crabbox-browser
template_require test "${CHROME_BIN:-}" = "$BROWSER"
template_require test -x "$BROWSER"
template_require "$BROWSER" --version >/dev/null
`
		setup.WriteString("template_component='browser wrapper usable by the configured SSH user'\ntemplate_require runuser -u " + user + " -- bash -c " + core.ShellQuote(prelude+browser) + "\n")
		checks.WriteString(browser)
	}
	if cfg.Desktop {
		setup.WriteString(fmt.Sprintf("template_ready=false\nfor attempt in {1..30}; do\n  if runuser -u %s -- bash -c %s >/dev/null 2>&1; then template_ready=true; break; fi\n  sleep 1\ndone\nif ! $template_ready; then\n  runuser -u %s -- bash -c %s\n  exit 1\nfi\n", user, core.ShellQuote(checks.String()), user, core.ShellQuote(checks.String())))
	}
	return setup.String(), checks.String()
}
