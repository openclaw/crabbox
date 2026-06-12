package cli

import (
	"context"
	"strings"
)

func (a App) pause(ctx context.Context, args []string) error {
	return a.pauseResume(ctx, args, "pause")
}

func (a App) resume(ctx context.Context, args []string) error {
	return a.pauseResume(ctx, args, "resume")
}

func (a App) pauseResume(ctx context.Context, args []string, action string) error {
	defaults := defaultConfig()
	fs := newFlagSet(action, a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpAll())
	id := fs.String("id", "", "lease id or slug")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	idFlagSet := flagWasSet(fs, "id")
	setIDFromFirstArg(fs, id)
	if strings.TrimSpace(*id) == "" || fs.NArg() > 1 || (idFlagSet && fs.NArg() > 0) {
		return exit(2, "usage: crabbox %s --id <lease-or-server-id>", action)
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Provider = *provider
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return err
	}
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		return err
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	pausable, ok := backend.(PausableBackend)
	if !ok {
		return exit(2, "provider=%s does not support %s", backend.Spec().Name, action)
	}
	opts := leaseOptionsFromConfig(cfg)
	if action == "pause" {
		return pausable.Pause(ctx, PauseRequest{Options: opts, ID: *id})
	}
	return pausable.Resume(ctx, ResumeRequest{Options: opts, ID: *id})
}
