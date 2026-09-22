package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestNativeWSLMetadataCommand(t *testing.T) {
	for _, shell := range []wslStageShell{wslStageCMD, wslStagePowerShell} {
		for _, args := range [][]string{
			{"__remote-control", "identity"},
			{"__remote-control", "control", "CBX-REMOTE-1", "owned-nonce", "status"},
			{"__remote-control", "control", "CBX-REMOTE-1", "owned-nonce", "cleanup", "7000"},
		} {
			command, err := nativeWSLMetadataCommand(`/opt/runtime fixtures/crabbox`, args, 5*time.Second, shell)
			if err != nil || len(command) > nativeWSLMetadataCommandLimit {
				t.Fatalf("shell=%s command size=%d error=%v", shell, len(command), err)
			}
			if shell == wslStagePowerShell && (!strings.HasPrefix(command, "& ([ScriptBlock]::Create(") || strings.Contains(command, "powershell.exe")) {
				t.Fatal("outer PowerShell must execute its script directly")
			}
			if shell == wslStageCMD && !strings.HasPrefix(command, "powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command ") {
				t.Fatal("cmd route must start PowerShell")
			}
			const marker = "FromBase64String('"
			_, encoded, found := strings.Cut(command, marker)
			if !found {
				t.Fatal("missing encoded script")
			}
			encoded, _, found = strings.Cut(encoded, "')")
			if !found {
				t.Fatal("missing encoded script terminator")
			}
			data, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatal(err)
			}
			script := string(data)
			for _, want := range []string{"ProcessStartInfo", "'wsl.exe'", `$i.Arguments='--exec "/opt/runtime fixtures/crabbox" "__remote-control"`, "WaitForExit(5000)", "WaitForExit(1000)", "$p.Kill()"} {
				if !strings.Contains(script, want) {
					t.Errorf("missing %q", want)
				}
			}
			for _, unexpected := range []string{"bash", "sh -", "RedirectStandardOutput", "RedirectStandardError", "WaitForExit()"} {
				if strings.Contains(script, unexpected) {
					t.Errorf("unexpected %q", unexpected)
				}
			}
		}
	}
	for _, shell := range []wslStageShell{"", "bash"} {
		if command, err := nativeWSLMetadataCommand("/runtime", []string{"__remote-control", "identity"}, time.Second, shell); command != "" || err == nil {
			t.Fatalf("unestablished route %q: command=%q error=%v", shell, command, err)
		}
	}
	for _, tt := range []struct {
		name, path string
		args       []string
		timeout    time.Duration
	}{
		{"relative", "bin/crabbox", []string{"__remote-control", "identity"}, time.Second},
		{"empty", "/runtime", nil, time.Second},
		{"workload", "/runtime", []string{"__remote-control", "run"}, time.Second},
		{"extra", "/runtime", []string{"__remote-control", "identity", "extra"}, time.Second},
		{"zero_timeout", "/runtime", []string{"__remote-control", "identity"}, 0},
		{"long_timeout", "/runtime", []string{"__remote-control", "identity"}, 2 * time.Minute},
		{"nul", "/runtime", []string{"__remote-control", "control", "CBX-REMOTE-1", "nonce\x00", "status"}, time.Second},
		{"long_arg", "/runtime", []string{"__remote-control", "control", "CBX-REMOTE-1", strings.Repeat("n", 257), "status"}, time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if command, err := nativeWSLMetadataCommand(tt.path, tt.args, tt.timeout, wslStageCMD); err == nil || command != "" {
				t.Fatalf("got command=%q error=%v", command, err)
			}
		})
	}
}

func TestNativeWSLShellResponse(t *testing.T) {
	nonce := strings.Repeat("a", 32)
	for _, shell := range []wslStageShell{wslStageCMD, wslStagePowerShell} {
		got, err := parseNativeWSLShell(nativeWSLRouteMarker+" "+nonce+" "+string(shell)+"\r\n", nonce)
		if err != nil || got != shell {
			t.Fatalf("shell=%s got=%s err=%v", shell, got, err)
		}
	}
	for _, record := range []string{"", nativeWSLRouteMarker + " stale cmd", nativeWSLRouteMarker + " " + nonce + " unknown", nativeWSLRouteMarker + " " + nonce + " cmd extra"} {
		if _, err := parseNativeWSLShell(record, nonce); err == nil {
			t.Fatalf("accepted invalid route response %q", record)
		}
	}
}

