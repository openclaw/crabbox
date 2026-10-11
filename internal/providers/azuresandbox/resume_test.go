package azuresandbox

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestSandboxIdleLeaseResumesForSupportedOperations(t *testing.T) {
	for _, operation := range []string{"warmup", "run", "copy", "heartbeat"} {
		t.Run(operation, func(t *testing.T) {
			f := &fixture{}
			b, req := fixtureBackend(t, f)
			if _, err := b.acquire(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			f.box.State = "Stopped"
			f.box.StateDetails.StoppedReason = "Idle"
			resumes, commands, uploads := 0, 0, 0
			b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/resume"):
					if r.Method != http.MethodPost {
						t.Fatal("resume did not use POST")
					}
					resumes++
					f.box.State = "Running"
					return response(204, ""), nil
				case strings.HasSuffix(r.URL.Path, "/executeShellCommand"):
					commands++
					return response(200, `{"exitCode":0}`), nil
				case strings.HasSuffix(r.URL.Path, "/files"):
					uploads++
					return response(204, ""), nil
				default:
					return f.request(t, r)
				}
			})
			var err error
			switch operation {
			case "warmup":
				_, err = b.acquire(t.Context(), req)
			case "run":
				_, err = b.Run(t.Context(), core.RunRequest{ID: req.RequestedLeaseID, Repo: req.Repo, Keep: true, NoSync: true, Command: []string{"true"}})
			case "copy":
				file := filepath.Join(t.TempDir(), "input")
				if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				err = b.Copy(t.Context(), core.CopyRequest{ID: req.RequestedLeaseID, RepoRoot: req.Repo.Root, Source: file, Destination: "SANDBOX:/tmp/input"})
			case "heartbeat":
				_, err = b.Heartbeat(t.Context(), core.LeaseHeartbeatRequest{ID: req.RequestedLeaseID})
			}
			if err != nil || resumes != 1 || f.creates != 1 || f.deletes != 0 {
				t.Fatalf("err=%v resumes=%d creates=%d deletes=%d", err, resumes, f.creates, f.deletes)
			}
			if (operation == "run" || operation == "heartbeat") && commands != 1 {
				t.Fatal("resumed lease did not execute")
			}
			if operation == "copy" && uploads != 1 {
				t.Fatal("resumed lease did not upload")
			}
		})
	}
}

func TestSandboxResumePreservesOwnershipAndDisabledGuards(t *testing.T) {
	for _, scenario := range []string{"disabled", "foreign-before", "foreign-after", "unknown-resume", "cancelled-wait"} {
		t.Run(scenario, func(t *testing.T) {
			f := &fixture{}
			b, req := fixtureBackend(t, f)
			if _, err := b.acquire(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			f.box.State = "Suspended"
			if scenario == "disabled" {
				f.box.StateDetails.StoppedReason = "Disabled"
			}
			if scenario == "foreign-before" {
				f.box.Labels["crabbox_attempt"] = "foreign"
			}
			resumes, commands := 0, 0
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			b.api.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/resume") {
					resumes++
					if scenario == "unknown-resume" {
						return response(500, "unknown"), nil
					}
					if scenario != "cancelled-wait" {
						f.box.State = "Running"
					} else {
						// Cancel only after admission, rather than racing claim I/O
						// against an arbitrary wall-clock deadline.
						cancel()
					}
					if scenario == "foreign-after" {
						f.box.Labels["crabbox_attempt"] = "foreign"
					}
					return response(204, ""), nil
				}
				if strings.HasSuffix(r.URL.Path, "/executeShellCommand") {
					commands++
				}
				return f.request(t, r)
			})
			_, err := b.Run(ctx, core.RunRequest{ID: req.RequestedLeaseID, Repo: req.Repo, Keep: true, NoSync: true, Command: []string{"true"}})
			if err == nil || commands != 0 || f.deletes != 0 {
				t.Fatalf("err=%v commands=%d deletes=%d", err, commands, f.deletes)
			}
			want := 1
			if scenario == "disabled" || scenario == "foreign-before" {
				want = 0
			}
			if resumes != want {
				t.Fatalf("resume count %d, want %d", resumes, want)
			}
			claim, err := b.claim(req.RequestedLeaseID)
			if err != nil || claim.FixedCreateIntent.State != "acquired" {
				t.Fatalf("claim lost: %+v %v", claim, err)
			}
		})
	}
}
