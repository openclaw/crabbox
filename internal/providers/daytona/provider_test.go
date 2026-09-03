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
