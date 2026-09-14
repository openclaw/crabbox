package daytona

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	api "github.com/daytonaio/daytona/libs/api-client-go"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestProviderSupportsCoordinator(t *testing.T) {
	spec := (Provider{}).Spec()
	if spec.Name != "daytona" || spec.Kind != core.ProviderKindSSHLease || spec.Coordinator != core.CoordinatorSupported {
		t.Fatalf("spec=%#v", spec)
	}
	for _, feature := range []core.Feature{core.FeatureSSH, core.FeatureCrabboxSync, core.FeatureArchiveSync, core.FeatureCheckpoint, core.FeatureFork, core.FeatureSnapshot} {
		if !spec.Features.Has(feature) {
			t.Fatalf("features=%v missing %s", spec.Features, feature)
		}
	}
	for _, unsupported := range []core.Feature{
		core.FeatureDesktop,
		core.FeatureBrowser,
		core.FeatureCode,
		core.FeatureTailscale,
		core.FeatureRestore,
	} {
		if spec.Features.Has(unsupported) {
			t.Fatalf("features=%v unexpectedly includes %s", spec.Features, unsupported)
		}
	}
}

func TestDaytonaBrokerPreservesConfiguredClasses(t *testing.T) {
	for _, source := range []string{"yaml", "environment", "flag"} {
		t.Run(source, func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			var creates atomic.Int32
			broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1/leases" {
					t.Errorf("unexpected broker operation %s %s", r.Method, r.URL.Path)
				}
				creates.Add(1)
				var request struct {
					Class, ServerType string
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if request.Class != "standard" || request.ServerType != "snapshot" {
					t.Errorf("broker sizing changed: class=%q serverType=%q", request.Class, request.ServerType)
				}
				// Stop at the real request boundary without creating a lease or SSH target.
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"synthetic allocation boundary"}`)
			}))
			defer broker.Close()
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			data := fmt.Sprintf("provider: daytona\nnetwork: public\ncoordinator: %s\ncoordinatorToken: fixture-broker-token\n", broker.URL)
			if source == "yaml" {
				data += "class: standard\n"
			}
			if err := os.WriteFile(configPath, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CRABBOX_CONFIG", configPath)
			t.Setenv("CRABBOX_DEFAULT_CLASS", "")
			if source == "environment" {
				t.Setenv("CRABBOX_DEFAULT_CLASS", "standard")
			}
			args := []string{"warmup", "--provider", "daytona", "--slug", "configured-class"}
			if source == "flag" {
				args = append(args, "--class", "standard")
			}
			err := (core.App{Stdout: io.Discard, Stderr: io.Discard}).Run(t.Context(), args)
			if source == "flag" {
				if err == nil || !strings.Contains(err.Error(), "requires direct mode") || creates.Load() != 0 {
					t.Fatalf("explicit broker class: creates=%d err=%v", creates.Load(), err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "synthetic allocation boundary") || creates.Load() != 1 {
				t.Fatalf("configured broker class: creates=%d err=%v", creates.Load(), err)
			}
		})
	}
}

func TestDaytonaDirectPreservesConfiguredSnapshots(t *testing.T) {
	for _, class := range []string{"standard", "custom-ci", "STANDARD", " standard "} {
		for _, source := range []string{"yaml", "environment", "flag"} {
			t.Run(source+"/"+class, func(t *testing.T) {
				f, b, repo := newDaytonaLifecycleFixture(t)
				f.classSnapshot = &api.SnapshotDto{Id: "test-snapshot", Name: "custom-off-tier", State: api.SNAPSHOTSTATE_ACTIVE, Cpu: 1, Mem: 1, Disk: 3, RegionIds: []string{"us"}, Entrypoint: []string{}}
				f.classSnapshot.SetSandboxClass("container")
				config := fmt.Sprintf("provider: daytona\nnetwork: public\ndaytona:\n  apiUrl: %s\n  snapshot: test-snapshot\n", f.server.URL)
				if source == "yaml" {
					config += fmt.Sprintf("class: %q\n", class)
				} else if source == "environment" {
					t.Setenv("CRABBOX_DEFAULT_CLASS", class)
				}
				configPath := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("CRABBOX_CONFIG", configPath)
				t.Setenv("CRABBOX_DAYTONA_API_KEY", b.cfg.Daytona.APIKey)
				if source == "flag" {
					err := (core.App{Stdout: io.Discard, Stderr: io.Discard}).Run(t.Context(), []string{"warmup", "--provider", "daytona", "--class", class})
					wantError, reads := "no class profile", 0
					if class == "standard" {
						wantError, reads = "must be active container", 1
					}
					if err == nil || !strings.Contains(err.Error(), wantError) || len(f.paths) != reads || f.sandboxCreates != 0 {
						t.Fatalf("CLI class must constrain the selected snapshot: paths=%v err=%v", f.paths, err)
					}
					return
				}
				cfg, err := core.LoadConfig()
				if err != nil {
					t.Fatal(err)
				}
				b.cfg = cfg
				if _, _, _, err := b.createDaytonaSandbox(t.Context(), repo, true, false, ""); err != nil {
					t.Fatal(err)
				}
				if f.sandboxCreates != 1 || f.create.GetSnapshot() != "test-snapshot" || f.create.GetLabels()["server_type"] != "snapshot" {
					t.Fatalf("legacy selection changed: creates=%d snapshot=%q type=%q", f.sandboxCreates, f.create.GetSnapshot(), f.create.GetLabels()["server_type"])
				}
				for _, request := range f.paths {
					if strings.Contains(request, "/snapshots/") || strings.Contains(request, "/regions") {
						t.Fatal("configured class unexpectedly replaced the selected snapshot")
					}
				}
				b.cfg.Daytona.Snapshot = ""
				if class == "standard" {
					f.classSnapshot.Name, f.classSnapshot.Cpu, f.classSnapshot.Mem, f.classSnapshot.Disk = "daytona-medium", 2, 4, 8
					if _, _, _, err := b.createDaytonaSandbox(t.Context(), repo, true, false, ""); err != nil || f.sandboxCreates != 2 || f.create.GetSnapshot() != "test-snapshot" || f.create.GetLabels()["server_type"] != "test-snapshot" {
						t.Fatalf("configured tier lost resolved selection: creates=%d type=%q err=%v", f.sandboxCreates, f.create.GetLabels()["server_type"], err)
					}
					return
				}
				if _, _, _, err := b.createDaytonaSandbox(t.Context(), repo, true, false, ""); err == nil || !strings.Contains(err.Error(), "requires --daytona-snapshot") || f.sandboxCreates != 1 {
					t.Fatalf("legacy path must still require a snapshot before allocation: creates=%d err=%v", f.sandboxCreates, err)
				}
			})
		}
	}
}

func TestDaytonaClassDoesNotBlockBrokerInspection(t *testing.T) {
	testutil.IsolateUserDirs(t)
	var reads atomic.Int32
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/leases/cbx_0123456789ab" {
			t.Errorf("unexpected broker operation %s %s", r.Method, r.URL.Path)
		}
		reads.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"lease":{"id":"cbx_0123456789ab","provider":"daytona","state":"released","targetOS":"linux"}}`)
	}))
	defer broker.Close()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(fmt.Sprintf("provider: daytona\nclass: standard\nnetwork: public\ncoordinator: %s\ncoordinatorToken: fixture-broker-token\n", broker.URL)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	if err := (core.App{Stdout: io.Discard, Stderr: io.Discard}).Run(t.Context(), []string{"status", "--provider", "daytona", "--id", "cbx_0123456789ab", "--json"}); err != nil || reads.Load() != 1 {
		t.Fatalf("class must not block existing-lease inspection: reads=%d err=%v", reads.Load(), err)
	}
}
