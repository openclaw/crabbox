package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIncusFixedLeaseConfiguredCLI(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "crabbox")
	build := exec.Command("go", "build", "-trimpath", "-o", binary, "./cmd/crabbox")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	root := t.TempDir()
	config := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(config, []byte("provider: incus\n"), 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + root, "PATH=" + os.Getenv("PATH"), "XDG_STATE_HOME=" + root, "CRABBOX_CONFIG=" + config}
	_, stderr, code := runDescribeTestBinary(binary, root, env, "warmup", "--provider", "incus", "--lease-id", "cbx_123456789abc", "--incus-socket", filepath.Join(root, "absent.socket"))
	if strings.Contains(string(stderr), "does not support") || strings.Contains(string(stderr), "unsupported") {
		t.Fatalf("fixed Incus allocation was rejected before reaching the provider: exit=%d stderr=%s", code, stderr)
	}
	if code == 0 || !strings.Contains(string(stderr), "absent.socket") {
		t.Fatalf("expected provider connection failure, exit=%d stderr=%s", code, stderr)
	}
}
