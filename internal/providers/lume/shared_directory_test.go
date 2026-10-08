package lume

import "testing"

func TestLumeSharedDirectoryRejectsRemoteColonPaths(t *testing.T) {
	for _, path := range []string{"remote:path", "/var/lib/crabbox:rw"} {
		if err := validateLumeSharedDirectoryPath(path); err == nil {
			t.Fatalf("validateLumeSharedDirectoryPath(%q) succeeded", path)
		}
	}
}
