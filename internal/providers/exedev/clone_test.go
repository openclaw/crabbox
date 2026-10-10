package exedev

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func cloneConfig(t *testing.T, args ...string) core.Config {
	t.Helper()
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	fs := flag.NewFlagSet("clone", flag.ContinueOnError)
	values := (Provider{}).RegisterFlags(fs, cfg)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if err := (Provider{}).ApplyFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestCloneUsesOwnNameTagsAndAdvertisedRoute(t *testing.T) {
	cfg := cloneConfig(t, "--exe-dev-from", "base", "--exe-dev-cpus", "1", "--exe-dev-memory", "1GB")
	leaseID, slug, generation := "cbx_abcdef123456", "blue", "cbx_111111111111"
	name := core.LeaseProviderName(leaseID, slug)
	var vm exeDevVM
	runner := &exeDevRecordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		cmd := req.Args[len(req.Args)-1]
		switch {
		case strings.HasPrefix(cmd, "cp "):
			for _, want := range []string{"cp base " + name, "--copy-tags=false", "--json", "--cpu 1", "--memory 1GB"} {
				if !strings.Contains(cmd, want) {
					t.Fatalf("clone command %q missing %q", cmd, want)
				}
			}
			if strings.Contains(cmd, "--image") || strings.Contains(cmd, "--command") {
				t.Fatalf("new-only option: %s", cmd)
			}
			vm = exeDevVM{VMName: name}
		case strings.HasPrefix(cmd, "tag --json "+name+" "):
			vm.Tags = strings.Fields(strings.TrimPrefix(cmd, "tag --json "+name+" "))
		case cmd == "ls --l --json":
			vm.SSHDest = "builder@advertised.example:2207"
			out, _ := json.Marshal(exeDevListResponse{VMs: []exeDevVM{vm}})
			return core.LocalCommandResult{Stdout: string(out)}, nil
		default:
			t.Fatalf("unexpected control command: %s", cmd)
		}
		out, _ := json.Marshal(vm)
		return core.LocalCommandResult{Stdout: string(out)}, nil
	}}
	b := newExeDevTestBackend(cfg, runner)
	got, created, err := b.createVM(context.Background(), cfg, name, leaseID, slug, generation)
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if got.Name() != name || got.SSHDest != "builder@advertised.example:2207" {
		t.Fatalf("vm=%+v", got)
	}
	for _, tag := range []string{"crabbox", "crabbox-lease-cbx_abcdef123456", "crabbox-slug-blue", "crabbox-claim-cbx_111111111111"} {
		if !slices.Contains(got.Tags, tag) {
			t.Fatalf("missing tag %q: %v", tag, got.Tags)
		}
	}
	if err := validateExeDevClaimGeneration(got, generation); err != nil {
		t.Fatal(err)
	}
}

