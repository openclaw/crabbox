package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRunSSHOutputBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell transport fixture")
	}
	for _, tc := range []struct {
		name, body, want string
		limit            int
		wantErr          bool
		limitErr         bool
		timeout          bool
	}{
		{name: "trim", body: "printf '  arm64  \\n'", want: "arm64", limit: 32},
		{name: "empty", body: "exit 0", limit: 32},
		{name: "limit", body: "printf '12345'", limit: 4, wantErr: true, limitErr: true},
		{name: "stderr bounded", body: "printf 'private remote error' >&2; exit 1", limit: 4, wantErr: true},
		{name: "transport takes precedence over overflow", body: "printf '123456'; exit 255", limit: 4, wantErr: true},
		{name: "deadline", body: "exec /bin/sleep 10", limit: 32, wantErr: true, timeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "args")
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(log) + "\n" + tc.body + "\n"
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			target := SSHTarget{User: "builder", Host: "build.example.test", Port: "2207", FallbackPorts: []string{}, TargetOS: targetLinux, Key: filepath.Join(dir, "key"), CertificateFile: filepath.Join(dir, "certificate"), KnownHostsFile: filepath.Join(dir, "known_hosts"), HostKeyAlias: "fixture-alias", ProxyCommand: "fixture-proxy %h %p", SSHConfigProxy: true}
			ctx := context.Background()
			if tc.timeout {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			got, err := RunSSHOutputBounded(ctx, target, "/bin/sh -c 'uname -m'", tc.limit)
			if (err != nil) != tc.wantErr || errors.Is(err, ErrSSHOutputLimit) != tc.limitErr || got != tc.want {
				t.Fatalf("output=%q err=%v", got, err)
			}
			if tc.timeout && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline err=%v", err)
			}
			if err != nil && strings.Contains(err.Error(), "private remote error") {
				t.Fatal("stderr exposed")
			}
			if tc.timeout {
				return
			} // A loaded host may reach its deadline before exec.
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			args := string(data)
			for _, want := range []string{"\n2207\n", "builder@build.example.test", target.Key, "CertificateFile=" + target.CertificateFile, "UserKnownHostsFile=" + target.KnownHostsFile, "HostKeyAlias=fixture-alias", "ForwardAgent=no", "ForwardX11=no", "ProxyCommand=fixture-proxy %h %p"} {
				if !strings.Contains(args, want) {
					t.Fatalf("missing %q in %s", want, args)
				}
			}
			if strings.Contains(args, "\n22\n") {
				t.Fatal("re-enabled port fallback")
			}
		})
	}
}

func TestBoundedSSHOutputCannotBypassLimitViaCopy(t *testing.T) {
	out := boundedSSHOutput{limit: 4}
	n, err := io.Copy(&out, strings.NewReader(strings.Repeat("x", 4096)))
	if err != nil || n != 4096 || len(out.String()) != 4 || !out.exceeded {
		t.Fatalf("n=%d err=%v retained=%d overflow=%t", n, err, len(out.String()), out.exceeded)
	}
}

func TestRunSSHOutputBoundedCancellationBeforeTransport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RunSSHOutputBounded(ctx, SSHTarget{}, "unused", 32); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestRunSSHOutputBoundedTargetWrapping(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell transport fixture")
	}
	for _, mode := range []string{windowsModeNormal, windowsModeWSL2} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "args")
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+shellQuote(log)+"\nprintf arm64\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			target := SSHTarget{Host: "win.example.test", Port: "2207", FallbackPorts: []string{}, TargetOS: targetWindows, WindowsMode: mode}
			command := "/bin/sh -c 'uname -m'"
			if mode == windowsModeNormal {
				command = "[Console]::WriteLine('arm64')"
			}
			if _, err := RunSSHOutputBounded(context.Background(), target, command, 32); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			want := wrapRemoteForTarget(target, command)
			if !strings.Contains(string(data), want) || !strings.HasPrefix(want, "powershell.exe ") {
				t.Fatalf("wrapper not used: %s", data)
			}
		})
	}
}

func TestRunSSHOutputBoundedFallbackSemantics(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell transport fixture")
	}
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "nil enables", true: "empty disables"}[disabled], func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "ports")
			script := `#!/bin/sh
while [ "$#" -gt 0 ]; do
 if [ "$1" = '-p' ]; then shift; port="$1"; fi
 shift
done
printf '%s\n' "$port" >> ` + shellQuote(log) + `
if [ "$port" = 22 ]; then printf arm64; exit 0; fi
exit 255
`
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			target := SSHTarget{Host: "build.example.test", Port: "2222", TargetOS: targetLinux}
			if disabled {
				target.FallbackPorts = []string{}
			}
			got, err := RunSSHOutputBounded(context.Background(), target, "unused", 32)
			data, readErr := os.ReadFile(log)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if disabled {
				if err == nil || got != "" || string(data) != "2222\n" {
					t.Fatalf("got=%q err=%v attempts=%s", got, err, data)
				}
			} else if err != nil || got != "arm64" || string(data) != "2222\n22\n" {
				t.Fatalf("got=%q err=%v attempts=%s", got, err, data)
			}
		})
	}
}
