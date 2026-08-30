package applecontainer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

type pinnedImageRunner struct {
	calls         []core.LocalCommandRequest
	configuration map[string]any
	state         string
	deleted       bool
	inspects      int
	hook          func(*pinnedImageRunner, core.LocalCommandRequest) (core.LocalCommandResult, error, bool)
}

func (r *pinnedImageRunner) Run(_ context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	r.calls = append(r.calls, req)
	if r.hook != nil {
		if out, err, handled := r.hook(r, req); handled {
			return out, err
		}
	}
	switch req.Args[0] {
	case "create":
		labels := map[string]string{}
		name, image := "", ""
		for i, arg := range req.Args {
			if arg == "--name" {
				name = req.Args[i+1]
			}
			if arg == "--label" {
				k, v, _ := strings.Cut(req.Args[i+1], "=")
				labels[k] = v
			}
			if strings.HasPrefix(arg, "docker.io/library/ubuntu@") {
				image = arg
			}
		}
		digest, _ := core.DefaultContainerImageDigest(image)
		r.configuration = map[string]any{"id": name, "labels": labels, "image": map[string]any{"reference": image, "descriptor": map[string]string{"digest": digest}}, "process": map[string]any{"arguments": []string{"/bin/sh", "-lc", "bootstrap"}}}
		r.state = "stopped"
		return core.LocalCommandResult{Stdout: name + "\n"}, nil
	case "inspect":
		r.inspects++
		data, _ := json.Marshal([]any{map[string]any{"configuration": r.configuration, "status": map[string]string{"state": r.state}}})
		return core.LocalCommandResult{Stdout: string(data)}, nil
	case "start":
		r.state = "running"
	case "delete":
		r.deleted = true
	case "ls":
		return core.LocalCommandResult{Stdout: "[]"}, nil
	}
	return core.LocalCommandResult{}, nil
}

func pinnedFixture(t *testing.T) (*backend, *pinnedImageRunner, core.Config) {
	t.Helper()
	testutil.IsolateUserDirs(t)
	r := &pinnedImageRunner{}
	cfg := core.BaseConfig()
	cfg.AppleContainer.CLIPath = "container"
	cfg.AppleContainer.ExtraRunArgs = []string{"--dns", "1.1.1.1"}
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Exec: r, Stdout: io.Discard, Stderr: io.Discard}).(*backend)
	return b, r, cfg
}

func TestPinnedDefaultVerifiesCreatedImageBeforeStart(t *testing.T) {
	b, r, cfg := pinnedFixture(t)
	id, err := b.createContainer(t.Context(), cfg, "crabbox-fixture", "cbx_pin_test", "pin-test", "public-fixture", false)
	if err != nil || id != "crabbox-fixture" {
		t.Fatal(id, err)
	}
	var verbs []string
	for _, call := range r.calls {
		verbs = append(verbs, call.Args[0])
	}
	if strings.Join(verbs, ",") != "create,inspect,inspect,start" {
		t.Fatal(verbs)
	}
	if r.state != "running" {
		t.Fatal("verified container not started")
	}
	if commandWasCalled(r.calls, "run") {
		t.Fatal("unverified run reached")
	}
	if len(r.calls[0].Args) > 1 && r.calls[0].Args[1] == "-d" {
		t.Fatal("run-only detach passed to create")
	}
}

