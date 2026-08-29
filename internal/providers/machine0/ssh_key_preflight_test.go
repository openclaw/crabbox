package machine0

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestDoctorRejectsMissingSSHKeyPrerequisites(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  *machineKey
	}{
		{name: "no default"},
		{name: "PUBLIC default", key: &machineKey{Name: "public-key", Type: "PUBLIC", FileName: "missing-key"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupState(t)
			api := &fakeAPI{selectedKey: tc.key, noDefaultKey: tc.key == nil, sizes: []machineSize{testSize()}}
			_, err := testBackendWithAPI(api).Doctor(context.Background(), DoctorRequest{})
			if err == nil || !strings.Contains(err.Error(), "key") {
				t.Fatalf("doctor accepted missing SSH prerequisites: %v", err)
			}
			if len(api.created) != 0 || len(api.primed) != 0 || len(api.removed) != 0 {
				t.Fatal("doctor mutated provider")
			}
		})
	}
}

func TestLegacySSHKeyPrerequisitesBeforeDoctorAndCreate(t *testing.T) {
	for _, tc := range []struct {
		name, missing, directory string
		statError                bool
	}{
		{name: "both missing", missing: "both"},
		{name: "private missing", missing: "id_rsa"},
		{name: "public missing", missing: "id_rsa.pub"},
		{name: "private directory", directory: "id_rsa"},
		{name: "public directory", directory: "id_rsa.pub"},
		{name: "stat permission error", statError: true},
	} {
		for _, mode := range []string{"doctor", "ordinary", "fixed"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				repo := setupState(t)
				root := os.Getenv("SSH_KEY_PATH")
				for _, name := range []string{"id_rsa", "id_rsa.pub"} {
					if tc.missing == "both" || tc.missing == name {
						continue
					}
					if tc.directory == name {
						if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
							t.Fatal(err)
						}
					} else if err := os.WriteFile(filepath.Join(root, name), []byte("synthetic key material\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				api := &fakeAPI{noDefaultKey: true, sizes: []machineSize{testSize()}}
				b := testBackendWithAPI(api)
				if tc.statError {
					b.stat = func(string) (os.FileInfo, error) { return nil, os.ErrPermission }
				}
				var err error
				if mode == "doctor" {
					_, err = b.Doctor(context.Background(), DoctorRequest{})
				} else {
					req := AcquireRequest{Repo: core.Repo{Root: repo}}
					if mode == "fixed" {
						req.RequestedLeaseID = fixedMachine0TestLeaseID
					}
					_, err = b.Acquire(context.Background(), req)
				}
				if err == nil || !strings.Contains(err.Error(), "no default SSH key") || !strings.Contains(err.Error(), "--machine0-key") {
					t.Fatalf("missing actionable prerequisite failure: %v", err)
				}
				if len(api.created) != 0 || len(api.primed) != 0 || len(api.removed) != 0 {
					t.Fatal("missing prerequisite reached a provider mutation")
				}
				if mode == "fixed" {
					claim := readFixedMachine0Claim(t, fixedMachine0TestLeaseID)
					if claim.CloudID != "" || len(claim.FixedCreateIntent.Attempt) != 0 {
						t.Fatal("missing prerequisite persisted a creation attempt")
					}
				}
			})
		}
	}
}

func TestDoctorLegacyPairIsOnlyAPrerequisite(t *testing.T) {
	for _, homeFallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "SSH_KEY_PATH", true: "home fallback"}[homeFallback], func(t *testing.T) {
			setupState(t)
			root := os.Getenv("SSH_KEY_PATH")
			if homeFallback {
				t.Setenv("USERPROFILE", os.Getenv("HOME"))
				t.Setenv("SSH_KEY_PATH", "")
			}
			const material = "not an authentication proof\n"
			for _, name := range []string{"id_rsa", "id_rsa.pub"} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(material), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			api := &fakeAPI{noDefaultKey: true, sizes: []machineSize{testSize()}}
			result, err := testBackendWithAPI(api).Doctor(context.Background(), DoctorRequest{})
			if err != nil || !strings.Contains(result.Message, "ssh_key_prerequisites=checked") || !strings.Contains(result.Message, "runtime=unchecked") {
				t.Fatalf("doctor result=%#v err=%v", result, err)
			}
			for _, name := range []string{"id_rsa", "id_rsa.pub"} {
				if data, err := os.ReadFile(filepath.Join(root, name)); err != nil || string(data) != material {
					t.Fatalf("doctor changed %s: %v", name, err)
				}
			}
			if len(api.created) != 0 || len(api.primed) != 0 {
				t.Fatal("doctor materialized a key or VM")
			}
		})
	}
}

func TestDoctorKeyFailureCancelsSiblingProbes(t *testing.T) {
	want := errors.New("key selection unavailable")
	api := &fakeAPI{selectedKeyErr: want, doctorDelay: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := testBackendWithAPI(api).Doctor(ctx, DoctorRequest{})
	if !errors.Is(err, want) || ctx.Err() != nil {
		t.Fatalf("doctor did not cancel siblings after key failure: err=%v ctx=%v", err, ctx.Err())
	}
}

func TestFixedReplayDoesNotRecheckCurrentDefaultKey(t *testing.T) {
	b, api, req := fixedMachine0TestFixture(t)
	first, err := b.Acquire(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	api.selectedKeyErr = errors.New("current default unavailable")
	replayed, err := b.Acquire(context.Background(), req)
	if err != nil || replayed.Server.CloudID != first.Server.CloudID || len(api.created) != 1 {
		t.Fatalf("existing replay consulted new-create prerequisites: err=%v creates=%d", err, len(api.created))
	}
}
