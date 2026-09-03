package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorHelpIncludesCommandContract(t *testing.T) {
	for _, flag := range []string{"--help", "-h"} {
		t.Run(flag, func(t *testing.T) {
			clearConfigEnv(t)
			t.Chdir(t.TempDir())
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			brokenConfig := []byte("broker: [invalid\n")
			if err := os.WriteFile(configPath, brokenConfig, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", configPath)
			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"doctor", flag})
			var exitErr ExitError
			if err != nil && (!AsExitError(err, &exitErr) || exitErr.Code != 0) {
				t.Fatalf("help error=%v stderr=%q", err, stderr.String())
			}
			text := stderr.String()
			boundary := strings.Index(text, "All flags:")
			if boundary < 0 {
				t.Fatal("doctor help omitted the full flag reference boundary")
			}
			for _, want := range []string{
				"Usage:\n  crabbox doctor [flags]",
				"Modes:",
				"crabbox doctor --provider aws",
				"crabbox doctor --id blue-box",
				"crabbox doctor --from-run run_",
				"crabbox doctor --pond my-pond",
				"crabbox doctor --all --prepare-check",
				"crabbox doctor --json",
				"--provider <name>", "--profile <name>", "--id <lease-id-or-slug>",
				"--from-run <run-id>", "--pond <name>", "--providers <list>",
				"--doctor-probe-ssh", "--target linux|macos|windows",
			} {
				if !strings.Contains(text[:boundary], want) {
					t.Fatalf("doctor help must show %q before the full flag reference", want)
				}
			}
			for _, want := range []string{"-local-container-runtime", "-windows-mode"} {
				if !strings.Contains(text[boundary:], want) {
					t.Fatalf("doctor help lost provider flag %q", want)
				}
			}
			if stdout.Len() != 0 {
				t.Fatal("doctor help changed its output stream")
			}
			if got, err := os.ReadFile(configPath); err != nil || !bytes.Equal(got, brokenConfig) {
				t.Fatalf("doctor help changed config: %v", err)
			}
		})
	}
}

func TestCopyHelpIncludesCommandContract(t *testing.T) {
	for _, flag := range []string{"--help", "-h"} {
		t.Run(flag, func(t *testing.T) {
			clearConfigEnv(t)
			var stdout, stderr bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"cp", flag})
			var exitErr ExitError
			if err != nil && (!AsExitError(err, &exitErr) || exitErr.Code != 0) {
				t.Fatalf("help error=%v stderr=%q", err, stderr.String())
			}
			text := stderr.String()
			for _, want := range []string{
				"Usage:\n  crabbox cp --id <lease-id-or-slug> [-L] <src> <dst>",
				"exactly one path must use SANDBOX:PATH",
				"--provider <name>",
				"./file.txt SANDBOX:/tmp/file.txt",
				"SANDBOX:/tmp/file.txt ./file.txt",
				"-L",
			} {
				if at := strings.Index(text, want); at < 0 || at > strings.Index(text, "All flags:") {
					t.Fatalf("copy help must show %q before the full flag reference", want)
				}
			}
			if !strings.Contains(text, "-local-container-runtime") || stdout.Len() != 0 {
				t.Fatal("copy help lost the provider reference or changed its output stream")
			}
		})
	}
}

func TestTopLevelHelpListsRegisteredXCPNgProvider(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"--help"})
	if err != nil {
		t.Fatalf("crabbox --help error=%v stderr=%q", err, stderr.String())
	}
	text := stdout.String()
	line := helpLineContaining(text, "CRABBOX_PROVIDER")
	if line == "" {
		t.Fatalf("top-level help omitted CRABBOX_PROVIDER:\n%s", text)
	}
	if !strings.Contains(line, "xcp-ng") {
		t.Fatalf("top-level CRABBOX_PROVIDER help omitted registered xcp-ng provider:\n%s", line)
	}
}

