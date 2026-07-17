package cli

import (
	"context"
	"encoding/base64"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDesktopLaunchRemoteCommandUsesDetachedPOSIXSession(t *testing.T) {
	got := desktopLaunchRemoteCommand(
		SSHTarget{TargetOS: targetLinux},
		"/work/crabbox/cbx_1/repo",
		map[string]string{"DISPLAY": ":99", "BROWSER": "/usr/bin/chromium", "GDK_BACKEND": "x11", "MOZ_ENABLE_WAYLAND": "0"},
		[]string{"/usr/bin/chromium", "https://example.com"},
		desktopLaunchOptions{WindowedBrowser: true, VerifyProcess: true},
	)
	for _, want := range []string{
		"mkdir -p '/work/crabbox/cbx_1/repo'",
		"cd '/work/crabbox/cbx_1/repo'",
		"DISPLAY=':99'",
		"BROWSER='/usr/bin/chromium'",
		"GDK_BACKEND='x11'",
		"MOZ_ENABLE_WAYLAND='0'",
		"setsid '/usr/bin/chromium' 'https://example.com'",
		"crabbox-desktop-launch.log",
		"launch_before_windows=",
		"desktop_launch_new_window",
		"desktop_launch_observed_window",
		"launch_pid=$!",
		`kill -0 "$launch_pid"`,
		"wmctrl -r :ACTIVE: -b remove,fullscreen",
		"xdotool search --onlyvisible --class google-chrome",
		"windowsize \"$window\" 1500 900",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("desktop launch command missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "swaymsg") {
		t.Fatalf("desktop launch command should not use Sway-specific window commands:\n%s", got)
	}
}

func TestDesktopLaunchRemoteCommandRejectsExitedPOSIXProcess(t *testing.T) {
	remote := desktopLaunchRemoteCommand(
		SSHTarget{TargetOS: targetLinux},
		"",
		nil,
		[]string{"sh", "-c", "exit 23"},
		desktopLaunchOptions{VerifyProcess: true},
	)
	out, err := exec.Command("sh", "-c", remote).CombinedOutput()
	if err == nil {
		t.Fatalf("launch command succeeded after child exited:\n%s", out)
	}
	if !strings.Contains(string(out), "desktop command exited during launch (status=23)") {
		t.Fatalf("launch failure output=%q", out)
	}
}

func TestDesktopLaunchRemoteCommandAcceptsNewVisibleWindowFromWrapper(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "window-created")
	xdotool := `#!/bin/sh
if [ "$1" = getmouselocation ]; then exit 0; fi
if [ "$1" = search ]; then
  [ -f ` + shellQuote(marker) + ` ] && echo 20
  exit 0
fi
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "xdotool"), []byte(xdotool), 0o755); err != nil {
		t.Fatal(err)
	}
	remote := desktopLaunchRemoteCommand(
		SSHTarget{TargetOS: targetLinux},
		"",
		nil,
		[]string{"sh", "-c", "touch " + shellQuote(marker)},
		desktopLaunchOptions{VerifyProcess: true},
	)
	cmd := exec.Command("sh", "-c", remote)
	cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "DISPLAY=:99")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("wrapper launch with new visible window failed: %v\n%s", err, out)
	}
}

func TestDesktopLaunchRemoteCommandRejectsFailedWrapperWithNewWindow(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "error-window-created")
	xdotool := `#!/bin/sh
if [ "$1" = getmouselocation ]; then exit 0; fi
if [ "$1" = search ]; then
  [ -f ` + shellQuote(marker) + ` ] && echo 20
  exit 0
fi
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "xdotool"), []byte(xdotool), 0o755); err != nil {
		t.Fatal(err)
	}
	remote := desktopLaunchRemoteCommand(
		SSHTarget{TargetOS: targetLinux},
		"",
		nil,
		[]string{"sh", "-c", "touch " + shellQuote(marker) + "; exit 23"},
		desktopLaunchOptions{VerifyProcess: true},
	)
	cmd := exec.Command("sh", "-c", remote)
	cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "DISPLAY=:99")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("failed wrapper with a new window was accepted:\n%s", out)
	}
	if !strings.Contains(string(out), "desktop command exited during launch (status=23)") {
		t.Fatalf("wrapper failure output=%q", out)
	}
}

