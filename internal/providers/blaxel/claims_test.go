package blaxel

import (
	"strings"
	"testing"
)

func TestNewSandboxNameFitsBlaxelLimit(t *testing.T) {
	name := newSandboxName(Repo{Name: "this-is-a-very-long-repository-name-that-would-exceed-the-blaxel-sandbox-name-limit"})
	if len(name) > sandboxNameMaxLen {
		t.Fatalf("name length=%d name=%q, want <= %d", len(name), name, sandboxNameMaxLen)
	}
	if !strings.HasPrefix(name, namePrefix) {
		t.Fatalf("name=%q missing prefix %q", name, namePrefix)
	}
}
