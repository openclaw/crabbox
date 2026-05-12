package tensorlake

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
)

func (b *tensorlakeBackend) uploadEnvProfile(ctx context.Context, cli *tensorlakeCLI, sandboxID string, env map[string]string) (string, func(), error) {
	if len(env) == 0 {
		return "", func() {}, nil
	}
	file, err := os.CreateTemp("", "crabbox-tensorlake-env-*.sh")
	if err != nil {
		return "", nil, fmt.Errorf("create tensorlake env profile: %w", err)
	}
	localPath := file.Name()
	cleanupLocal := func() { _ = os.Remove(localPath) }
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			cleanupLocal()
		}
	}()
	if _, err := file.WriteString(formatTensorlakeShellEnvFile(env)); err != nil {
		return "", nil, fmt.Errorf("write tensorlake env profile: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", nil, fmt.Errorf("close tensorlake env profile: %w", err)
	}
	remotePath := "/tmp/crabbox-env-" + randomSuffix() + ".sh"
	if err := cli.uploadFile(ctx, sandboxID, localPath, remotePath); err != nil {
		return "", nil, err
	}
	keep = true
	cleanup := func() {
		cleanupLocal()
		if err := cli.execShell(context.Background(), sandboxID, "rm -f "+shellQuote(remotePath)); err != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: tensorlake env profile cleanup failed for %s: %v\n", sandboxID, err)
		}
	}
	return remotePath, cleanup, nil
}

func formatTensorlakeShellEnvFile(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for key := range env {
		if validTensorlakeEnvName(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("set -a\n")
	for _, key := range keys {
		fmt.Fprintf(&b, "%s=%s\n", key, shellQuote(env[key]))
	}
	b.WriteString("set +a\n")
	return b.String()
}

func wrapCommandWithEnvProfile(command []string, envPath string) []string {
	script := ". " + shellQuote(envPath) + "\n"
	if len(command) == 3 && command[0] == "bash" && command[1] == "-lc" {
		script += command[2]
	} else {
		script += "exec " + shellScriptFromArgv(command)
	}
	return []string{"bash", "-lc", script}
}

func validTensorlakeEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}
