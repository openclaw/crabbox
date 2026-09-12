package blaxel

import (
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestNewSandboxNameFitsBlaxelLimit(t *testing.T) {
	name := newSandboxName(core.Repo{Name: "this-is-a-very-long-repository-name-that-would-exceed-the-blaxel-sandbox-name-limit"})
	if len(name) > sandboxNameMaxLen {
		t.Fatalf("name length=%d name=%q, want <= %d", len(name), name, sandboxNameMaxLen)
	}
	if !strings.HasPrefix(name, namePrefix) {
		t.Fatalf("name=%q missing prefix %q", name, namePrefix)
	}
}
