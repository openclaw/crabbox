package blacksmith

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestBlacksmithReadinessTimeoutArgs(t *testing.T) {
	for _, timeout := range []time.Duration{0, 90 * time.Second, 30 * time.Minute} {
		cfg := core.BaseConfig()
		cfg.Blacksmith.WaitTimeout = timeout
		args := blacksmithRunArgs(cfg, "tbx_fixture", "", []string{"sleep", "600"}, false, false)
		want := ""
		if timeout > 0 {
			want = timeout.String()
		}
		if got := testBlacksmithFlag(args, "--wait-timeout"); got != want {
			t.Fatalf("timeout=%s argv=%v, want native wait %q", timeout, args, want)
		}
		if !strings.Contains(args[len(args)-1], "sleep") || strings.Contains(args[len(args)-1], "timeout") {
			t.Fatalf("readiness wait changed workload execution: %v", args)
		}
	}
}

func TestBlacksmithReadinessTimeoutCustody(t *testing.T) {
	for _, artifacts := range []bool{false, true} {
		for _, mode := range []string{"one-shot", "keep", "keep-on-failure", "reuse", "uncertain-stop"} {
			t.Run(mode+"/"+map[bool]string{false: "ordinary", true: "artifact"}[artifacts], func(t *testing.T) {
				isolateArtifactOwnership(t)
				repo := t.TempDir()
				t.Chdir(repo)
				const id = "tbx_wait_fixture"
				cfg := core.BaseConfig()
				cfg.Blacksmith.Workflow = ".github/workflows/testbox.yml"
				cfg.Blacksmith.WaitTimeout = 90 * time.Second
				var original core.LeaseClaim
				if mode == "reuse" {
					original = testOwnedBlacksmithClaim(t, id, "existing", repo)
					prepareBlacksmithGuestKey(t, id)
				}
				runs, stops := 0, 0
				runner := &blacksmithFuncRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
					switch req.Args[1] {
					case "warmup":
						if testBlacksmithFlag(req.Args, "--wait-timeout") != "" {
							t.Fatal("readiness timeout forwarded to dispatch")
						}
						return core.LocalCommandResult{Stdout: id + "\n"}, nil
					case "run":
						runs++
						if mode == "reuse" {
							var readErr error
							original, readErr = core.ReadLeaseClaim(id)
							if readErr != nil {
								t.Fatal(readErr)
							}
						}
						if testBlacksmithFlag(req.Args, "--wait-timeout") != "1m30s" {
							t.Fatalf("missing native readiness deadline: %v", req.Args)
						}
						return core.LocalCommandResult{ExitCode: 1}, errors.New("timed out waiting for testbox to become ready")
					case "stop":
						stops++
						if mode == "uncertain-stop" {
							return core.LocalCommandResult{ExitCode: 1}, errors.New("stop unconfirmed")
						}
					}
					return core.LocalCommandResult{}, nil
				}}
				b := newTestBlacksmithBackend(cfg, runner)
				req := core.RunRequest{Repo: core.Repo{Root: repo}, Command: []string{"true"}, Keep: mode == "keep", KeepOnFailure: mode == "keep-on-failure"}
				if artifacts {
					req.ArtifactGlobs = []string{"reports/**"}
				}
				if mode == "reuse" {
					req.ID = id
				}
				result, err := b.Run(t.Context(), req)
				if err == nil || result.ExitCode != 1 || runs != 1 {
					t.Fatalf("timeout lost or allocation repeated: result=%+v err=%v runs=%d", result, err, runs)
				}
				wantStops := 0
				if mode == "one-shot" || mode == "uncertain-stop" {
					wantStops = 1
				}
				claim, exists, readErr := core.ReadLeaseClaimWithPresence(id)
				if readErr != nil || stops != wantStops || exists != (mode != "one-shot") {
					t.Fatalf("custody changed: stops=%d claim=%t err=%v", stops, exists, readErr)
				}
				if mode == "reuse" && !reflect.DeepEqual(claim, original) {
					t.Fatal("readiness timeout changed reused claim")
				}
				if mode == "uncertain-stop" && (result.Session == nil || !result.Session.Kept) {
					t.Fatal("unconfirmed cleanup did not report retention")
				}
			})
		}
	}
}
