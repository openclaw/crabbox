package modal

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
)

func (b *modalBackend) uploadEnvProfile(ctx context.Context, client modalAPI, sandboxID string, env map[string]string) (string, func(), error) {
	if len(env) == 0 {
		return "", func() {}, nil
	}
	file, err := os.CreateTemp("", "crabbox-modal-env-*.sh")
	if err != nil {
		return "", nil, fmt.Errorf("create modal env profile: %w", err)
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
	if _, err := file.WriteString(formatModalShellEnvFile(env)); err != nil {
		return "", nil, fmt.Errorf("write modal env profile: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", nil, fmt.Errorf("close modal env profile: %w", err)
	}
	remotePath := "/tmp/crabbox-modal-env-" + modalRandomSuffix() + ".sh"
	if err := client.UploadFile(ctx, sandboxID, localPath, remotePath); err != nil {
		return "", nil, err
	}
	keep = true
	cleanup := func() {
		cleanupLocal()
		if err := b.execShell(context.Background(), client, sandboxID, "rm -f "+shellQuote(remotePath), nil); err != nil {
			fmt.Fprintf(b.rt.Stderr, "warning: modal env profile cleanup failed for %s: %v\n", sandboxID, err)
		}
	}
	return remotePath, cleanup, nil
}

func formatModalShellEnvFile(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for key := range env {
		if validModalEnvName(key) {
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

func wrapModalCommandWithEnvProfile(command []string, envPath string) []string {
	script := ". " + shellQuote(envPath) + "\n"
	if len(command) == 3 && command[0] == "bash" && command[1] == "-lc" {
		script += command[2]
	} else {
		script += "exec " + shellScriptFromArgv(command)
	}
	return []string{"bash", "-lc", script}
}

func validModalEnvName(name string) bool {
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