func TestDesktopLaunchRemoteCommandAcceptsExistingWindowHandoff(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "existing-window-focused")
	xdotool := `#!/bin/sh
if [ "$1" = getmouselocation ]; then exit 0; fi
if [ "$1" = getactivewindow ]; then
  if [ -f ` + shellQuote(marker) + ` ]; then echo 20; else echo 10; fi
  exit 0
fi
if [ "$1" = search ]; then
  printf '10\n20\n'
  exit 0
fi
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "xdotool"), []byte(xdotool), 0o755); err != nil {
		t.Fatal(err)
	}
	remote := desktopLaunchRemoteCommand(
		SSHTarget{TargetOS: targetLinux},
		"",
		nil,
		[]string{"sh", "-c", "touch " + shellQuote(marker)},
		desktopLaunchOptions{VerifyProcess: true},
	)
	cmd := exec.Command("sh", "-c", remote)
	cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "DISPLAY=:99")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("existing-window handoff failed: %v\n%s", err, out)
	}
}

func TestDesktopLaunchRemoteCommandChecksLinuxTerminalVisibility(t *testing.T) {
	got := desktopLaunchRemoteCommand(
		SSHTarget{TargetOS: targetLinux},
		"",
		nil,
		[]string{"xterm"},
		desktopLaunchOptions{VerifyProcess: true, VisibleWindowTitle: "Crabbox Desktop cbx-test"},
	)
	for _, want := range []string{
		"launch_before_windows=",
		`&& desktop_launch_new_window`,
		"desktop window not visible",
		`kill "$launch_pid"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("launch visibility check missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `xdotool search --onlyvisible --name 'Crabbox Desktop cbx-test'`) {
		t.Fatalf("terminal verification should not depend on a mutable title:\n%s", got)
	}
}

func TestDesktopLaunchRemoteCommandAcceptsRetitledTerminalWindow(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "terminal-window-created")
	xdotool := `#!/bin/sh
if [ "$1" = getmouselocation ]; then exit 0; fi
if [ "$1" = getactivewindow ]; then echo 10; exit 0; fi
if [ "$1" = search ]; then
  echo 10
  [ -f ` + shellQuote(marker) + ` ] && echo 20
  exit 0
fi
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "xdotool"), []byte(xdotool), 0o755); err != nil {
		t.Fatal(err)
	}
	remote := desktopLaunchRemoteCommand(
		SSHTarget{TargetOS: targetLinux},
		"",
		nil,
		[]string{"sh", "-c", "touch " + shellQuote(marker) + "; sleep 2"},
		desktopLaunchOptions{VerifyProcess: true, VisibleWindowTitle: "Crabbox Desktop cbx-test"},
	)
	cmd := exec.Command("sh", "-c", remote)
	cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "DISPLAY=:99")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("retitled terminal window failed stable-identity verification: %v\n%s", err, out)
	}
}

func TestDesktopLaunchRemoteCommandMakesMacOpenWait(t *testing.T) {
	got := desktopLaunchRemoteCommand(
		SSHTarget{TargetOS: targetMacOS},
		"",
		nil,
		[]string{"open", "-na", "Ghostty.app"},
		desktopLaunchOptions{VerifyProcess: true},
	)
	if !strings.Contains(got, "open' '-W' '-na' 'Ghostty.app") {
		t.Fatalf("macOS open command does not wait for the app:\n%s", got)
	}
}

func TestDesktopBrowserDarkModeCommandPatchesManagedChromiumWrapper(t *testing.T) {
	got := desktopBrowserDarkModeCommand("/usr/local/bin/crabbox-browser")
	for _, want := range []string{
		"/usr/local/bin/crabbox-configure-desktop-theme",
		`[ "$browser_wrapper" = "/usr/local/bin/crabbox-browser" ]`,
		"grep -q -- \"--force-dark-mode\"",
		"grep -q -- \"desktop-theme\"",
		"grep -q -- \"--user-data-dir\"",
		`theme="$(cat "${CRABBOX_DESKTOP_THEME_FILE:-$HOME/.config/crabbox/desktop-theme}"`,
		"umask 077",
		`chmod 700 "$profile"`,
		"--blink-settings=preferredColorScheme=1",
		"--force-dark-mode --enable-features=WebUIDarkMode --blink-settings=preferredColorScheme=2",
		`--user-data-dir=\"\$profile\"`,
		`CRABBOX_DESKTOP_ENV=gnome`,
		`CRABBOX_DESKTOP_ENV=wayland`,
		`export DISPLAY="${DISPLAY:-:0}"`,
		`export GDK_BACKEND=x11 MOZ_ENABLE_WAYLAND=0`,
		"--ozone-platform=x11",
		"--ozone-platform=wayland",
		"sudo install -m 0755",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("dark mode command missing %q:\n%s", want, got)
		}
	}
}

func TestDesktopTypeUsesPasteForSymbolHeavyText(t *testing.T) {
	for _, text := range []string{"peter@example.com", "token+secret", "line one\nline two", "https://example.com"} {
		if !desktopShouldPasteForType(text) {
			t.Fatalf("expected paste fallback for %q", text)
		}
	}
	if desktopShouldPasteForType("helloWorld123") {
		t.Fatal("plain alphanumeric text should use xdotool type")
	}
}

