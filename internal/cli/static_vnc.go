package cli

import (
	"context"
	"fmt"
	"strings"
)

const staticManagedVNCPasswordName = "macos-vnc.password"

type staticManagedVNCLogin struct {
	User         string
	Password     string
	PasswordPath string
}

func ensureStaticManagedVNCLogin(ctx context.Context, cfg Config, target SSHTarget) (staticManagedVNCLogin, error) {
	if !isStaticProvider(cfg.Provider) || !cfg.Static.ManagedLogin {
		return staticManagedVNCLogin{}, nil
	}
	user := strings.TrimSpace(firstNonEmpty(cfg.Static.ManagedUser, "crabbox"))
	if user == "" {
		return staticManagedVNCLogin{}, exit(2, "static managed VNC login requires --managed-user")
	}
	switch target.TargetOS {
	case targetMacOS:
		return ensureStaticMacOSManagedVNCLogin(ctx, cfg, target, user)
	case targetWindows:
		return staticManagedVNCLogin{}, exit(2, "static managed VNC login for target=windows requires SSH/WinRM plus a supported VNC server setup path; this target only exposes an existing host-managed VNC service")
	default:
		return staticManagedVNCLogin{}, exit(2, "static managed VNC login is supported for target=macos only in this release")
	}
}

func ensureStaticMacOSManagedVNCLogin(ctx context.Context, cfg Config, target SSHTarget, user string) (staticManagedVNCLogin, error) {
	root := strings.TrimSpace(cfg.Static.WorkRoot)
	out, err := runSSHOutput(ctx, target, staticMacOSManagedVNCLoginScript(user, root))
	if err != nil {
		return staticManagedVNCLogin{}, exit(5, "configure static macOS managed VNC login over SSH: %v", err)
	}
	values := parseEnvLines(out)
	if values["USER"] == "" || values["PASSWORD"] == "" || values["PASSWORD_FILE"] == "" {
		return staticManagedVNCLogin{}, exit(5, "static macOS managed VNC login did not return credentials")
	}
	return staticManagedVNCLogin{
		User:         values["USER"],
		Password:     values["PASSWORD"],
		PasswordPath: values["PASSWORD_FILE"],
	}, nil
}

func staticMacOSManagedVNCLoginScript(user, root string) string {
	rootArg := ``
	if strings.TrimSpace(root) != "" {
		rootArg = shellQuote(root)
	}
	return fmt.Sprintf(`set -eu
user=%s
root=%s
if [ -z "$root" ]; then
  root="$HOME/crabbox"
fi
secret_dir="$root/.crabbox"
secret_file="$secret_dir/%s"
sudo -n mkdir -p "$secret_dir"
sudo -n chmod 700 "$secret_dir"
if [ ! -s "$secret_file" ]; then
  pw="$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 18)"
  printf '%%s\n' "$pw" | sudo -n tee "$secret_file" >/dev/null
  sudo -n chmod 600 "$secret_file"
fi
pw="$(sudo -n cat "$secret_file")"
if ! id "$user" >/dev/null 2>&1; then
  sudo -n sysadminctl -addUser "$user" -fullName "Crabbox" -password "$pw" -home "/Users/$user" -shell /bin/zsh >/dev/null
  sudo -n createhomedir -c -u "$user" >/dev/null 2>&1 || true
else
  sudo -n dscl . -passwd "/Users/$user" "$pw"
  sudo -n pwpolicy -u "$user" -clearaccountpolicies >/dev/null 2>&1 || true
fi
home="$(dscl . -read "/Users/$user" NFSHomeDirectory | sed 's/NFSHomeDirectory: //')"
uid="$(id -u "$user")"
sudo -n mkdir -p "$home/Library/Preferences"
sudo -n chown -R "$user":staff "$home/Library" >/dev/null 2>&1 || true
sudo -n /System/Library/CoreServices/RemoteManagement/ARDAgent.app/Contents/Resources/kickstart -activate -configure -allowAccessFor -specifiedUsers >/dev/null
sudo -n /System/Library/CoreServices/RemoteManagement/ARDAgent.app/Contents/Resources/kickstart -configure -access -on -privs -all -users "$user" >/dev/null
sudo -n /System/Library/CoreServices/RemoteManagement/ARDAgent.app/Contents/Resources/kickstart -configure -clientopts -setreqperm -reqperm no >/dev/null
sudo -n launchctl asuser "$uid" sudo -u "$user" defaults write com.apple.SetupAssistant DidSeeAccessibility -bool true >/dev/null 2>&1 || true
sudo -n launchctl asuser "$uid" sudo -u "$user" defaults write com.apple.SetupAssistant DidSeeSiriSetup -bool true >/dev/null 2>&1 || true
sudo -n launchctl asuser "$uid" sudo -u "$user" defaults write com.apple.SetupAssistant DidSeePrivacy -bool true >/dev/null 2>&1 || true
sudo -n launchctl asuser "$uid" sudo -u "$user" defaults write com.apple.SetupAssistant DidSeeCloudSetup -bool true >/dev/null 2>&1 || true
sudo -n launchctl asuser "$uid" sudo -u "$user" defaults write com.apple.SetupAssistant LastSeenCloudProductVersion "$(sw_vers -productVersion)" >/dev/null 2>&1 || true
sudo -n pkill -u "$user" -x "Setup Assistant" >/dev/null 2>&1 || true
sudo -n pkill -u "$user" -x SetupAssistant >/dev/null 2>&1 || true
sudo -n /System/Library/CoreServices/RemoteManagement/ARDAgent.app/Contents/Resources/kickstart -restart -agent >/dev/null
printf 'USER=%%s\n' "$user"
printf 'PASSWORD_FILE=%%s\n' "$secret_file"
printf 'PASSWORD=%%s\n' "$pw"
`, shellQuote(user), rootArg, staticManagedVNCPasswordName)
}
