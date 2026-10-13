package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/openclaw/crabbox/internal/runtimeartifact"
)

const egressIdentityArg = "--internal-identity"

type egressRuntimeIdentity struct {
	Protocol string `json:"protocol"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
}

// RunEgressClient is the runtime pack's restricted client entrypoint.
func RunEgressClient(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return (App{Stdin: stdin, Stdout: stdout, Stderr: stderr, egressClientRuntime: true}).egressClient(ctx, args)
}

func egressRuntimeSource(ctx context.Context, controller, manifest string) (runtimeartifact.Source, error) {
	source, err := runtimeartifact.ResolveSource(ctx, controller, manifest, runtimeartifact.Requirement{
		Capability: runtimeartifact.Egress, ProtocolVersion: runtimeartifact.EgressProtocol,
	}, nil)
	if err != nil {
		return nil, Exit(2, "select verified egress helper: %v; reinstall the matching complete release, or prepare a controller-bound local pack with --egress and set %s", err, runtimeArtifactsEnv)
	}
	return source, nil
}

func validateEgressRuntimeIdentity(data []byte, arch string) error {
	var identity egressRuntimeIdentity
	if err := json.Unmarshal(data, &identity); err != nil || identity != (egressRuntimeIdentity{
		Protocol: runtimeartifact.EgressProtocol, OS: "linux", Arch: arch,
	}) {
		return errors.New("egress helper handshake mismatch; reinstall the matching complete release")
	}
	return nil
}

func (a App) installRemoteEgressClient(ctx context.Context, target SSHTarget) (err error) {
	if target.TargetOS != "" && target.TargetOS != targetLinux {
		return Exit(2, "egress start only supports Linux lease targets; target=%s is not supported", target.TargetOS)
	}
	target.TargetOS = targetLinux
	controller, err := os.Executable()
	if err != nil {
		return err
	}
	source, err := egressRuntimeSource(ctx, controller, strings.TrimSpace(os.Getenv(runtimeArtifactsEnv)))
	if err != nil {
		return err
	}
	helper, err := preparePOSIXRuntime(ctx, target, source, runtimeartifact.Requirement{
		Capability: runtimeartifact.Egress, ProtocolVersion: runtimeartifact.EgressProtocol,
	})
	if err != nil {
		return Exit(5, "provision verified egress helper: %v; check lease SSH access and reinstall the matching complete release if its runtime pack is invalid", err)
	}
	defer func() { err = errors.Join(err, helper.close(ctx)) }()
	nonce, err := randomHex(8)
	if err != nil {
		return err
	}
	uploadPath := egressRemoteBinary + ".tmp-" + nonce
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if cleanupErr := runSSHQuiet(cleanupCtx, target, "rm -f -- "+shellQuote(uploadPath)); cleanupErr != nil {
			err = errors.Join(err, Exit(5, "cleanup egress helper staging failed; remove %s on the lease", uploadPath))
		}
	}()
	command := "cp -- " + shellQuote(helper.path) + " " + shellQuote(uploadPath) +
		" && chmod 700 " + shellQuote(uploadPath) + " && mv -f -- " + shellQuote(uploadPath) + " " + shellQuote(egressRemoteBinary)
	if err := runSSHQuiet(ctx, target, command); err != nil {
		return Exit(5, "install verified egress helper failed; check lease SSH access")
	}
	fmt.Fprintf(a.Stdout, "egress helper: verified runtime pack target=linux/%s sha256=%s\n", helper.identity.Target.Arch, helper.identity.SHA256)
	return nil
}

func currentEgressRuntimeIdentity() egressRuntimeIdentity {
	return egressRuntimeIdentity{Protocol: runtimeartifact.EgressProtocol, OS: runtime.GOOS, Arch: runtime.GOARCH}
}