func TestDesktopTextInputSupportsLinuxAndMacOS(t *testing.T) {
	for _, target := range []SSHTarget{{TargetOS: targetLinux}, {TargetOS: targetMacOS}} {
		if !desktopTextSupportsTarget(target) {
			t.Errorf("target %q should support desktop text input", target.TargetOS)
		}
	}
	for _, target := range []SSHTarget{{TargetOS: targetWindows}, {TargetOS: "freebsd"}} {
		if desktopTextSupportsTarget(target) {
			t.Errorf("target %q should not support desktop text input", target.TargetOS)
		}
	}
}

func TestDesktopPasteFailureSafeToRetry(t *testing.T) {
	for _, detail := range []string{
		"missing clipboard tool; warm a new lease",
		"clipboard helper exited before paste (status=42)",
		"clipboard helper failed to provide requested contents (xsel)",
	} {
		if !desktopPasteFailureSafeToRetry(detail) {
			t.Errorf("expected pre-input failure to be retryable: %q", detail)
		}
	}
	for _, detail := range []string{
		"clipboard helper failed while serving paste (status=42)",
		"xdotool type failed after entering part of the text",
		"wtype returned status 1",
	} {
		if desktopPasteFailureSafeToRetry(detail) {
			t.Errorf("expected potentially partial input failure not to be retryable: %q", detail)
		}
	}
}

func TestDesktopPasteRemoteCommandPrefersClipboardTools(t *testing.T) {
	got := desktopPasteRemoteCommand()
	for _, want := range []string{
		`CRABBOX_DESKTOP_ENV:-xfce`,
		"wtype -d 1 -",
		"timeout 5s xclip -quiet -selection clipboard -loops 1",
		`timeout 1s xclip -selection clipboard -o | cmp -s - "$tmp"`,
		"xsel --nodetach --selectionTimeout 5000 --clipboard --input",
		`timeout 1s xsel --clipboard --output | cmp -s - "$tmp"`,
		"clipboard helper failed to provide requested contents (xsel)",
		"wl-copy --foreground --paste-once",
		"getactivewindow getwindowclassname",
		"getactivewindow getwindowpid",
		`*xterm*|*terminal*|*konsole*|*alacritty*|*kitty*|*wezterm*)`,
		`xdotool type --clearmodifiers --delay 1 --file "$tmp"`,
		"xdotool key --clearmodifiers ctrl+v",
		`kill -0 "$clip_pid"`,
		"clipboard helper exited before paste",
		"clipboard helper failed while serving paste",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("paste command missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `wait "$clip_pid" || true`) {
		t.Fatalf("paste command still swallows clipboard helper failure:\n%s", got)
	}
}

func TestDesktopPasteRemoteCommandPropagatesClipboardHelperFailure(t *testing.T) {
	dir := t.TempDir()
	for name, script := range map[string]string{
		"timeout": "#!/bin/sh\nshift\nexec \"$@\"\n",
		"xdotool": "#!/bin/sh\n[ \"$1\" = key ] && exit 0\nexit 1\n",
		"xclip":   "#!/bin/sh\nexit 42\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", desktopPasteRemoteCommand())
	cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "CRABBOX_DESKTOP_ENV=xfce", "DISPLAY=:99")
	cmd.Stdin = strings.NewReader("new clipboard value")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("paste command succeeded after clipboard helper failure:\n%s", out)
	}
	if !strings.Contains(string(out), "clipboard helper") || !strings.Contains(string(out), "status=42") {
		t.Fatalf("paste failure output=%q", out)
	}
}

