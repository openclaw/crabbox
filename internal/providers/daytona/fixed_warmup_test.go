package daytona

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestDaytonaFixedWarmupReachesProviderAdmission(t *testing.T) {
	testutil.IsolateUserDirs(t)
	repo := t.TempDir()
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	t.Chdir(repo)
	for _, name := range []string{"CRABBOX_COORDINATOR", "CRABBOX_COORDINATOR_MODE", "CRABBOX_COORDINATOR_TOKEN_COMMAND", "CRABBOX_POND", "CRABBOX_TAILSCALE", "CRABBOX_DAYTONA_JWT_TOKEN", "DAYTONA_JWT_TOKEN", "CRABBOX_DAYTONA_ORGANIZATION_ID", "DAYTONA_ORGANIZATION_ID"} {
		t.Setenv(name, "")
	}
	t.Setenv("CRABBOX_DAYTONA_API_KEY", "synthetic-fixed-admission")
	var reads, mutations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			reads.Add(1)
		} else {
			mutations.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"synthetic admission denied"}`)
	}))
	t.Cleanup(server.Close)
	t.Setenv("CRABBOX_DAYTONA_API_URL", server.URL)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("provider: daytona\ntarget: linux\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_CONFIG", configPath)
	err := (core.App{Stdout: io.Discard, Stderr: io.Discard}).Run(t.Context(), []string{
		"warmup", "--provider", "daytona", "--network", "public",
		"--lease-id", "cbx_123456abcdef", "--daytona-snapshot", "synthetic-snapshot",
	})
	if err == nil || reads.Load() == 0 || mutations.Load() != 0 {
		t.Fatalf("fixed warmup must reach provider admission and stop before allocation: reads=%d mutations=%d err=%v", reads.Load(), mutations.Load(), err)
	}
}
