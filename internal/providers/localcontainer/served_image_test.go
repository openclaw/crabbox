package localcontainer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type servedRuntimeFunc func(context.Context, core.LocalCommandRequest) (core.LocalCommandResult, error)

func (f servedRuntimeFunc) Run(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
	return f(ctx, req)
}

func TestServedCreateRecoversAfterOtherAgentPrunesPreparedImage(t *testing.T) {
	for _, runtimeName := range []string{"docker", "podman"} {
		t.Run(runtimeName, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var mu sync.Mutex
			imageID := ""
			commits := 0
			var runImages []string
			containers := map[string]string{}
			blocked, release := make(chan struct{}), make(chan struct{})
			tag := servedPreparedTag("sha256:base")
			runner := servedRuntimeFunc(func(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				a := req.Args
				mu.Lock()
				defer mu.Unlock()
				out := ""
				switch a[0] {
				case "image":
					switch a[1] {
					case "ls":
						out = imageID
					case "rm":
						if a[2] != imageID || imageID == "" {
							t.Errorf("unexpected image removal: %v", a)
						}
						imageID = ""
					case "inspect":
						if a[2] == "configured:stable" {
							out = servedImageFixture("sha256:base")
						} else if imageID != "" && (a[2] == tag || a[2] == imageID) {
							out = servedImageFixture(imageID)
						} else {
							message := "No such image: " + a[2]
							if runtimeName == "podman" {
								message = a[2] + ": image not known"
							}
							return core.LocalCommandResult{Stderr: message}, errors.New("exit status 1")
						}
					}
				case "run":
					if a[1] == "--rm" {
						out = "prepare"
					} else if a[1] == "--name" {
						if commits == 1 {
							close(blocked)
							mu.Unlock()
							select {
							case <-release:
							case <-ctx.Done():
							}
							mu.Lock()
							if ctx.Err() != nil {
								return core.LocalCommandResult{}, ctx.Err()
							}
						}
					} else {
						image := a[len(a)-4]
						runImages = append(runImages, image)
						if image != "configured:stable" && image != imageID {
							return core.LocalCommandResult{}, errors.New("container image unavailable")
						}
						out = fmt.Sprintf("container-%d", len(runImages))
						containers[out] = image
					}
				case "commit":
					commits++
					imageID = fmt.Sprintf("sha256:prepared-%d", commits)
				case "inspect":
					out = `[{"Id":"` + a[1] + `","Image":"` + containers[a[1]] + `","State":{"Running":true},"NetworkSettings":{"Ports":{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"22222"}]}}}]`
				case "ps", "rm":
				default:
					t.Errorf("unexpected command %v", a)
				}
				return core.LocalCommandResult{Stdout: out}, nil
			})
			cfg := core.Config{LocalContainer: core.LocalContainerConfig{Runtime: runtimeName, Image: "configured:stable"}}
			rt := core.Runtime{Exec: runner, Stderr: io.Discard}
			// Independent runtime instances represent agents with separate state
			// directories sharing one Docker/Podman image store.
			pruner, idle := NewServedRuntime(cfg, rt), NewServedRuntime(cfg, rt)
			if err := pruner.Prepare(ctx); err != nil {
				t.Fatal(err)
			}
			if err := idle.Prepare(ctx); err != nil {
				t.Fatal(err)
			}
			defer func() { cancel(); idle.WaitPreparation() }()
			if err := pruner.PruneCaches(ctx, nil); err != nil {
				t.Fatal(err)
			}
			create := func(id string) {
				t.Helper()
				if _, err := idle.Create(ctx, id, "ssh-ed25519 AAAA", 1, 1024, cfg.LocalContainer.Image, nil, ServedContainerOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			create("cbx_123456abcdef")
			select {
			case <-blocked:
			case <-time.After(5 * time.Second):
				t.Fatal("missing asynchronous re-preparation")
			}
			create("cbx_123456abcdee")
			close(release)
			idle.WaitPreparation()
			create("cbx_123456abcded")
			mu.Lock()
			defer mu.Unlock()
			want := []string{"sha256:prepared-1", "configured:stable", "configured:stable", "sha256:prepared-2"}
			if !reflect.DeepEqual(runImages, want) || commits != 2 {
				t.Fatalf("run images=%v commits=%d", runImages, commits)
			}
		})
	}
}

func TestServedCreateDoesNotFallbackOnOtherRuntimeErrors(t *testing.T) {
	for _, inspectError := range []string{"", "permission denied"} {
		t.Run(inspectError, func(t *testing.T) {
			runs := 0
			runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
				if req.Args[0] == "run" {
					runs++
					return core.LocalCommandResult{}, errors.New("port already allocated")
				}
				if inspectError != "" {
					return core.LocalCommandResult{Stderr: inspectError}, errors.New("exit status 1")
				}
				return core.LocalCommandResult{Stdout: servedImageFixture("sha256:prepared")}, nil
			}}
			r := NewServedRuntime(core.Config{LocalContainer: core.LocalContainerConfig{Runtime: "docker", Image: "configured:stable"}}, core.Runtime{Exec: runner, Stderr: io.Discard})
			r.preparedImageID = "sha256:prepared"
			_, err := r.Create(t.Context(), "cbx_123456abcdef", "ssh-ed25519 AAAA", 1, 1024, r.Image(), nil, ServedContainerOptions{})
			if err == nil || runs != 1 || r.preparedImageID != "sha256:prepared" {
				t.Fatalf("err=%v runs=%d prepared=%s", err, runs, r.preparedImageID)
			}
		})
	}
}