func TestDesktopPasteRemoteCommandAcceptsXclipManagerHandoff(t *testing.T) {
	dir := t.TempDir()
	clipboard := filepath.Join(dir, "clipboard")
	pasted := filepath.Join(dir, "pasted")
	for _, name := range []string{"cat", "cmp", "mktemp", "rm", "sleep", "tr"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	for name, script := range map[string]string{
		"timeout": "#!/bin/sh\nshift\nexec \"$@\"\n",
		"xdotool": "#!/bin/sh\nif [ \"$1\" = key ]; then : > " + shellQuote(pasted) + "; exit 0; fi\nexit 1\n",
		"xclip":   "#!/bin/sh\ncase \" $* \" in\n  *\" -o \"*) cat " + shellQuote(clipboard) + ";;\n  *)\n    for arg do input=$arg; done\n    cat \"$input\" > " + shellQuote(clipboard) + "\n    ;;\nesac\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", desktopPasteRemoteCommand())
	cmd.Env = append(os.Environ(), "PATH="+dir, "CRABBOX_DESKTOP_ENV=xfce", "DISPLAY=:99")
	cmd.Stdin = strings.NewReader("xclip manager clipboard value")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("xclip manager handoff failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(pasted); err != nil {
		t.Fatalf("xclip manager handoff did not send Ctrl+V: %v", err)
	}
}

func TestDesktopPasteRemoteCommandVerifiesXselByReadback(t *testing.T) {
	dir := t.TempDir()
	clipboard := filepath.Join(dir, "clipboard")
	pasted := filepath.Join(dir, "pasted")
	stopped := filepath.Join(dir, "stopped")
	for _, name := range []string{"cat", "cmp", "mktemp", "rm", "sleep", "tr"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	for name, script := range map[string]string{
		"timeout": "#!/bin/sh\nshift\nexec \"$@\"\n",
		"xdotool": "#!/bin/sh\nif [ \"$1\" = key ]; then : > " + shellQuote(pasted) + "; exit 0; fi\nexit 1\n",
		"xsel":    "#!/bin/sh\ncase \"$*\" in\n  *--input*)\n    cat > " + shellQuote(clipboard) + "\n    trap ': > " + shellQuote(stopped) + "; exit 0' TERM INT\n    while :; do sleep 0.05; done\n    ;;\n  *--output*) cat " + shellQuote(clipboard) + ";;\nesac\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", desktopPasteRemoteCommand())
	cmd.Env = append(os.Environ(), "PATH="+dir, "CRABBOX_DESKTOP_ENV=xfce", "DISPLAY=:99")
	cmd.Stdin = strings.NewReader("xsel clipboard value")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("xsel paste command failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(pasted); err != nil {
		t.Fatalf("xsel paste did not send Ctrl+V: %v", err)
	}
	if _, err := os.Stat(stopped); err != nil {
		t.Fatalf("xsel clipboard owner was not stopped: %v", err)
	}
}

func TestDesktopLinuxTerminalSupportsWaylandAndX11(t *testing.T) {
	got, err := desktopTerminalCommand(
		SSHTarget{TargetOS: targetLinux},
		[]string{"bash", "-lc", "echo hello"},
		desktopTerminalOptions{FontSize: 16, Cols: 120, Rows: 40},
	)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{
		"CRABBOX_DESKTOP_ENV:-xfce",
		"export DISPLAY=\"${DISPLAY:-:0}\"",
		"export GDK_BACKEND=x11 MOZ_ENABLE_WAYLAND=0",
		"exec gnome-terminal --wait --title='Crabbox Desktop' --working-directory=\"$PWD\" -- bash -lc",
		"exec foot --title='Crabbox Desktop'",
		"monospace:size=16",
		"export DISPLAY=\"${DISPLAY:-:99}\"",
		"exec xterm -title 'Crabbox Desktop' -fa monospace -fs '16' -geometry '120x40'",
		"bash -lc ''\\''bash'\\'' '\\''-lc'\\'' '\\''echo hello'\\'''",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("linux terminal command missing %q: %v", want, got)
		}
	}
}

func TestDesktopClickRemoteCommandSupportsManagedTargets(t *testing.T) {
	linux := desktopClickRemoteCommand(SSHTarget{TargetOS: targetLinux}, 12, 34)
	for _, want := range []string{"CRABBOX_DESKTOP_ENV:-xfce", "not supported on Wayland desktop envs", "DISPLAY=\"${DISPLAY:-:99}\"", "xdotool getactivewindow", "xdotool mousemove 12 34 click 1"} {
		if !strings.Contains(linux, want) {
			t.Fatalf("linux click command missing %q:\n%s", want, linux)
		}
	}
	if strings.Index(linux, "xdotool getactivewindow") > strings.Index(linux, "not supported on Wayland desktop envs") {
		t.Fatalf("linux click must try an active XWayland window before rejecting the desktop env:\n%s", linux)
	}
	mac := desktopClickRemoteCommand(SSHTarget{TargetOS: targetMacOS}, 12, 34)
	for _, want := range []string{"cliclick c:12,34", "import CoreGraphics", "CGEvent"} {
		if !strings.Contains(mac, want) {
			t.Fatalf("mac click command missing %q:\n%s", want, mac)
		}
	}
	windows := desktopClickRemoteCommand(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, 12, 34)
	for _, want := range []string{"MouseInput", "SetCursorPos(12, 34)", "schtasks.exe", "/IT"} {
		if !strings.Contains(windows, want) {
			t.Fatalf("windows click command missing %q:\n%s", want, windows)
		}
	}
}

func TestDesktopWaylandInputBranches(t *testing.T) {
	key := desktopKeyRemoteCommand("ctrl+l")
	for _, want := range []string{
		"CRABBOX_DESKTOP_ENV:-xfce",
		"command -v wtype",
		"exec 'wtype' '-M' 'ctrl' '-k' 'l' '-m' 'ctrl'",
		"xdotool key --clearmodifiers 'ctrl+l'",
	} {
		if !strings.Contains(key, want) {
			t.Fatalf("key command missing %q:\n%s", want, key)
		}
	}
	if strings.Index(key, "xdotool getactivewindow") > strings.Index(key, "command -v wtype") {
		t.Fatalf("key command must prefer an active XWayland window before native Wayland fallback:\n%s", key)
	}
	unsupported := desktopKeyRemoteCommand("ctrl+l alt+Tab")
	if !strings.Contains(unsupported, "supports a single key or modifier+key sequence") {
		t.Fatalf("complex wayland key sequence should be rejected:\n%s", unsupported)
	}
	typed := desktopTypeRemoteCommand("hello")
	for _, want := range []string{"wtype -d 1 -- 'hello'", "xdotool type --clearmodifiers --delay 1 -- 'hello'"} {
		if !strings.Contains(typed, want) {
			t.Fatalf("type command missing %q:\n%s", want, typed)
		}
	}
	if strings.Index(typed, "xdotool getactivewindow") > strings.Index(typed, "wtype -d 1") {
		t.Fatalf("type command must prefer an active XWayland window before native Wayland fallback:\n%s", typed)
	}
}

func TestDesktopClickRejectsWSL2Target(t *testing.T) {
	if desktopClickSupportsTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}) {
		t.Fatal("WSL2 desktop click should be rejected before xdotool dispatch")
	}
	if !desktopClickSupportsTarget(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}) {
		t.Fatal("native Windows desktop click should be supported")
	}
}