func TestPinnedDefaultRejectsBeforeBootstrap(t *testing.T) {
	for _, kind := range []string{"digest-mismatch", "missing-digest", "wrong-id", "wrong-label", "wrong-reference", "running", "malformed", "multiple", "create-failure", "start-failure", "changed-config", "appeared-claim", "rollback-changed-config", "rollback-ambiguous-inventory", "rollback-appeared-claim"} {
		t.Run(kind, func(t *testing.T) {
			b, r, cfg := pinnedFixture(t)
			r.hook = func(r *pinnedImageRunner, req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				switch req.Args[0] {
				case "create":
					if kind == "create-failure" {
						return core.LocalCommandResult{ExitCode: 1}, nil, true
					}
				case "start":
					if kind == "start-failure" {
						return core.LocalCommandResult{ExitCode: 1}, nil, true
					}
				case "ls":
					if kind == "rollback-ambiguous-inventory" {
						return core.LocalCommandResult{Stdout: "null"}, nil, true
					}
				case "inspect":
					image := r.configuration["image"].(map[string]any)
					if r.inspects == 0 {
						if kind == "appeared-claim" || kind == "rollback-appeared-claim" {
							if err := core.ClaimLeaseForRepoProvider("cbx_pin_test", "successor", providerName, t.TempDir(), 0, false); err != nil {
								t.Fatal(err)
							}
						}
						switch kind {
						case "digest-mismatch", "rollback-changed-config", "rollback-ambiguous-inventory", "rollback-appeared-claim":
							image["descriptor"] = map[string]string{"digest": "sha256:" + strings.Repeat("a", 64)}
						case "missing-digest":
							image["descriptor"] = map[string]string{}
						case "wrong-reference":
							image["reference"] = "ubuntu:26.04"
						case "wrong-id":
							r.configuration["id"] = "someone-else"
						case "wrong-label":
							r.configuration["labels"].(map[string]string)["lease"] = "cbx_other"
						case "running":
							r.state = "running"
						case "malformed":
							return core.LocalCommandResult{Stdout: "{"}, nil, true
						case "multiple":
							return core.LocalCommandResult{Stdout: "[{},{}]"}, nil, true
						}
					} else if kind == "changed-config" || kind == "rollback-changed-config" {
						r.configuration["hostname"] = "replacement"
					}
				}
				return core.LocalCommandResult{}, nil, false
			}
			_, err := b.createContainer(t.Context(), cfg, "crabbox-fixture", "cbx_pin_test", "pin-test", "public-fixture", false)
			if err == nil {
				t.Fatal("unsafe image accepted")
			}
			if kind != "start-failure" && commandWasCalled(r.calls, "start") {
				t.Fatal("bootstrap reached after failed verification")
			}
			if commandWasCalled(r.calls, "run") {
				t.Fatal("unverified run reached")
			}
			rollbackExpected := kind == "digest-mismatch" || kind == "rollback-ambiguous-inventory"
			if r.deleted != rollbackExpected {
				t.Fatalf("deleted=%t expected=%t", r.deleted, rollbackExpected)
			}
			var retained *retainedImageContainerError
			if errors.As(err, &retained) != (kind != "digest-mismatch") {
				t.Fatal("incorrect key/resource retention contract", err)
			}
		})
	}
}

func TestCustomContainerImagePreservesNativeRunPath(t *testing.T) {
	b, r, cfg := pinnedFixture(t)
	cfg.AppleContainer.Image = "ubuntu:26.04"
	r.hook = func(_ *pinnedImageRunner, req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		return core.LocalCommandResult{Stdout: "custom-container\n"}, nil, true
	}
	id, err := b.createContainer(t.Context(), cfg, "custom-container", "cbx_pin_test", "pin-test", "public-fixture", false)
	if err != nil || id != "custom-container" || len(r.calls) != 1 || r.calls[0].Args[0] != "run" {
		t.Fatal("custom image behavior changed", err)
	}
}

func TestCancelledPinnedCreateDoesNotRetainAnUncreatedTarget(t *testing.T) {
	b, r, cfg := pinnedFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := b.createContainer(ctx, cfg, "crabbox-fixture", "cbx_pin_test", "pin-test", "public-fixture", false)
	var retained *retainedImageContainerError
	if !errors.Is(err, context.Canceled) || errors.As(err, &retained) || len(r.calls) != 0 {
		t.Fatal("cancelled create changed resource/key retention", err)
	}
}