func servedImageFixture(id string) string {
	return `[{"Id":"` + id + `","Config":{"Cmd":["default-command"],"Entrypoint":["entrypoint"],"User":"1000","Env":["FIXTURE=value"]}}]`
}

func TestServedPrepare(t *testing.T) {
	for _, runtimeName := range []string{"docker", "podman"} {
		for _, mode := range []string{"ready", "skip", "prepare", "reuse", "pull", "base-pull-failure", "base-inspect-failure", "probe-failure", "probe-output", "run-failure", "commit-failure", "inspect-failure", "config-mismatch", "cleanup-failure", "tag-cleanup-failure", "cancel", "nil-cmd", "empty-cmd", "nil-entrypoint", "empty-entrypoint"} {
			t.Run(runtimeName+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var warning strings.Builder
				fixture := func(id string) string {
					result := servedImageFixture(id)
					for _, field := range []string{"cmd", "entrypoint"} {
						value, name := `["default-command"]`, "Cmd"
						if field == "entrypoint" {
							value, name = `["entrypoint"]`, "Entrypoint"
						}
						if mode == "nil-"+field {
							result = strings.Replace(result, `"`+name+`":`+value, `"`+name+`":null`, 1)
						}
						if mode == "empty-"+field {
							result = strings.Replace(result, `"`+name+`":`+value, `"`+name+`":[]`, 1)
						}
					}
					return result
				}
				tag := servedPreparedTag("sha256:base")
				baseInspects, tagInspects, prepRuns, commits, cleanups, tagRemovals := 0, 0, 0, 0, 0, 0
				var temp, probe string
				runner := &recordingRunner{runContext: func(ctx context.Context, req core.LocalCommandRequest) (core.LocalCommandResult, error) {
					if req.Name != runtimeName {
						t.Fatalf("runtime=%s", req.Name)
					}
					a := req.Args
					output := ""
					switch a[0] {
					case "image":
						if a[1] == "rm" {
							tagRemovals++
							if ctx.Err() != nil || a[2] != tag {
								t.Fatal("tag cleanup", a, ctx.Err())
							}
							if mode == "tag-cleanup-failure" {
								return core.LocalCommandResult{}, errors.New("tag cleanup failed")
							}
							break
						}
						if a[2] == "configured:stable" {
							baseInspects++
							if strings.HasPrefix(mode, "base-") || mode == "pull" && baseInspects == 1 {
								return core.LocalCommandResult{}, errors.New("missing base")
							}
							output = fixture("sha256:base")
						} else if a[2] == tag {
							tagInspects++
							if mode != "reuse" && tagInspects == 1 {
								return core.LocalCommandResult{}, errors.New("missing tag")
							}
							if mode == "inspect-failure" {
								return core.LocalCommandResult{}, errors.New("inspect failed")
							}
							output = fixture("sha256:prepared")
							if mode == "config-mismatch" || mode == "tag-cleanup-failure" {
								output = strings.ReplaceAll(output, "default-command", "changed-command")
							}
						} else {
							t.Fatalf("unexpected inspect %v", a)
						}
					case "pull":
						if a[1] != "configured:stable" || req.Stdout != &warning || req.Stderr != &warning {
							t.Fatal(a)
						}
						if mode == "base-pull-failure" {
							return core.LocalCommandResult{}, errors.New("pull failed")
						}
					case "run":
						if a[1] == "--rm" {
							probe = a[3]
							output = "prepare"
							if mode == "ready" || mode == "skip" {
								output = mode
							}
							if mode == "probe-output" {
								output = "unexpected"
							}
							if !strings.Contains(a[len(a)-1], bootstrapTools) {
								t.Fatal("missing tool probe")
							}
							if mode == "probe-failure" {
								return core.LocalCommandResult{}, errors.New("probe failed")
							}
						} else {
							prepRuns++
							temp = a[2]
							if !strings.Contains(a[len(a)-1], bootstrapPackages) || !strings.Contains(a[len(a)-1], "rm -f /etc/ssh/ssh_host_*") || !strings.Contains(a[len(a)-1], "/var/lib/apt/lists/*") {
								t.Fatal("invalid preparation script")
							}
							if req.Stdout != &warning || req.Stderr != &warning {
								t.Fatal("preparation output not directed to stderr")
							}
							if mode == "cancel" {
								cancel()
								return core.LocalCommandResult{}, context.Canceled
							}
							if mode == "run-failure" {
								return core.LocalCommandResult{}, errors.New("run failed")
							}
						}
					case "commit":
						commits++
						wantCmd, wantEntrypoint := `CMD ["default-command"]`, `ENTRYPOINT ["entrypoint"]`
						if strings.HasSuffix(mode, "-cmd") {
							wantCmd = "CMD []"
						}
						if strings.HasSuffix(mode, "-entrypoint") {
							wantEntrypoint = "ENTRYPOINT []"
						}
						joined := strings.Join(a, "\n")
						for _, want := range []string{wantCmd, wantEntrypoint, "USER 1000", "crabbox.serve.prepared=true", `crabbox.serve.base-reference="configured:stable"`, `crabbox.serve.base-image-id="sha256:base"`, tag, temp} {
							if !strings.Contains(joined, want) {
								t.Fatalf("missing %s in %v", want, a)
							}
						}
						if mode == "commit-failure" {
							return core.LocalCommandResult{}, errors.New("commit failed")
						}
					case "rm":
						cleanups++
						expected := temp
						if mode == "probe-failure" {
							expected = probe
						}
						if ctx.Err() != nil || !reflect.DeepEqual(a, []string{"rm", "-f", expected}) {
							t.Fatal("cleanup", a, ctx.Err())
						}
						if mode == "cleanup-failure" {
							return core.LocalCommandResult{}, errors.New("cleanup failed")
						}
					default:
						t.Fatalf("unexpected %v", a)
					}
					return core.LocalCommandResult{Stdout: output}, nil
				}}
				r := NewServedRuntime(core.Config{LocalContainer: core.LocalContainerConfig{Runtime: runtimeName, Image: "configured:stable"}}, core.Runtime{Exec: runner, Stderr: &warning})
				r.preparedImageID = "stale-prepared"
				r.baseImageID = "stale-base"
				err := r.Prepare(ctx)
				fatal := strings.HasPrefix(mode, "base-")
				fallback := !fatal && (strings.HasSuffix(mode, "failure") || mode == "probe-output" || mode == "cancel" || mode == "config-mismatch")
				if (err != nil) != fatal {
					t.Fatalf("error=%v", err)
				}
				wantWarnings := 0
				if fallback {
					wantWarnings = 1
				}
				if got := strings.Count(warning.String(), "warning: serve image preparation failed; leases will bootstrap at start:"); got != wantWarnings || strings.Count(warning.String(), "\n") != wantWarnings {
					t.Fatalf("warnings=%q", warning.String())
				}
				built := !fatal && mode != "ready" && mode != "skip" && mode != "reuse" && mode != "probe-failure" && mode != "probe-output"
				expectedCleanups := 0
				if built || mode == "probe-failure" {
					expectedCleanups = 1
				}
				if cleanups != expectedCleanups || built && prepRuns != 1 || !built && (prepRuns != 0 || commits != 0) {
					t.Fatalf("runs=%d commits=%d cleanups=%d", prepRuns, commits, cleanups)
				}
				expectedRemovals := 0
				if fallback && commits > 0 {
					expectedRemovals = 1
				}
				if tagRemovals != expectedRemovals {
					t.Fatalf("tag removals=%d want=%d", tagRemovals, expectedRemovals)
				}
				wantID := ""
				if !fatal && !fallback && mode != "ready" && mode != "skip" {
					wantID = "sha256:prepared"
				}
				wantBase := "sha256:base"
				if fatal {
					wantBase = ""
				}
				if r.Image() != "configured:stable" || r.preparedImageID != wantID || r.baseImageID != wantBase {
					t.Fatalf("runtime=%+v", r)
				}
			})
		}
	}
}

