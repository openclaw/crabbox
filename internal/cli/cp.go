package cli

import (
	"context"
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
	if !ok {
		return exit(2, "provider=%s does not support cp; use a provider with native file copy", backend.Spec().Name)
	}
	return copyBackend.Copy(ctx, CopyRequest{
		Options:     leaseOptionsFromConfig(cfg),
		ID:          *id,
		Source:      fs.Arg(0),
		Destination: fs.Arg(1),
		FollowLink:  *followLink,
	})
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