func TestDesktopAndArtifactCommandsApplyProviderFlags(t *testing.T) {
	ctx := context.Background()
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	base := []string{
		"--provider", "azure",
		"--azure-backend", "invalid",
		"--id", "cbx_abcdef123456",
	}
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "click", run: func() error {
			return app.desktopClick(ctx, append(append([]string(nil), base...), "--x", "1", "--y", "1"))
		}},
		{name: "launch", run: func() error {
			return app.desktopLaunch(ctx, append([]string(nil), base...))
		}},
		{name: "terminal", run: func() error {
			return app.desktopTerminal(ctx, append([]string(nil), base...))
		}},
		{name: "proof", run: func() error {
			return app.desktopProof(ctx, append([]string(nil), base...))
		}},
		{name: "artifacts collect", run: func() error {
			return app.artifactsCollect(ctx, append([]string(nil), base...))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run()
			if err == nil || !strings.Contains(err.Error(), "azure backend must be vm or dynamic-sessions") {
				t.Fatalf("error=%v, want provider flag application error", err)
			}
			if strings.Contains(err.Error(), "flag provided but not defined") {
				t.Fatalf("provider flag was not registered: %v", err)
			}
		})
	}

	keys, err := desktopKeySequenceArg([]string{
		"--provider", "azure",
		"--azure-backend", "vm",
		"--id", "cbx_abcdef123456",
		"--keys", "ctrl+l",
	})
	if err != nil || keys != "ctrl+l" {
		t.Fatalf("desktop key provider flags: keys=%q err=%v", keys, err)
	}
}

func TestDesktopKeySequenceArgSkipsLeaseID(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "positional id",
			args: []string{"blue-lobster", "ctrl+l"},
			want: "ctrl+l",
		},
		{
			name: "single dash id",
			args: []string{"-id", "blue-lobster", "ctrl+l"},
			want: "ctrl+l",
		},
		{
			name: "double dash id",
			args: []string{"--id", "blue-lobster", "ctrl+l"},
			want: "ctrl+l",
		},
		{
			name: "equals id",
			args: []string{"--id=blue-lobster", "ctrl+l"},
			want: "ctrl+l",
		},
		{
			name: "explicit keys",
			args: []string{"--id", "blue-lobster", "--keys", "ctrl+l"},
			want: "ctrl+l",
		},
		{
			name: "single dash explicit keys",
			args: []string{"-id", "blue-lobster", "-keys", "ctrl+l"},
			want: "ctrl+l",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := desktopKeySequenceArg(tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("keys=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestStringFlagValueAcceptsGoFlagForms(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "double dash space", args: []string{"--output", "screen.mp4"}, want: "screen.mp4"},
		{name: "double dash equals", args: []string{"--output=screen.mp4"}, want: "screen.mp4"},
		{name: "single dash space", args: []string{"-output", "screen.mp4"}, want: "screen.mp4"},
		{name: "single dash equals", args: []string{"-output=screen.mp4"}, want: "screen.mp4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := stringFlagValue(tt.args, "output")
			if !ok {
				t.Fatal("missing flag")
			}
			if got != tt.want {
				t.Fatalf("value=%q, want %q", got, tt.want)
			}
		})
	}
}

func TestBoolFlagValueOrAcceptsGoFlagForms(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "default", args: nil, want: true},
		{name: "bare true", args: []string{"--contact-sheet"}, want: true},
		{name: "equals false", args: []string{"--contact-sheet=false"}, want: false},
		{name: "equals zero", args: []string{"-contact-sheet=0"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := boolFlagValueOr(tt.args, "contact-sheet", true); got != tt.want {
				t.Fatalf("boolFlagValueOr=%t, want %t", got, tt.want)
			}
		})
	}
}

