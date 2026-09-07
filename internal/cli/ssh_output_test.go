package cli

import (
	"context"
	"crypto/sha256"
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

func TestRunSSHOutputBoundedNativeWindowsWrapping(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell transport fixture")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+shellQuote(log)+"\nprintf arm64\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	target := SSHTarget{Host: "win.example.test", Port: "2207", FallbackPorts: []string{}, TargetOS: targetWindows, WindowsMode: windowsModeNormal}
	command := "[Console]::WriteLine('arm64')"
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
}

func TestRunSSHOutputBoundedWSL2StagesCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell transport fixture")
	}
	dir := t.TempDir()
	executions, remotePath, stdinPath := filepath.Join(dir, "executions"), filepath.Join(dir, "remote"), filepath.Join(dir, "stdin")
	script := "#!/bin/sh\nprintf x >> " + shellQuote(executions) + "\nlast=; for arg; do last=$arg; done\nprintf '%s' \"$last\" > " + shellQuote(remotePath) + "\ncat > " + shellQuote(stdinPath) + "\nprintf arm64\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const nonce = "0123456789abcdef0123456789abcdef"
	var stageCount int
	var stagedCommand string
	var payload []byte
	var size int64
	var digest [sha256.Size]byte
	oldStage := stageWSLSpool
	stageWSLSpool = func(spool *wslStageSpool, _ context.Context, target *SSHTarget, _ wslStageTiming, _, _ string, _ io.Writer) (string, error) {
		stageCount++
		spool.shell = wslStageCMD
		target.NoControlMaster = true
		reader, err := spool.input.reset()
		if err != nil {
			return "", err
		}
		raw, err := io.ReadAll(reader)
		if err == nil {
			_, _, stagedCommand, payload = decodeWSLStage(t, raw)
			size, digest = spool.size, spool.digest()
		}
		return nonce, err
	}
	t.Cleanup(func() { stageWSLSpool = oldStage })

	target := SSHTarget{Host: "win.example.test", Port: "2207", FallbackPorts: []string{}, TargetOS: targetWindows, WindowsMode: windowsModeWSL2}
	got, err := RunSSHOutputBounded(context.Background(), target, "/bin/sh -c 'uname -m'", 32)
	if err != nil || got != "arm64" {
		t.Fatalf("output=%q err=%v", got, err)
	}
	if stageCount != 1 || stagedCommand != "/bin/sh -c 'uname -m'" || len(payload) != 0 {
		t.Fatalf("stages=%d command=%q payload=%d", stageCount, stagedCommand, len(payload))
	}
	launcher := wslStageLauncherCommand(nonce, size, digest, wslStageCMD)
	remote, remoteErr := os.ReadFile(remotePath)
	stdin, stdinErr := os.ReadFile(stdinPath)
	runs, runsErr := os.ReadFile(executions)
	if remoteErr != nil || stdinErr != nil || runsErr != nil {
		t.Fatalf("remote=%v stdin=%v executions=%v", remoteErr, stdinErr, runsErr)
	}
	if launcher == "" || len(launcher) >= wslStageLauncherCommandLimit || string(remote) != launcher || len(stdin) != 0 || string(runs) != "x" {
		t.Fatalf("launcher=%d remote=%t stdin=%d executions=%q", len(launcher), string(remote) == launcher, len(stdin), runs)
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
			} else if err != nil || got != "arm64" || string(data) != "2222\n22\n22\n" {
				t.Fatalf("got=%q err=%v attempts=%s", got, err, data)
			}
		})
	}
}
