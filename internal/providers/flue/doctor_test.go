package flue

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDoctorChecksLocalFlueSurfaceAndPathsWithoutRunningWorkflow(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "flue.config.ts"), []byte("export default {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("TOKEN=redacted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Flue.Root = root
	cfg.Flue.Config = "flue.config.ts"
	cfg.Flue.EnvFile = ".env"
	cfg.Flue.Output = "json"
	cfg.Flue.CLIPath = "/opt/flue/bin/flue"
	runner := &recordingRunner{fn: func(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "--help":
			if req.Dir != root {
				t.Fatalf("help Dir=%q want %q", req.Dir, root)
			}
			return LocalCommandResult{ExitCode: 0, Stdout: "Usage: flue run workflow:name --input <json>\n"}, nil
		case "--version":
			return LocalCommandResult{ExitCode: 1, Stderr: "version unavailable"}, errors.New("version unavailable")
		default:
			t.Fatalf("doctor must not run workflow, got args=%v", req.Args)
			return LocalCommandResult{}, nil
		}
	}}
	result, err := testBackend(cfg, runner, io.Discard, io.Discard).Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor err=%v", err)
	}
	if result.Status != "ok" || !strings.Contains(result.Message, "workflow_discovery=unchecked") {
		t.Fatalf("result=%#v", result)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("runner calls=%#v want help and version only", runner.calls)
	}
	assertDoctorCheck(t, result, "flue_help", "ok")
	assertDoctorCheck(t, result, "flue_root", "ok")
	assertDoctorCheck(t, result, "config", "ok")
	assertDoctorCheck(t, result, "env_file", "ok")
	assertDoctorCheck(t, result, "output", "ok")
	assertDoctorCheck(t, result, "target", "ok")
	workflow := assertDoctorCheck(t, result, "workflow", "warning")
	if workflow.Details["discoverability"] != "unchecked" || workflow.Details["mutation"] != "false" {
		t.Fatalf("workflow check=%#v", workflow)
	}
	version := assertDoctorCheck(t, result, "flue_version", "warning")
	if version.Details["optional"] != "true" {
		t.Fatalf("version check=%#v", version)
	}
}

func TestDoctorReportsMissingRootAndUnsupportedTarget(t *testing.T) {
	cfg := testConfig()
	cfg.Flue.Root = filepath.Join(t.TempDir(), "missing")
	cfg.Flue.Target = "cloudflare"
	runner := &recordingRunner{fn: func(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
		if req.Dir != "" {
			t.Fatalf("doctor should not use missing root as command dir, got %q", req.Dir)
		}
		switch strings.Join(req.Args, " ") {
		case "--help":
			return LocalCommandResult{ExitCode: 0, Stdout: "Usage: flue\n"}, nil
		case "--version":
			return LocalCommandResult{ExitCode: 0, Stdout: "flue 1.2.3\n"}, nil
		default:
			t.Fatalf("unexpected args=%v", req.Args)
			return LocalCommandResult{}, nil
		}
	}}
	result, err := testBackend(cfg, runner, io.Discard, io.Discard).Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor err=%v", err)
	}
	if result.Status != "error" {
		t.Fatalf("result=%#v", result)
	}
	root := assertDoctorCheck(t, result, "flue_root", "failed")
	if root.Details["path"] == "" {
		t.Fatalf("root details=%#v", root.Details)
	}
	target := assertDoctorCheck(t, result, "target", "failed")
	if !strings.Contains(target.Message, "target=node only") {
		t.Fatalf("target check=%#v", target)
	}
}

func TestDoctorReportsUnreadableConfiguredFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod unreadable fixture is Unix-specific")
	}
	root := t.TempDir()
	config := filepath.Join(root, "flue.config.ts")
	if err := os.WriteFile(config, []byte("export default {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(config, 0); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(config, 0o600) }()
	if file, err := os.Open(config); err == nil {
		_ = file.Close()
		t.Skip("current user can read chmod 000 files")
	}

	cfg := testConfig()
	cfg.Flue.Root = root
	cfg.Flue.Config = "flue.config.ts"
	runner := &recordingRunner{fn: func(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
		switch strings.Join(req.Args, " ") {
		case "--help":
			return LocalCommandResult{ExitCode: 0, Stdout: "Usage: flue\n"}, nil
		case "--version":
			return LocalCommandResult{ExitCode: 0, Stdout: "flue 1.2.3\n"}, nil
		default:
			t.Fatalf("unexpected args=%v", req.Args)
			return LocalCommandResult{}, nil
		}
	}}
	result, err := testBackend(cfg, runner, io.Discard, io.Discard).Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor err=%v", err)
	}
	if result.Status != "error" {
		t.Fatalf("result=%#v", result)
	}
	configCheck := assertDoctorCheck(t, result, "config", "failed")
	if !strings.Contains(configCheck.Message, "not readable") {
		t.Fatalf("config check=%#v", configCheck)
	}
}

func TestDoctorReportsMissingCLIWithoutMutation(t *testing.T) {
	cfg := testConfig()
	runner := &recordingRunner{fn: func(_ context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
		if len(req.Args) == 1 && req.Args[0] == "--help" {
			return LocalCommandResult{ExitCode: 127, Stderr: "flue not found"}, errors.New("not found")
		}
		if len(req.Args) == 1 && req.Args[0] == "--version" {
			return LocalCommandResult{ExitCode: 127, Stderr: "flue not found"}, errors.New("not found")
		}
		t.Fatalf("unexpected args=%v", req.Args)
		return LocalCommandResult{}, nil
	}}
	result, err := testBackend(cfg, runner, io.Discard, io.Discard).Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatalf("Doctor err=%v", err)
	}
	if result.Status != "error" {
		t.Fatalf("result=%#v", result)
	}
	help := assertDoctorCheck(t, result, "flue_help", "failed")
	if !strings.Contains(help.Message, "flue not found") || help.Details["mutation"] != "false" {
		t.Fatalf("help check=%#v", help)
	}
}

func assertDoctorCheck(t *testing.T, result DoctorResult, name, status string) DoctorCheck {
	t.Helper()
	for _, check := range result.Checks {
		if check.Check == name {
			if check.Status != status {
				t.Fatalf("check %s status=%q want %q: %#v", name, check.Status, status, check)
			}
			return check
		}
	}
	t.Fatalf("missing doctor check %q in %#v", name, result.Checks)
	return DoctorCheck{}
}
