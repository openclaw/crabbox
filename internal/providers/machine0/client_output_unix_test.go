//go:build !windows

package machine0

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestClientReadsCompleteJSONFromExitingCLI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required to reproduce the native CLI's process.exit output contract")
	}
	path := filepath.Join(t.TempDir(), "machine0")
	// The published CLI prints JSON and immediately exits. A payload larger
	// than the pipe capacity exposes pending writes discarded by process.exit.
	script := "#!" + node + `
if (process.argv[2] === "--version") {
  console.log(require("node:fs").fstatSync(1).isFIFO() ? "pipe" : "unexpected transport");
  process.exit(0);
}
const payload = "x".repeat(1 << 20);
const value = process.argv[2] === "whoami"
  ? { user: { id: "account-a" }, payload }
  : [{ id: "vm-a", name: "box", status: "RUNNING", payload }];
console.log(JSON.stringify(value));
process.exit(0);
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	c := &client{cfg: Machine0Config{CLIPath: path}, rt: core.RuntimeForProviderOperation(io.Discard)}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	t.Run("account identity", func(t *testing.T) {
		id, err := c.AccountID(ctx)
		if err != nil || id != "account-a" {
			t.Fatalf("account identity=%q err=%v", id, err)
		}
	})
	t.Run("inventory", func(t *testing.T) {
		items, err := c.List(ctx)
		if err != nil || len(items) != 1 || items[0].ID != "vm-a" {
			t.Fatalf("inventory items=%d err=%v", len(items), err)
		}
	})
	t.Run("non-JSON commands retain pipes", func(t *testing.T) {
		version, err := c.Version(ctx)
		if err != nil || version != "pipe" {
			t.Fatalf("non-JSON transport=%q err=%v", version, err)
		}
	})
}
