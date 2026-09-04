package shared

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"

	core "github.com/openclaw/crabbox/internal/cli"
)

// PreparedShellEnvProfile owns one synchronous upload attempt and its files.
// Close must run after Upload returns, including when the upload failed. Remote
// permissions and resource authorization remain the adapter's responsibility.
type PreparedShellEnvProfile struct {
	localPath, remotePath string
	uploadAttempted       bool
	closed                bool
	closeOnce             sync.Once
	closeErr              error
}

func PrepareShellEnvProfile(env map[string]string, prefix string) (*PreparedShellEnvProfile, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("name environment profile: %w", err)
	}
	file, err := os.CreateTemp("", prefix+"*.sh")
	if err != nil {
		return nil, fmt.Errorf("create environment profile: %w", err)
	}
	_, writeErr := file.WriteString(renderShellEnvProfile(env))
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		_ = os.Remove(file.Name())
		return nil, fmt.Errorf("write environment profile: %w", err)
	}
	return &PreparedShellEnvProfile{localPath: file.Name(), remotePath: path.Join("/tmp", prefix+hex.EncodeToString(nonce[:])+".sh")}, nil
}

func (p *PreparedShellEnvProfile) RemotePath() string { return p.remotePath }

func (p *PreparedShellEnvProfile) Upload(ctx context.Context, upload func(context.Context, string, string) error) error {
	if p.closed || p.uploadAttempted {
		return errors.New("environment profile upload already attempted or closed")
	}
	// A failed response can follow a partial remote write.
	p.uploadAttempted = true
	defer func() { _ = os.Remove(p.localPath) }()
	return upload(ctx, p.localPath, p.remotePath)
}

func (p *PreparedShellEnvProfile) Close(ctx context.Context, remove func(context.Context, string) error) error {
	p.closeOnce.Do(func() {
		p.closed = true
		localErr := os.Remove(p.localPath)
		if errors.Is(localErr, os.ErrNotExist) {
			localErr = nil
		}
		var remoteErr error
		if p.uploadAttempted {
			remoteErr = remove(ctx, p.remotePath)
		}
		p.closeErr = errors.Join(localErr, remoteErr)
	})
	return p.closeErr
}

func renderShellEnvProfile(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for key := range env {
		valid := key != ""
		for i, r := range key {
			if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
				valid = false
				break
			}
		}
		if valid {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var out strings.Builder
	out.WriteString("set -a\n")
	for _, key := range keys {
		fmt.Fprintf(&out, "%s=%s\n", key, core.ShellQuote(env[key]))
	}
	out.WriteString("set +a\n")
	return out.String()
}

// WrapCommandWithShellEnvProfile gates only profile sourcing, not user commands.
func WrapCommandWithShellEnvProfile(command []string, envPath string) []string {
	script := ". " + core.ShellQuote(envPath) + " || exit $?\n"
	if len(command) == 3 && command[0] == "bash" && command[1] == "-lc" {
		script += command[2]
	} else {
		script += "exec " + core.ShellScriptFromArgv(command)
	}
	return []string{"bash", "-lc", script}
}