func TestDesktopLaunchWebVNCArgsCarriesTargetDetails(t *testing.T) {
	got := desktopLaunchWebVNCArgs(
		Config{Provider: "aws", TargetOS: targetWindows, WindowsMode: windowsModeWSL2, Network: NetworkTailscale},
		SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2},
		"cbx_1",
		true,
		true,
	)
	joined := strings.Join(got, " ")
	for _, want := range []string{
		"--provider aws",
		"--target windows",
		"--network tailscale",
		"--windows-mode wsl2",
		"--id cbx_1",
		"--open",
		"--take-control",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("webvnc args missing %q: %v", want, got)
		}
	}
}

func TestDesktopLaunchWebVNCArgsCarriesExternalPrivateRouting(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const leaseID = "cbx_abcdef123456"
	stored := ExternalConfig{Command: "provider-command", WorkRoot: "/work/crabbox"}
	stored.Connection.Desktop = ExternalDesktopConfig{Username: "screen-user", PasswordEnv: "SCREEN_SHARING_PASSWORD"}
	routingPath, err := PersistExternalRouting(leaseID, stored)
	if err != nil {
		t.Fatal(err)
	}
	routing, err := LoadExternalRouting(routingPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Provider: "external", TargetOS: targetMacOS}
	got := desktopLaunchWebVNCArgs(cfg, SSHTarget{TargetOS: targetMacOS}, leaseID, true, false)
	want := []string{
		"--provider", "external",
		"--target", targetMacOS,
		"--id", leaseID,
		"--external-routing-file", routingPath,
		"--external-routing-digest", ExternalRoutingDigest(routing),
		"--external-desktop-username", "screen-user",
		"--external-desktop-password-env", "SCREEN_SHARING_PASSWORD",
		"--open",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("webvnc args=%#v, want %#v", got, want)
	}
}

func TestDesktopLaunchWebVNCArgsCarriesProviderRoutingHook(t *testing.T) {
	got := desktopLaunchWebVNCArgs(
		Config{Provider: "direct-webvnc-test", TargetOS: targetLinux},
		SSHTarget{TargetOS: targetLinux},
		"cbx_abcdef123456",
		false,
		false,
	)
	want := []string{
		"--provider", "direct-webvnc-test",
		"--target", targetLinux,
		"--id", "cbx_abcdef123456",
		"--direct-webvnc-routing", "route-cbx_abcdef123456",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("webvnc args=%#v, want %#v", got, want)
	}
}

func TestDesktopLaunchRemoteCommandCanPassEgressProxyToBrowser(t *testing.T) {
	got := desktopLaunchRemoteCommand(
		SSHTarget{TargetOS: targetLinux},
		"/work/crabbox/cbx_1/repo",
		map[string]string{"DISPLAY": ":99", "BROWSER": "/usr/bin/chromium"},
		[]string{"/usr/bin/chromium", "--proxy-server=http://127.0.0.1:3128", "https://discord.com/login"},
		desktopLaunchOptions{WindowedBrowser: true},
	)
	if !strings.Contains(got, "'/usr/bin/chromium' '--proxy-server=http://127.0.0.1:3128' 'https://discord.com/login'") {
		t.Fatalf("desktop launch command missing egress proxy arg:\n%s", got)
	}
}

func TestDesktopCommandLooksLikeBrowser(t *testing.T) {
	if !desktopCommandLooksLikeBrowser([]string{"/usr/bin/google-chrome"}, "") {
		t.Fatal("google-chrome should be treated as browser")
	}
	if !desktopCommandLooksLikeBrowser([]string{"/opt/crabbox-browser"}, "/opt/crabbox-browser") {
		t.Fatal("BROWSER env wrapper should be treated as browser")
	}
	if desktopCommandLooksLikeBrowser([]string{"xterm"}, "/opt/crabbox-browser") {
		t.Fatal("xterm should not be treated as browser")
	}
}

func TestDesktopBrowserLaunchCheckAvoidsSelfMatchingShell(t *testing.T) {
	got := desktopBrowserLaunchCheckCommand()
	if strings.Contains(got, "pgrep -f") {
		t.Fatalf("launch check must not match its own shell text:\n%s", got)
	}
	for _, want := range []string{
		"pgrep -x google-chrome",
		"pgrep -x chrome",
		"pgrep -x chromium",
		"pgrep -x chromium-browser",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("launch check missing process-name probe %q:\n%s", want, got)
		}
	}
}

func TestWindowsDesktopLaunchRemoteCommandUsesActiveInteractiveSession(t *testing.T) {
	got := windowsDesktopLaunchPowerShell(
		`C:\crabbox\cbx_1\repo`,
		map[string]string{"BROWSER": `C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`},
		[]string{`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`, "https://example.com"},
	)
	for _, want := range []string{
		"CrabboxDesktopLauncher",
		"desktop-launch-requests",
		"CrabboxDesktopWindows",
		"RelatedTo",
		"AddSeconds(45)",
		"SetForegroundWindow",
		"GetForegroundWindow",
		windowsDesktopWindowMarker,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("windows desktop launch command missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"schtasks.exe", "windows.username", "windows.password", `"/IT"`} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("windows desktop launch command still contains %q:\n%s", forbidden, got)
		}
	}
}