func TestCloneWaitsForInventory(t *testing.T) {
	for _, phase := range []string{"before tagging", "after tagging"} {
		t.Run(phase, func(t *testing.T) {
			cfg := cloneConfig(t, "--exe-dev-from", "base")
			leaseID, slug, generation := "cbx_abcdef123456", "blue", "cbx_111111111111"
			name := core.LeaseProviderName(leaseID, slug)
			vm := exeDevVM{VMName: name, SSHDest: "builder@advertised.example:2207"}
			var tagged, hidden bool
			observations := 0
			runner := &exeDevRecordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				cmd := req.Args[len(req.Args)-1]
				switch {
				case strings.HasPrefix(cmd, "cp "):
				case strings.HasPrefix(cmd, "tag --json "+name+" "):
					tagged = true
					vm.Tags = strings.Fields(strings.TrimPrefix(cmd, "tag --json "+name+" "))
				case cmd == "ls --l --json":
					observations++
					vms := []exeDevVM{vm}
					if !hidden && tagged == (phase == "after tagging") {
						hidden = true
						vms = nil
					}
					out, _ := json.Marshal(exeDevListResponse{VMs: vms})
					return core.LocalCommandResult{Stdout: string(out)}, nil
				default:
					t.Fatalf("unexpected command: %s", cmd)
				}
				out, _ := json.Marshal(vm)
				return core.LocalCommandResult{Stdout: string(out)}, nil
			}}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			got, created, err := newExeDevTestBackend(cfg, runner).createVM(ctx, cfg, name, leaseID, slug, generation)
			if err != nil || !created || !tagged || observations < 3 {
				t.Fatalf("created=%v tagged=%v observations=%d err=%v", created, tagged, observations, err)
			}
			if err := validateExeDevClaimGeneration(got, generation); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCloneRejectsImageAndCommandBeforeProvisioning(t *testing.T) {
	for _, option := range []string{"--exe-dev-image", "--exe-dev-command"} {
		t.Run(option, func(t *testing.T) {
			cfg := cloneConfig(t, "--exe-dev-from", "base", option, "value")
			runner := &exeDevRecordingRunner{}
			_, _, err := newExeDevTestBackend(cfg, runner).createVM(context.Background(), cfg, "clone", "lease", "slug", "generation")
			if err == nil || len(runner.calls) != 0 {
				t.Fatalf("err=%v calls=%d", err, len(runner.calls))
			}
		})
	}
}

func TestCloneFailureDoesNotTagOrDeleteSource(t *testing.T) {
	cfg := cloneConfig(t, "--exe-dev-from", "base")
	runner := &exeDevRecordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if !strings.HasPrefix(req.Args[len(req.Args)-1], "cp base clone ") {
			t.Fatalf("unexpected command: %v", req.Args)
		}
		return core.LocalCommandResult{ExitCode: 1}, errors.New("copy failed")
	}}
	_, created, err := newExeDevTestBackend(cfg, runner).createVM(context.Background(), cfg, "clone", "lease", "slug", "generation")
	if err == nil || created || len(runner.calls) != 1 {
		t.Fatalf("created=%v err=%v calls=%d", created, err, len(runner.calls))
	}
}

func TestCloneMissingOrForbiddenBaseDoesNotMutateVMs(t *testing.T) {
	for _, message := range []string{"VM not found", "permission denied"} {
		for _, exitCode := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/exit-%d", message, exitCode), func(t *testing.T) {
				cfg := cloneConfig(t, "--exe-dev-from", "base")
				runner := &exeDevRecordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
					if cmd := req.Args[len(req.Args)-1]; !strings.HasPrefix(cmd, "cp base clone ") {
						t.Fatalf("unexpected command after rejected copy: %s", cmd)
					}
					out, _ := json.Marshal(map[string]string{"error": message})
					var err error
					if exitCode != 0 {
						err = errors.New("control command failed")
					}
					return core.LocalCommandResult{Stdout: string(out), ExitCode: exitCode}, err
				}}
				_, created, err := newExeDevTestBackend(cfg, runner).createVM(t.Context(), cfg, "clone", "lease", "slug", "generation")
				if err == nil || !strings.Contains(err.Error(), message) || created || len(runner.calls) != 1 {
					t.Fatalf("created=%v err=%v calls=%d", created, err, len(runner.calls))
				}
			})
		}
	}
}

func TestCloneWorkRootAndArchiveCapabilities(t *testing.T) {
	cfg := cloneConfig(t)
	if cfg.WorkRoot != "/var/tmp/crabbox" {
		t.Fatalf("work root=%q", cfg.WorkRoot)
	}
	spec := (Provider{}).Spec()
	for _, feature := range []core.Feature{core.FeatureCheckpoint, core.FeatureFork, core.FeatureRestore, core.FeatureSnapshot} {
		if slices.Contains(spec.Features, feature) {
			t.Fatalf("shared archive fallback must not advertise native capability %s", feature)
		}
	}
}

