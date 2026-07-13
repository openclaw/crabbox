package cli

import (
	"context"
	"os/exec"
	"strings"
)

func (a App) copyCommand(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("cp", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpAll())
	id := fs.String("id", "", "lease id or slug")
	followLink := fs.Bool("L", false, "follow symbolic links when copying from host to sandbox")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if strings.TrimSpace(*id) == "" || fs.NArg() != 2 {
		return exit(2, "usage: crabbox cp --id <lease-id-or-slug> [-L] <src> <dst>")
	}
	if err := validateCopyArgs(fs.Arg(0), fs.Arg(1)); err != nil {
		return err
	}
	cfg, err := loadPortsConfig(fs, *provider, providerFlags, targetFlags, *id)
	if err != nil {
		return err
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	copyBackend, ok := backend.(CopyBackend)
	if ok {
		return copyBackend.Copy(ctx, CopyRequest{
			Options:     leaseOptionsFromConfig(cfg),
			ID:          *id,
			Source:      fs.Arg(0),
			Destination: fs.Arg(1),
			FollowLink:  *followLink,
		})
	}
	if _, ok := backend.(SSHLoginBackend); !ok {
		return exit(2, "provider=%s does not support cp; use a provider with native file copy or SSH access", backend.Spec().Name)
	}
	lease, err := a.resolveNetworkLoginLeaseTargetForRepo(ctx, &cfg, *id, false, false, true)
	if err != nil {
		return err
	}
	if lease.SSH.AuthSecret {
		return exit(2, "provider=%s does not support SSH cp with token-as-username authentication", backend.Spec().Name)
	}
	if err := a.claimAndTouchLeaseTarget(ctx, cfg, lease.Server, lease.SSH, lease.LeaseID, false); err != nil {
		return err
	}
	copyArgs, err := sshCopyArgs(lease.SSH, fs.Arg(0), fs.Arg(1))
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "scp", copyArgs...)
	cmd.Stdout = a.Stdout
	cmd.Stderr = a.Stderr
	if err := cmd.Run(); err != nil {
		if code := exitCode(err); code > 0 {
			return ExitError{Code: code}
		}
		return err
	}
	return nil
}

func sshCopyArgs(target SSHTarget, source, destination string) ([]string, error) {
	if err := validateCopyArgs(source, destination); err != nil {
		return nil, err
	}
	remote := func(path string) string {
		return target.User + "@" + target.Host + ":" + strings.SplitN(path, ":", 2)[1]
	}
	if isSandboxCopyArg(source) {
		source = remote(source)
	} else {
		destination = remote(destination)
	}
	return append(scpBaseArgs(target), source, destination), nil
}

func validateCopyArgs(src, dst string) error {
	srcSandbox := isSandboxCopyArg(src)
	dstSandbox := isSandboxCopyArg(dst)
	if srcSandbox == dstSandbox {
		return exit(2, "usage: crabbox cp --id <lease-id-or-slug> [-L] <src> <dst> (exactly one path must use SANDBOX:PATH)")
	}
	return nil
}

func isSandboxCopyArg(value string) bool {
	prefix := value
	if idx := strings.IndexByte(value, ':'); idx >= 0 {
		prefix = value[:idx]
	}
	return strings.EqualFold(strings.TrimSpace(prefix), "SANDBOX") && strings.Contains(value, ":")
}
