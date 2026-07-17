package blacksmith

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestFailedWarmupDoesNotStopConcurrentUnclaimedTestbox(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))

	boxExists := false
	listCalls := 0
	stopped := ""
	runner := &blacksmithFuncRunner{fn: func(req LocalCommandRequest) (LocalCommandResult, error) {
		if len(req.Args) < 2 || req.Args[0] != "testbox" {
			return LocalCommandResult{}, nil
		}
		switch req.Args[1] {
		case "list":
			listCalls++
			if boxExists {
				return LocalCommandResult{Stdout: "tbx_bee123 running example-org .github/workflows/testbox.yml check main 2026-07-14T10:00:00.000000Z\n"}, nil
			}
			return LocalCommandResult{Stdout: "ID STATUS REPO WORKFLOW JOB REF CREATED\n"}, nil
		case "warmup":
			// Another invocation can create its testbox before warmup returns
			// and before that invocation publishes a local lease claim.
			boxExists = true
			return LocalCommandResult{ExitCode: 1, Stdout: "error: delegated queue unavailable\n"}, errors.New("exit status 1")
		case "stop":
			for i, arg := range req.Args {
				if arg == "--id" && i+1 < len(req.Args) {
					stopped = req.Args[i+1]
				}
			}
			boxExists = false
		}
		return LocalCommandResult{}, nil
	}}

	cfg := baseConfig()
	cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
	cfg.Blacksmith.Job = "check"
	cfg.Blacksmith.Ref = "main"
	backend := newTestBlacksmithBackend(cfg, runner)

	if _, _, err := backend.warmupLease(context.Background(), Repo{Root: "/repo-a"}, false, ""); err == nil {
		t.Fatal("expected warmup failure")
	}
	if stopped != "" || !boxExists {
		t.Fatalf("failed warmup stopped concurrent testbox=%q exists=%v", stopped, boxExists)
	}
	if listCalls != 0 {
		t.Fatalf("failed warmup inferred ownership from %d inventory scan(s)", listCalls)
	}
}