func TestServedPreparedCreateAndEvidence(t *testing.T) {
	for _, mode := range []string{"configured", "prepared", "spoofed", "previous-prepared"} {
		for _, digestStatus := range []string{"available", "unavailable", "unknown"} {
			t.Run(mode+"/"+digestStatus, func(t *testing.T) {
				prepared := mode == "prepared" || mode == "previous-prepared"
				runtimeID := "sha256:configured"
				if prepared {
					runtimeID = "sha256:prepared"
				}
				wantDigestImage := runtimeID
				if mode == "prepared" {
					wantDigestImage = "sha256:base"
				}
				digest := "example.invalid/base@sha256:" + strings.Repeat("a", 64)
				runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
					a := req.Args
					switch a[0] {
					case "run":
						wantImage := "configured:stable"
						if prepared {
							wantImage = runtimeID
						}
						if a[len(a)-4] != wantImage {
							t.Fatalf("run image=%s want=%s", a[len(a)-4], wantImage)
						}
						if labelFromRunArgs(t, a, "image") != "configured:stable" {
							t.Fatal("changed configured label")
						}
						return core.LocalCommandResult{Stdout: "container"}, nil
					case "inspect":
						labels := `"image":"configured:stable","crabbox.serve.prepared":"true","crabbox.serve.base-image-id":"sha256:spoofed"`
						return core.LocalCommandResult{Stdout: `[{"Id":"container","Image":"` + runtimeID + `","State":{"Running":true},"Config":{"Image":"` + runtimeID + `","Labels":{` + labels + `}},"NetworkSettings":{"Ports":{"2222/tcp":[{"HostIp":"127.0.0.1","HostPort":"22222"}]}}}]`}, nil
					case "image":
						if a[2] != wantDigestImage {
							t.Fatalf("digest image=%s want=%s", a[2], wantDigestImage)
						}
						if digestStatus == "unknown" {
							return core.LocalCommandResult{}, errors.New("unavailable")
						}
						digests := []string{}
						if digestStatus == "available" {
							digests = append(digests, digest)
						}
						data, _ := json.Marshal(digests)
						return core.LocalCommandResult{Stdout: string(data)}, nil
					default:
						t.Fatal(a)
					}
					return core.LocalCommandResult{}, nil
				}}
				r := NewServedRuntime(core.Config{LocalContainer: core.LocalContainerConfig{Runtime: "docker", Image: "configured:stable"}}, core.Runtime{Exec: runner, Stderr: io.Discard})
				r.baseImageID = "sha256:base"
				if prepared {
					r.preparedImageID = runtimeID
				}
				c, err := r.Create(t.Context(), "cbx_123456abcdef", "ssh-ed25519 AAAA", 1, 1024, "configured:stable", nil, ServedContainerOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if mode == "previous-prepared" || mode == "spoofed" {
					r.preparedImageID = "sha256:another-prepared"
				}
				evidence, err := r.Evidence(t.Context(), c)
				if err != nil || evidence.ConfiguredReference != "configured:stable" || evidence.RuntimeImageID != runtimeID || evidence.RepositoryDigestStatus != digestStatus {
					t.Fatalf("evidence=%+v error=%v", evidence, err)
				}
				if digestStatus == "available" && !reflect.DeepEqual(evidence.RepositoryDigests, []string{digest}) {
					t.Fatal(evidence)
				}
			})
		}
	}
}