func TestParseWindowsDesktopWindow(t *testing.T) {
	title := "Crabbox test — Edge"
	encoded := base64.StdEncoding.EncodeToString([]byte(title))
	got, err := parseWindowsDesktopWindow("noise\n" + windowsDesktopWindowMarker + " pid=421 session=3 title=" + encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != 421 || got.SessionID != 3 || got.Title != title {
		t.Fatalf("window=%#v", got)
	}
	for _, invalid := range []string{
		"",
		windowsDesktopWindowMarker + " pid=0 session=3 title=" + encoded,
		windowsDesktopWindowMarker + " pid=421 session=-1 title=" + encoded,
		windowsDesktopWindowMarker + " pid=421 session=3 title=not-base64",
	} {
		if _, err := parseWindowsDesktopWindow(invalid); err == nil {
			t.Fatalf("invalid marker accepted: %q", invalid)
		}
	}
}

func TestWindowsDesktopLaunchScriptStartsAndForegroundsProcess(t *testing.T) {
	got := windowsDesktopLaunchScript(
		`C:\crabbox\cbx_1\repo`,
		map[string]string{"BROWSER": `C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`},
		[]string{`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`, "https://example.com"},
	)
	for _, want := range []string{
		`New-Item -ItemType Directory -Force -Path 'C:\crabbox\cbx_1\repo'`,
		`Set-Location -LiteralPath 'C:\crabbox\cbx_1\repo'`,
		`$env:BROWSER = 'C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe'`,
		"ProcessStartInfo",
		"function Q",
		"$psi.Arguments=",
		"[System.Diagnostics.Process]::Start($psi)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("windows desktop launch script missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ArgumentList") {
		t.Fatalf("windows desktop launch script must not use PowerShell 7-only ArgumentList:\n%s", got)
	}
}

func TestWindowsDesktopTerminalUsesMinttyWithSixelDefaults(t *testing.T) {
	got, err := desktopTerminalCommand(
		SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal},
		[]string{"/c/gifgrep-smoke/run.sh"},
		desktopTerminalOptions{FontSize: 24, Cols: 84, Rows: 26, Sixel: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{
		`C:\Program Files\Git\usr\bin\mintty.exe`,
		"FontHeight=24",
		"Columns=84",
		"Rows=26",
		"Scrollbar=none",
		"TERM=xterm-256color",
		"GIFGREP_INLINE",
		"'/c/gifgrep-smoke/run.sh'",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("terminal command missing %q: %v", want, got)
		}
	}
	for _, bad := range []string{"cmd.exe", "start"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("terminal command should launch mintty directly, found %q: %v", bad, got)
		}
	}
}

func TestMacOSDesktopTerminalUsesGhostty(t *testing.T) {
	got, err := desktopTerminalCommand(
		SSHTarget{TargetOS: targetMacOS},
		[]string{"/tmp/run-demo.sh"},
		desktopTerminalOptions{FontSize: 18, Cols: 118, Rows: 34},
	)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{
		"open -W -na Ghostty.app --args",
		"--title=Crabbox Desktop",
		"--font-size=18",
		"--window-width=118",
		"--window-height=34",
		"--window-save-state=never",
		"GIFGREP_INLINE",
		"GIFGREP_SOFTWARE_ANIM",
		"'/tmp/run-demo.sh'",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("terminal command missing %q: %v", want, got)
		}
	}
}

func TestDesktopTerminalPositionalIDSkipsStaticProviders(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		id       string
		argCount int
		want     bool
	}{
		{name: "managed positional id", provider: "aws", argCount: 1, want: true},
		{name: "explicit id", provider: "aws", id: "cbx_1", argCount: 1, want: false},
		{name: "no args", provider: "aws", argCount: 0, want: false},
		{name: "ssh command", provider: "ssh", argCount: 1, want: false},
		{name: "static alias command", provider: "static", argCount: 1, want: false},
		{name: "static ssh alias command", provider: "static-ssh", argCount: 1, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldConsumeDesktopTerminalPositionalID(tt.provider, tt.id, tt.argCount); got != tt.want {
				t.Fatalf("shouldConsumeDesktopTerminalPositionalID=%t want %t", got, tt.want)
			}
		})
	}
}

func TestTrimCommandSeparatorAfterPositionalProofID(t *testing.T) {
	got := trimCommandSeparator([]string{"--", "./scripts/smoke.sh", "--flag"})
	want := []string{"./scripts/smoke.sh", "--flag"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trimCommandSeparator=%v want %v", got, want)
	}
	if got := trimCommandSeparator([]string{"./scripts/smoke.sh"}); !reflect.DeepEqual(got, []string{"./scripts/smoke.sh"}) {
		t.Fatalf("trimCommandSeparator without separator=%v", got)
	}
}