func TestCloneResolvePreservesClaimedWorkRoot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID, slug := "cbx_abcdef123456", "blue"
	vm := ownedExeDevVM(leaseID, slug)
	old := core.Config{WorkRoot: "/tmp/crabbox", ExeDev: core.ExeDevConfig{WorkRoot: "/tmp/crabbox"}}
	applyExeDevDefaults(&old)
	repoRoot := t.TempDir()
	persistExeDevClaim(t, old, vm, leaseID, slug, repoRoot)
	runner := exeDevInventoryRunner(t, vm)
	lease, err := newExeDevTestBackend(cloneConfig(t), runner).Resolve(t.Context(), core.ResolveRequest{ID: leaseID, StatusOnly: true, NoLocalStateMutations: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Labels["work_root"] != "/tmp/crabbox" {
		t.Fatalf("recorded root lost: server=%v target=%+v", lease.Server.Labels, lease.SSH)
	}
	_, err = newExeDevTestBackend(cloneConfig(t), runner).Resolve(t.Context(), core.ResolveRequest{ID: leaseID, Repo: core.Repo{Root: repoRoot}})
	if err != nil {
		t.Fatal(err)
	}
	claim, _, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || claim.Labels["work_root"] != "/tmp/crabbox" {
		t.Fatalf("refreshed root=%v err=%v", claim.Labels, err)
	}
}

func TestCloneAcquireFailureAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name                                                                        string
		keep, tagLost, tagRejected, replacement, malformed, wrongName, readyFailure bool
		ownedBase                                                                   bool
		copyResponse                                                                string
		wantError, wantDelete                                                       bool
	}{
		{name: "normal release", wantDelete: true},
		{name: "Crabbox lease as base", ownedBase: true, wantDelete: true},
		{name: "acknowledgement-only copy response", copyResponse: `{}`, wantDelete: true},
		{name: "lost tag response", tagLost: true, wantDelete: true},
		{name: "rejected tags", tagRejected: true, wantError: true},
		{name: "rejected tags retained", keep: true, tagRejected: true, wantError: true},
		{name: "route generation replaced", replacement: true, wantError: true},
		{name: "malformed copy response", malformed: true, wantError: true},
		{name: "non-object copy response", copyResponse: `[]`, wantError: true},
		{name: "null copy response", copyResponse: `null`, wantError: true},
		{name: "scalar copy response", copyResponse: `true`, wantError: true},
		{name: "wrong copy name", wrongName: true, wantError: true},
		{name: "SSH failure", readyFailure: true, wantError: true, wantDelete: true},
		{name: "SSH failure retained", readyFailure: true, keep: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			source := exeDevVM{VMName: "base", Tags: []string{"private-base-tag"}}
			if tc.ownedBase {
				source = ownedExeDevVM("cbx_222222222222", "source")
			}
			cfg := cloneConfig(t, "--exe-dev-from", source.Name())
			var vm exeDevVM
			var deleted, tagged, connected bool
			inventoryAfterTag := 0
			runner := &exeDevRecordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				cmd := req.Args[len(req.Args)-1]
				fields := strings.Fields(cmd)
				switch fields[0] {
				case "whoami":
					return core.LocalCommandResult{Stdout: `{"email":"test@example.com"}`}, nil
				case "cp":
					if fields[1] != source.Name() || fields[2] == source.Name() || !slices.Contains(fields, "--copy-tags=false") {
						t.Fatalf("copy must leave source identity behind: %s", cmd)
					}
					vm = exeDevVM{VMName: fields[2], Status: "running"}
					if tc.malformed {
						return core.LocalCommandResult{Stdout: "{"}, nil
					}
					if tc.copyResponse != "" {
						return core.LocalCommandResult{Stdout: tc.copyResponse}, nil
					}
					if tc.wrongName {
						return core.LocalCommandResult{Stdout: `{"vm_name":"unrelated"}`}, nil
					}
				case "tag":
					if fields[2] == source.Name() {
						t.Fatal("mutated source")
					}
					tagged = true
					if tc.tagRejected {
						return core.LocalCommandResult{ExitCode: 1}, errors.New("tag rejected")
					}
					vm.Tags = fields[3:]
					if tc.tagLost {
						return core.LocalCommandResult{ExitCode: 1}, errors.New("tag response lost")
					}
				case "ls":
					if tagged && !tc.tagRejected {
						inventoryAfterTag++
						if inventoryAfterTag >= 2 {
							vm.SSHDest = "builder@clone.example:2207"
							if tc.replacement {
								for i, tag := range vm.Tags {
									if strings.HasPrefix(tag, "crabbox-claim-") {
										vm.Tags[i] = "crabbox-claim-cbx_000000000000"
									}
								}
							}
						}
					}
					vms := []exeDevVM{source}
					if vm.Name() != "" && !deleted {
						vms = append(vms, vm)
					}
					out, _ := json.Marshal(exeDevListResponse{VMs: vms})
					return core.LocalCommandResult{Stdout: string(out)}, nil
				case "rm":
					if fields[1] != vm.Name() || fields[1] == source.Name() {
						t.Fatalf("wrong deletion: %s", cmd)
					}
					deleted = true
					return core.LocalCommandResult{Stdout: `{}`}, nil
				default:
					t.Fatalf("unexpected command: %s", cmd)
				}
				out, _ := json.Marshal(vm)
				return core.LocalCommandResult{Stdout: string(out)}, nil
			}}
			oldWait := waitForSSHReady
			t.Cleanup(func() { waitForSSHReady = oldWait })
			waitForSSHReady = func(_ context.Context, target *core.SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
				connected = true
				if target.Host != "clone.example" || target.User != "builder" || target.Port != "2207" {
					t.Fatalf("target=%+v", target)
				}
				if tc.readyFailure {
					return errors.New("SSH failed")
				}
				return nil
			}
			backend := newExeDevTestBackend(cfg, runner)
			lease, err := backend.Acquire(t.Context(), core.AcquireRequest{Repo: core.Repo{Root: t.TempDir(), Name: "my-app"}, Keep: tc.keep})
			if (err != nil) != tc.wantError {
				t.Fatalf("err=%v wantError=%v", err, tc.wantError)
			}
			if err == nil {
				if tc.ownedBase && lease.Server.Labels[exeDevClaimGenerationLabel] == "cbx_111111111111" {
					t.Fatal("clone inherited the base claim generation")
				}
				if err := backend.ReleaseLease(t.Context(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
					t.Fatal(err)
				}
			} else if !tc.readyFailure {
				if connected {
					t.Fatal("connected before clone identity verified")
				}
				if !strings.Contains(err.Error(), "manual cleanup") {
					t.Fatalf("missing recovery diagnostic: %v", err)
				}
			}
			if deleted != tc.wantDelete {
				t.Fatalf("deleted=%v want=%v err=%v", deleted, tc.wantDelete, err)
			}
			if (tc.malformed || (tc.wantError && tc.copyResponse != "") || tc.wrongName) && tagged {
				t.Fatal("tagged after untrusted copy response")
			}
		})
	}
}

func TestCloneBaseRemainsOneLiteralSSHArgument(t *testing.T) {
	cfg := cloneConfig(t, "--exe-dev-from", "base'; touch /tmp/unwanted; echo '")
	runner := &exeDevRecordingRunner{fn: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		want := `cp 'base'\''; touch /tmp/unwanted; echo '\''' clone --copy-tags=false --json --cpu 2 --memory 4GB --disk 10GB`
		if cmd := req.Args[len(req.Args)-1]; cmd != want {
			t.Fatalf("command=%q want=%q", cmd, want)
		}
		return core.LocalCommandResult{ExitCode: 1}, errors.New("fixture copy rejected")
	}}
	_, _, _ = newExeDevTestBackend(cfg, runner).createVM(t.Context(), cfg, "clone", "lease", "slug", "generation")
	if len(runner.calls) != 1 {
		t.Fatalf("calls=%d", len(runner.calls))
	}
}