func TestNativeWSLPOSIXControlPortable(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("local PowerShell Core unavailable")
	}
	source := "printf '\\000\\377owned\\n'; printf 'owned stderr\\n' >&2; exit 23"
	for _, shell := range []wslStageShell{wslStageCMD, wslStagePowerShell} {
		command, err := nativeWSLPOSIXCommand(source, 5*time.Second, shell)
		if err != nil {
			t.Fatal(err)
		}
		_, encoded, ok := strings.Cut(command, "FromBase64String('")
		if !ok {
			t.Fatal("missing script encoding")
		}
		encoded, _, _ = strings.Cut(encoded, "')")
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		script := string(data)
		for _, replacement := range [][2]string{
			{`[Diagnostics.ProcessStartInfo]::new('wsl.exe')`, `[Diagnostics.ProcessStartInfo]::new('/usr/bin/env')`},
			{`'--exec "/usr/bin/env" `, `'`},
		} {
			if strings.Count(script, replacement[0]) != 1 {
				t.Fatal("portable control substitution missing")
			}
			script = strings.Replace(script, replacement[0], replacement[1], 1)
		}
		// PowerShell cold startup is outside the generated launcher's five-second child budget.
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		cmd := exec.CommandContext(ctx, pwsh, "-NoProfile", "-NonInteractive", "-Command", script)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		started := time.Now()
		err = cmd.Run()
		contextErr := ctx.Err()
		cancel()
		if contextErr != nil || exitCode(err) != 23 || !bytes.Equal(stdout.Bytes(), []byte{0, 255, 'o', 'w', 'n', 'e', 'd', '\n'}) || stderr.String() != "owned stderr\n" {
			t.Fatalf("shell=%s elapsed=%s context=%v err=%v exit=%d stdout=%x stderr=%q", shell, time.Since(started), contextErr, err, exitCode(err), stdout.Bytes(), stderr.String())
		}
	}
}

func TestNativeRuntimeTransportBudget(t *testing.T) {
	if budget, err := nativeRuntimeTransportBudget(t.Context()); err != nil || budget <= 0 {
		t.Fatalf("default budget=%s err=%v", budget, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if budget, err := nativeRuntimeTransportBudget(ctx); err != nil || budget <= 0 || budget > time.Second {
		t.Fatalf("remaining budget=%s err=%v", budget, err)
	}
	cancel()
	if _, err := nativeRuntimeTransportBudget(ctx); err != context.Canceled {
		t.Fatalf("cancellation=%v", err)
	}
}

func TestNativeWSLMetadataOwnedHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "--native-metadata-fixture" {
			continue
		}
		mode := os.Args[i+1]
		if mode == "wait" {
			time.Sleep(10 * time.Second)
			os.Exit(0)
		}
		data, err := json.Marshal(os.Args[i+2:])
		if err != nil {
			os.Exit(2)
		}
		os.Stdout.Write(data)
		os.Stdout.Write([]byte{0, 255, 13, 10})
		os.Stderr.Write([]byte{'e', 0, 254, 10})
		os.Exit(23)
	}
}

func TestNativeWSLMetadataPowerShellProcess(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("local PowerShell Core is unavailable")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	literalArgs := []string{"plain", "two words", `single'quote`, `double"quote`, `one\"quote`, `trailing\`, "", "é 🦞", "$literal;(&)"}
	args := append([]string{"-test.run=^TestNativeWSLMetadataOwnedHelper$", "--", "--native-metadata-fixture", "echo"}, literalArgs...)
	script := nativeRuntimeMetadataProcessScript(executable, quoteWindowsCommandArgs(args), 5*time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, pwsh, "-NoProfile", "-NonInteractive", "-Command", wslStagePowerShellCommand(script, wslStagePowerShell))
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	want, marshalErr := json.Marshal(literalArgs)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	want = append(want, 0, 255, 13, 10)
	if exitCode(err) != 23 || !bytes.Equal(stdout.Bytes(), want) || !bytes.Equal(stderr.Bytes(), []byte{'e', 0, 254, 10}) {
		t.Fatalf("owned metadata process: exit=%d stdout=%x stderr=%x err=%v", exitCode(err), stdout.Bytes(), stderr.Bytes(), err)
	}
}

func TestNativeWSLMetadataPowerShellTimeout(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("local PowerShell Core is unavailable")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"-test.run=^TestNativeWSLMetadataOwnedHelper$", "--", "--native-metadata-fixture", "wait"}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	script := nativeRuntimeMetadataProcessScript(executable, quoteWindowsCommandArgs(args), 100*time.Millisecond)
	command := exec.CommandContext(ctx, pwsh, "-NoProfile", "-NonInteractive", "-Command", wslStagePowerShellCommand(script, wslStagePowerShell))
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	if ctx.Err() != nil || exitCode(err) != 124 || stdout.Len() != 0 || strings.TrimSpace(stderr.String()) != "WSL metadata launcher timed out" {
		t.Fatalf("owned launcher timeout: exit=%d stdout=%q stderr=%q err=%v", exitCode(err), stdout.String(), stderr.String(), err)
	}
}