func TestServedPruneSkipsRunningPreparedImages(t *testing.T) {
	runner := &recordingRunner{responses: map[string]core.LocalCommandResult{
		commandKey([]string{"image", "ls", "--filter", "label=crabbox.serve.prepared=true", "--format", "{{.ID}}", "--no-trunc"}): {Stdout: "sha256:unused\nsha256:running\n"},
		commandKey([]string{"ps", "-q", "--filter", "ancestor=sha256:running"}):                                                   {Stdout: "container\n"},
	}}
	r := NewServedRuntime(core.Config{LocalContainer: core.LocalContainerConfig{Runtime: "docker"}}, core.Runtime{Exec: runner})
	if err := r.PruneCaches(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, call := range runner.calls {
		if len(call.Args) > 2 && call.Args[0] == "image" && call.Args[1] == "rm" {
			removed = append(removed, call.Args[2])
		}
	}
	if !reflect.DeepEqual(removed, []string{"sha256:unused"}) {
		t.Fatal(removed)
	}
}

func TestServedPrunesImagesEvenWhenCacheVolumeRemovalFails(t *testing.T) {
	volumeErr := errors.New("volume is absent or in use")
	runner := &recordingRunner{run: func(req core.LocalCommandRequest) (core.LocalCommandResult, error) {
		if req.Args[0] == "volume" {
			return core.LocalCommandResult{}, volumeErr
		}
		return core.LocalCommandResult{}, nil
	}}
	r := NewServedRuntime(core.Config{LocalContainer: core.LocalContainerConfig{Runtime: "docker"}}, core.Runtime{Exec: runner})
	if err := r.PruneCaches(t.Context(), []core.CacheVolumeConfig{{Key: "cache", Path: "/cache"}}); !errors.Is(err, volumeErr) {
		t.Fatalf("error=%v", err)
	}
	if got := runner.calls[len(runner.calls)-1].Args; len(got) < 2 || got[0] != "image" || got[1] != "ls" {
		t.Fatalf("prepared images were not considered: %v", got)
	}
}
