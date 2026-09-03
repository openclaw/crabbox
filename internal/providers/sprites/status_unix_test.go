//go:build !windows

package sprites

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestSpritesStatusOnlyProbesSSHWhenRequested(t *testing.T) {
	testutil.IsolateUserDirs(t)
	bin := t.TempDir()
	probe := filepath.Join(bin, "probed")
	// -G only parses the isolated SSH config; other calls record a fake probe.
	script := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = -G ]; then exec /usr/bin/ssh "$@"; fi
done
test "$SPRITE_TOKEN" = test-token || exit 1
test "$SPRITE_URL" = https://api.sprites.dev || exit 1
printf 'probed' > "$CRABBOX_TEST_STATUS_PROBE"
`
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_TEST_STATUS_PROBE", probe)
	t.Setenv("SPRITE_TOKEN", "ambient-token")
	t.Setenv("SPRITE_URL", "https://ambient.example")
	cfg := Config{Provider: spritesProvider, Sprites: SpritesConfig{Token: "test-token", WorkRoot: "/home/sprite/crabbox"}}
	lease, claim, _ := spritesTestClaim(t, cfg, "crabbox-test", "original-id", "test-org")
	key, _, err := ensureTestboxKey(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	b := &spritesBackend{cfg: cfg, client: &fakeSpritesAPI{get: spritesInfo{
		ID: "original-id", Name: "crabbox-test", Organization: "test-org", Status: "cold",
		Labels: spritesAPILabels(lease.LeaseID, "test-identity"),
	}}}
	view, err := b.Status(t.Context(), core.StatusRequest{ID: lease.LeaseID})
	if err != nil || view.Ready || view.SSHKey != key {
		t.Fatalf("plain status: ready=%t key=%q err=%v", view.Ready, view.SSHKey, err)
	}
	if _, err := os.Stat(probe); !os.IsNotExist(err) {
		t.Fatalf("plain status invoked SSH: %v", err)
	}
	view, err = b.Status(t.Context(), core.StatusRequest{ID: lease.LeaseID, Wait: true})
	if err != nil || !view.Ready {
		t.Fatalf("explicit SSH probe: ready=%t err=%v", view.Ready, err)
	}
	if _, err := os.Stat(probe); err != nil {
		t.Fatalf("explicit status probe did not run SSH: %v", err)
	}
	after, err := os.ReadFile(key)
	if err != nil || string(before) != string(after) {
		t.Fatalf("status changed key: %v", err)
	}
	stored, _, err := core.ReadLeaseClaimWithPresence(lease.LeaseID)
	if err != nil || !reflect.DeepEqual(claim, stored) {
		t.Fatalf("status changed claim: %v", err)
	}
}
