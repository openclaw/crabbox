package daytona

import (
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

func TestDaytonaClassScopeUsesConfigProvenance(t *testing.T) {
	for _, source := range []string{"inherited", "yaml", "environment"} {
		t.Run(source, func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			data := "provider: daytona\n"
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
			cfg, err := core.LoadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if core.ClassWasExplicit(cfg) != (source != "inherited") {
				t.Fatal("class provenance not preserved")
			}
			err = (&daytonaLeaseBackend{cfg: cfg}).ValidateCoordinatorAcquire()
			if (err != nil) != (source != "inherited") || err != nil && !strings.Contains(err.Error(), "requires direct mode") {
				t.Fatalf("source=%s err=%v", source, err)
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
