package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// VerifyEmbeddedRuntime is the credential-free release execution probe. It
// creates its own fixture; no caller paths, config, network, or toolchain are used.
func VerifyEmbeddedRuntime(ctx context.Context, output io.Writer) error {
	if embeddedRunnerBundle() == nil || BundleBuildID == "" {
		return errors.New("release runner verification requires a pinned embedded bundle")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	artifact, err := ArtifactFor(ctx, Target{OS: runtime.GOOS, Arch: runtime.GOARCH})
	if err != nil {
		return err
	}
	directory, err := os.MkdirTemp("", "crabbox-runner-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	name := filepath.Join(directory, "runner")
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(name, artifact.Data, 0o700); err != nil {
		return err
	}
	fixture := []byte("\x00\xff__CRABBOX_RESULT_FILE__:not-a-frame\nCBXR\r\n")
	if err := os.WriteFile(filepath.Join(directory, "fixture"), fixture, 0o600); err != nil {
		return err
	}
	for _, mode := range []string{"serve", "serve-base64"} {
		transport := Transport(func(ctx context.Context, input io.Reader, output io.Writer) error {
			command := exec.CommandContext(ctx, name, mode)
			command.Stdin, command.Stdout = input, output
			return command.Run()
		})
		if mode == "serve-base64" {
			transport = Base64Transport(transport)
		}
		client := &Client{Identity: artifact.Identity, Transport: transport}
		results, err := client.Collect(ctx, directory, []string{"fixture"}, false, "")
		if err != nil {
			return fmt.Errorf("embedded runner %s: %w", mode, err)
		}
		if len(results.Files) != 1 || len(results.Warnings) != 0 || results.Files[0].Path != "fixture" || !bytes.Equal(results.Files[0].Data, fixture) {
			return fmt.Errorf("embedded runner %s returned incorrect fixture bytes", mode)
		}
	}
	return json.NewEncoder(output).Encode(struct {
		Identity           Identity `json:"identity"`
		SHA256             string   `json:"sha256"`
		TrustPolicyVersion int      `json:"trustPolicyVersion"`
		Verified           bool     `json:"verified"`
	}{artifact.Identity, artifact.SHA256, ReleaseRunnerTrustPolicyVersion, true})
}
