package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/openclaw/crabbox/internal/runner/runnerfs"
	"github.com/openclaw/crabbox/internal/runner/runnerwire"
)

func recoveryFixture(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("recovery requires POSIX")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	if err := os.MkdirAll(filepath.Join(root, "Library", "Caches"), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "destination")
	// Inert test-owned sidecars exercise protocol plumbing; the runnerfs tests
	// separately own proof against the genuine historical writer fixture.
	for name, contents := range map[string]string{target: "current", target + ".crabbox-cp-backup": "previous", target + ".crabbox-cp-transaction": "retained recovery information"} {
		if err := os.WriteFile(name, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return target
}

func TestClientRecoverArchiveExactDestination(t *testing.T) {
	for _, choice := range []runnerfs.ArchiveRecoveryChoice{runnerfs.ArchiveKeepDestination, runnerfs.ArchiveRestoreBackup} {
		t.Run(string(choice), func(t *testing.T) {
			target := recoveryFixture(t)
			identity := CurrentIdentity()
			client := &Client{Identity: identity, Transport: func(ctx context.Context, input io.Reader, output io.Writer) error {
				return Serve(ctx, input, output, identity)
			}}
			retained, err := client.RecoverArchive(context.Background(), "~/destination", choice)
			if err != nil {
				t.Fatal(err)
			}
			want, saved, retainedName := "current", "previous", "legacy-backup"
			if choice == runnerfs.ArchiveRestoreBackup {
				want, saved, retainedName = "previous", "current", "legacy-destination"
			}
			for name, contents := range map[string]string{target: want, filepath.Join(retained, retainedName): saved} {
				data, err := os.ReadFile(name)
				if err != nil || string(data) != contents {
					t.Fatalf("%s = %q, %v; want %q", name, data, err, contents)
				}
			}
		})
	}
}

func TestRecoveryRequestRequiresCompleteStream(t *testing.T) {
	target := recoveryFixture(t)
	var request, response bytes.Buffer
	if err := writeFrame(&request, runnerwire.Request, Request{BuildID: BuildID, Operation: RecoverArchive, Destination: target, RecoveryChoice: runnerfs.ArchiveRestoreBackup}, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := Serve(context.Background(), &request, &response, CurrentIdentity()); err == nil {
		t.Fatal("incomplete request accepted")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "current" {
		t.Fatalf("destination changed: %q, %v", data, err)
	}
	if _, err := os.Stat(target + ".crabbox-cp-backup"); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryClientRequiresTransportSuccess(t *testing.T) {
	identity := CurrentIdentity()
	failed := errors.New("transport exit failed")
	client := &Client{Identity: identity, Transport: func(_ context.Context, input io.Reader, output io.Writer) error {
		if _, err := io.Copy(io.Discard, input); err != nil {
			return err
		}
		if err := writeFrame(output, runnerwire.Hello, identity, 0, nil); err != nil {
			return err
		}
		if err := writeFrame(output, runnerwire.Result, Outcome{RetainedPath: "/remote/retained"}, 0, nil); err != nil {
			return err
		}
		if err := writeFrame(output, runnerwire.End, nil, 0, nil); err != nil {
			return err
		}
		return failed
	}}
	retained, err := client.RecoverArchive(context.Background(), "/remote/destination", runnerfs.ArchiveKeepDestination)
	if !errors.Is(err, failed) || retained != "" {
		t.Fatalf("reported recovery success: %q, %v", retained, err)
	}
}

func TestRecoveryInvalidChoiceLeavesDestination(t *testing.T) {
	target := recoveryFixture(t)
	var request, response bytes.Buffer
	if err := writeFrame(&request, runnerwire.Request, Request{BuildID: BuildID, Operation: RecoverArchive, Destination: target}, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(&request, runnerwire.End, nil, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := Serve(context.Background(), &request, &response, CurrentIdentity()); err == nil {
		t.Fatal("missing choice accepted")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "current" {
		t.Fatalf("destination changed: %q, %v", data, err)
	}
}

func TestUploadRecoveryRequiredCarriesExactTarget(t *testing.T) {
	target := recoveryFixture(t)
	source := filepath.Join(filepath.Dir(target), "source")
	if err := os.WriteFile(source, []byte("incoming"), 0600); err != nil {
		t.Fatal(err)
	}
	identity := CurrentIdentity()
	client := &Client{Identity: identity, Transport: func(ctx context.Context, input io.Reader, output io.Writer) error {
		return Serve(ctx, input, output, identity)
	}}
	err := client.Upload(context.Background(), source, target, false)
	var remote RemoteError
	if !errors.As(err, &remote) || remote.Code != "recovery-required" || remote.Target != target {
		t.Fatalf("missing recovery target: %#v, %v", remote, err)
	}
}