func TestCleanupHelpListsRegisteredXCPNgProvider(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"cleanup", "--help"})
	if err != nil {
		var exitErr ExitError
		if !AsExitError(err, &exitErr) || exitErr.Code != 0 {
			t.Fatalf("crabbox cleanup --help error=%v stderr=%q", err, stderr.String())
		}
	}
	line := helpLineContaining(stderr.String(), "provider:")
	if line == "" {
		t.Fatalf("cleanup help omitted provider flag:\n%s", stderr.String())
	}
	if !strings.Contains(line, "xcp-ng") {
		t.Fatalf("cleanup provider help omitted registered xcp-ng cleanup provider:\n%s", line)
	}
}

func TestWarmupHelpDescribesConfiguredProviderWithoutCompiledDefault(t *testing.T) {
	isolateDoctorProviderSelectionTest(t)
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).warmup(context.Background(), []string{"--help"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 0 {
		t.Fatalf("crabbox warmup --help error=%v stderr=%q", err, stderr.String())
	}
	help := stderr.String()
	if strings.Contains(help, `default "hetzner"`) {
		t.Fatalf("warmup help exposed compiled provider default:\n%s", help)
	}
	if !strings.Contains(help, "defaults to configured selection") {
		t.Fatalf("warmup help omitted configured-selection guidance:\n%s", help)
	}
	if !strings.Contains(help, `default "default"`) {
		t.Fatalf("warmup help suppressed unrelated defaults:\n%s", help)
	}
}

func TestLiteralProviderDefaultsRemainVisibleInHelp(t *testing.T) {
	for _, command := range []struct {
		name string
		run  func(App) error
		want string
	}{
		{name: "marketplace quote", run: func(app App) error { return app.marketplaceQuote(context.Background(), []string{"--help"}) }, want: `default "auto"`},
		{name: "image promote", run: func(app App) error { return app.imagePromote(context.Background(), []string{"--help"}) }, want: `default "aws"`},
	} {
		t.Run(command.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := command.run(App{Stdout: &stdout, Stderr: &stderr})
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 0 {
				t.Fatalf("help error=%v stderr=%q", err, stderr.String())
			}
			help := stderr.String()
			if helpLineContaining(help, "-provider string") == "" || !strings.Contains(help, command.want) {
				t.Fatalf("provider help missing %q\n%s", command.want, help)
			}
		})
	}
}

func TestRunHelpDescribesStandaloneScriptUpload(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"run", "--help"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 0 {
		t.Fatalf("crabbox run --help error=%v stderr=%q", err, stderr.String())
	}
	want := "on POSIX SSH leases, upload and run a standalone content-hashed copy under .crabbox/scripts/; delegated module runtimes use source input"
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("run help omitted standalone script upload semantics:\n%s", stderr.String())
	}
}

func TestRunHelpDescribesRetainedLeaseOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"run", "--help"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 0 {
		t.Fatalf("crabbox run --help error=%v stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "write a retained JSON lease handle for orchestrators on supported providers") {
		t.Fatalf("run help omitted retained lease-output semantics:\n%s", stderr.String())
	}
}

func TestTopLevelAndCommandHelpDescribeInteractiveConnect(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run(context.Background(), []string{"--help"}); err != nil {
		t.Fatalf("crabbox --help error=%v", err)
	}
	if !strings.Contains(stdout.String(), "connect     Open an interactive SSH session to a lease") {
		t.Fatalf("top-level help omitted connect:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	err := app.Run(context.Background(), []string{"connect", "--help"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 0 {
		t.Fatalf("crabbox connect --help error=%v stderr=%q", err, stderr.String())
	}
	help := stderr.String()
	if !strings.Contains(help, "-id string") || !strings.Contains(help, "-network string") {
		t.Fatalf("connect help omitted lease flags:\n%s", help)
	}
	if strings.Contains(help, "show-secret") {
		t.Fatalf("connect help exposed print-only show-secret flag:\n%s", help)
	}
}

func TestCheckpointCreateHelpListsAzureSnapshotSKU(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"checkpoint", "create", "--help"})
	var exitErr ExitError
	if !AsExitError(err, &exitErr) || exitErr.Code != 0 {
		t.Fatalf("crabbox checkpoint create --help error=%v stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "-azure-snapshot-sku string") {
		t.Fatalf("checkpoint create help omitted Azure snapshot SKU:\n%s", stderr.String())
	}
}

func helpLineContaining(text, want string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	return ""
}