func TestDesktopPublishOptionsRequireArtifactDirectory(t *testing.T) {
	pr := 123
	dir := ""
	storage := "local"
	baseURL := "https://artifacts.example.com"
	flags := desktopPublishFlagValues{
		PR:       &pr,
		Dir:      &dir,
		Storage:  &storage,
		BaseURL:  &baseURL,
		Explicit: map[string]bool{"storage": true},
	}
	if _, ok, err := publishOptionsFromDesktopFlags(".", flags); err == nil || ok {
		t.Fatalf("expected missing publish directory error, ok=%t err=%v", ok, err)
	}

	dir = "artifacts/proof"
	opts, ok, err := publishOptionsFromDesktopFlags("", flags)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected publish options")
	}
	if opts.Directory != dir || opts.PR != pr || opts.Storage != storage || opts.BaseURL != baseURL {
		t.Fatalf("opts=%#v", opts)
	}
}

func TestDesktopPublishOptionsHonorArtifactStorageEnv(t *testing.T) {
	t.Setenv("CRABBOX_ARTIFACTS_STORAGE", "s3")
	t.Setenv("CRABBOX_ARTIFACTS_BUCKET", "proof-bucket")
	pr := 123
	dir := "artifacts/proof"
	storage := "auto"
	flags := desktopPublishFlagValues{
		PR:      &pr,
		Dir:     &dir,
		Storage: &storage,
	}
	opts, ok, err := publishOptionsFromDesktopFlags("", flags)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected publish options")
	}
	if opts.Storage != "s3" {
		t.Fatalf("storage=%q want env default s3", opts.Storage)
	}
	flags.Explicit = map[string]bool{"storage": true}
	opts, ok, err = publishOptionsFromDesktopFlags("", flags)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected publish options")
	}
	if opts.Storage != "auto" {
		t.Fatalf("explicit storage=%q want auto", opts.Storage)
	}
}

func TestDesktopVideoTargetGate(t *testing.T) {
	tests := []struct {
		name   string
		target SSHTarget
		want   bool
	}{
		{name: "linux", target: SSHTarget{TargetOS: targetLinux}, want: true},
		{name: "native windows", target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}, want: true},
		{name: "wsl2 windows", target: SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2}, want: false},
		{name: "macos", target: SSHTarget{TargetOS: targetMacOS}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := supportsDesktopVideoTarget(tt.target); got != tt.want {
				t.Fatalf("supportsDesktopVideoTarget=%t want %t", got, tt.want)
			}
		})
	}
}

func TestRejectWaylandDesktopVideoEnv(t *testing.T) {
	err := rejectWaylandDesktopVideoEnv(map[string]string{"CRABBOX_DESKTOP_ENV": desktopEnvWayland}, "desktop proof")
	if err == nil || !strings.Contains(err.Error(), "does not support Wayland desktop envs") {
		t.Fatalf("err=%v, want wayland video rejection", err)
	}
	if err := rejectWaylandDesktopVideoEnv(map[string]string{"CRABBOX_DESKTOP_ENV": desktopEnvXFCE}, "desktop proof"); err != nil {
		t.Fatalf("xfce video rejected: %v", err)
	}
}

func TestDesktopVideoRemoteCommandRejectsWayland(t *testing.T) {
	got := desktopVideoRemoteCommand(5*time.Second, 8)
	for _, want := range []string{
		"/var/lib/crabbox/desktop.env",
		`CRABBOX_DESKTOP_ENV:-xfce`,
		"does not support Wayland desktop envs",
		"x11grab",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("video command missing %q:\n%s", want, got)
		}
	}
}

func TestDesktopRecorderDiagnosticsCommandsCoverWindowsAndLinux(t *testing.T) {
	win := desktopRecorderDiagnosticsRemoteCommand(SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal})
	for _, want := range []string{"powershell.exe", "-EncodedCommand"} {
		if !strings.Contains(win, want) {
			t.Fatalf("windows diagnostics missing %q:\n%s", want, win)
		}
	}
	linux := desktopRecorderDiagnosticsRemoteCommand(SSHTarget{TargetOS: targetLinux})
	for _, want := range []string{"remote-ffmpeg", "xdpyinfo", "vnc-listener"} {
		if !strings.Contains(linux, want) {
			t.Fatalf("linux diagnostics missing %q:\n%s", want, linux)
		}
	}
}

func TestContactSheetFlagsDefaultEnabled(t *testing.T) {
	enabled := true
	skip := false
	if !contactSheetEnabled(contactSheetFlagValues{Enabled: &enabled, Skip: &skip}) {
		t.Fatal("contact sheet should default enabled")
	}
	skip = true
	if contactSheetEnabled(contactSheetFlagValues{Enabled: &enabled, Skip: &skip}) {
		t.Fatal("skip flag should disable contact sheet")
	}
}
