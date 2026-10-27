//go:build windows

package sshhost

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// MXC wraps commandLine in cmd.exe /c. A quoted executable followed by a
// quoted argument loses its outer quotes there; an encoded PowerShell command
// preserves both literal paths across the CMD and PowerShell parsers.
func mxcEndpointCommand(executable, state string) string {
	return core.PowershellCommand("& " + core.PowerShellQuote(executable) + " host endpoint --endpoint-state " + core.PowerShellQuote(state) + "; exit $LASTEXITCODE")
}

// Pinned upstream MXC 0.8.0 executors, independently versioned wire schema.
// Activation is deliberately performed by the interactive-context agent. SSH
// network logon is not an activation bridge, and host execution is never a fallback.
func validateRuntime(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("--mxc-executable must identify a pinned MXC 0.8.0 runtime")
	}
	expected := map[string]string{"amd64": "6049c64723af1173c3739dc6cd6b2f33f6c021bb2832c4216233cba7f71aee9a", "arm64": "dde1c592270e9a659b01dccad70362da7b99fec114885fa4d625507aa775a503"}[runtime.GOARCH]
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	digest := sha256.New()
	if _, err = io.Copy(digest, f); err != nil {
		return err
	}
	if expected == "" || hex.EncodeToString(digest.Sum(nil)) != expected {
		return fmt.Errorf("MXC executable does not match the supported 0.8.0 pin")
	}
	probeCommand := exec.Command(path, "--probe")
	probeCommand.Env = mxcEnvironment()
	output, err := probeCommand.Output()
	if err != nil {
		return fmt.Errorf("MXC probe failed in this logon context")
	}
	var probe struct {
		Probes struct {
			Available bool `json:"isolationSessionAvailable"`
		} `json:"probes"`
	}
	if json.Unmarshal(output, &probe) != nil || !probe.Probes.Available {
		return fmt.Errorf("MXC isolation_session unavailable: start the host agent in the logged-in interactive desktop context; SSH network logon is unsupported")
	}
	return nil
}
func newRuntimeState(host string, l hostAssignment) (*runtimeState, error) {
	return &runtimeState{Marker: fmt.Sprintf("crabbox:%s:%s:%d:%d", host, l.LeaseID, l.Generation, l.AccessGeneration)}, nil
}
func mxcArgs(envelope map[string]any) ([]string, error) {
	envelope["version"] = "0.6.0-alpha"
	data, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	return []string{"--config-base64", base64.StdEncoding.EncodeToString(data), "--experimental"}, nil
}
func mxcPhase(ctx context.Context, path string, envelope map[string]any) (json.RawMessage, error) {
	args, err := mxcArgs(envelope)
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, path, args...)
	command.Env = mxcEnvironment()
	data, err := command.Output()
	return decodeMXCReceipt(fmt.Sprint(envelope["phase"]), data, err)
}
func prepareRuntime(ctx context.Context, r *runtimeState, l hostAssignment, path string, save func() error) error {
	// The intent is durable before dispatch. A lost provision receipt quarantines
	// this allocation; never manufacture an identity or provision a replacement.
	result, err := mxcPhase(ctx, path, map[string]any{"phase": "provision", "containment": "isolation_session", "network": map[string]any{"defaultPolicy": "allow", "allowLocalNetwork": true}})
	if err != nil {
		return err
	}
	var provision struct {
		SandboxID         string `json:"sandboxId"`
		CorrelationVector string `json:"correlationVector"`
		Metadata          struct {
			Workspace string `json:"ephemeralWorkspacePath"`
		} `json:"metadata"`
	}
	if json.Unmarshal(result, &provision) != nil || !strings.HasPrefix(provision.SandboxID, "iso:") || !filepath.IsAbs(provision.Metadata.Workspace) {
		return fmt.Errorf("MXC provision identity unavailable; quarantine required")
	}
	r.SandboxID = provision.SandboxID
	r.CorrelationVector = provision.CorrelationVector
	r.Home = provision.Metadata.Workspace
	if err = save(); err != nil {
		return err
	}
	if _, err = mxcPhase(ctx, path, map[string]any{"phase": "start", "sandboxId": r.SandboxID, "correlationVector": r.CorrelationVector}); err != nil {
		return err
	}
	work := filepath.Join(r.Home, "work")
	if err = os.MkdirAll(work, 0700); err != nil {
		return err
	}
	private, public, err := generateHostKey()
	if err != nil {
		return err
	}
	r.HostKey = strings.TrimSpace(public)
	r.Port, err = freePort()
	if err != nil {
		return err
	}
	r.Endpoint = filepath.Join(r.Home, "endpoint.json")
	if err = writeJSON(r.Endpoint, endpointConfig{PublicKey: l.PublicKey, PrivateKey: private, WorkRoot: work, Deadline: l.Deadline, Port: r.Port}, 0600); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	source, err := os.Open(executable)
	if err != nil {
		return err
	}
	defer source.Close()
	target := filepath.Join(r.Home, "crabbox-endpoint.exe")
	dest, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		return err
	}
	_, err = io.Copy(dest, source)
	closeErr := dest.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = save(); err != nil {
		return err
	}
	args, err := mxcArgs(map[string]any{"phase": "exec", "sandboxId": r.SandboxID, "correlationVector": r.CorrelationVector, "process": map[string]string{"commandLine": mxcEndpointCommand(target, r.Endpoint)}})
	if err != nil {
		return err
	}
	command := exec.Command(path, args...)
	command.Env = mxcEnvironment()
	if err = command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}
func cleanupRuntime(ctx context.Context, r *runtimeState, path string) error {
	if r.SandboxID == "" {
		return fmt.Errorf("MXC provision receipt missing: operator recovery required; host remains quarantined")
	}
	// MXC deprovision removes this exact agent user and terminates its session.
	// Stop is not idempotent; retry deprovision directly after uncertain receipts.
	_, err := mxcPhase(ctx, path, map[string]any{"phase": "deprovision", "sandboxId": r.SandboxID, "correlationVector": r.CorrelationVector})
	if err != nil && !mxcDeprovisionConfirmedAbsent(err) {
		return err
	}
	return nil
}
func writeEndpoint(r *runtimeState, c endpointConfig) error {
	return writeJSON(r.Endpoint, c, 0600)
}

func mxcEnvironment() []string {
	var env []string
	for _, name := range []string{"SYSTEMROOT", "WINDIR", "PATH", "TEMP", "TMP", "LOCALAPPDATA", "APPDATA", "USERPROFILE"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}
