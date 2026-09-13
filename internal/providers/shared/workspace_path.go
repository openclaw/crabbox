package shared

import (
	"path"
	"slices"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// CleanPOSIXWorkspacePath rejects broad system roots, plus any adapter-owned
// mount roots. It checks exact roots, not their dedicated subdirectories.
func CleanPOSIXWorkspacePath(label, workspace string, reservedRoots ...string) (string, error) {
	trimmed := strings.TrimSpace(workspace)
	if trimmed == "" {
		return "", core.Exit(2, "%s is empty", label)
	}
	clean := path.Clean(trimmed)
	if !strings.HasPrefix(clean, "/") {
		return "", core.Exit(2, "%s %q must resolve to an absolute path", label, workspace)
	}
	switch clean {
	case "/", "/bin", "/dev", "/etc", "/home", "/lib", "/lib64", "/opt", "/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var":
		return "", core.Exit(2, "%s %q is too broad; choose a dedicated subdirectory", label, clean)
	}
	if slices.Contains(reservedRoots, clean) {
		return "", core.Exit(2, "%s %q is too broad; choose a dedicated subdirectory", label, clean)
	}
	return clean, nil
}
