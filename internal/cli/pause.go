package cli

import (
	"context"
	"encoding/json"
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
	provider := registerProviderSelectionFlag(fs, defaults, providerHelpAll())
	id := fs.String("id", "", "lease id or slug")
	operationID := fs.String("operation-id", "", "durable lifecycle operation ID")
	ownerBinding := fs.String("owner-binding", "", "immutable session/environment digest")
	revision := fs.String("expected-claim-revision", "", "claim revision from retention inspect")
	expectedLease := fs.String("expected-provider-lease-id", "", "immutable provider lease identity")
	expectedAttempt := fs.String("expected-provider-attempt-lease-id", "", "immutable provider attempt identity")
	expectedSlug := fs.String("expected-provider-slug", "", "immutable provider slug identity")
	expectedResource := fs.String("expected-provider-resource-id", "", "immutable provider resource identity")
	jsonOut := fs.Bool("json", false, "print independent lifecycle status readback")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	idFlagSet := flagWasSet(fs, "id")
	setIDFromFirstArg(fs, id)
	if strings.TrimSpace(*id) == "" || fs.NArg() > 1 || (idFlagSet && fs.NArg() > 0) {
		return Exit(2, "usage: crabbox %s --id <lease-or-server-id>", action)
	}
	cfg, err := loadConfigForFlags(fs)
	if err != nil {
		return err
	}
	if err := prepareProviderSelection(&cfg, *provider); err != nil {
		return err
	}
	if err := autoRouteClaimLeaseProvider(&cfg, fs, *id); err != nil {
		return err
	}
	if err := autoRouteStaticLease(&cfg, fs, *id); err != nil {
		return err
	}
	if err := autoRouteExternalLease(&cfg, fs, *id); err != nil {
		return err
	}
	if err := applyTargetAndProviderFlags(&cfg, fs, targetFlags, providerFlags); err != nil {
		return err
	}
	if err := finalizeProviderSelection(&cfg); err != nil {
		return err
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	pausable, ok := backend.(PausableBackend)
	if !ok {
		return Exit(2, "provider=%s does not support %s", backend.Spec().Name, action)
	}
	lifecycle := RetentionLifecycleRequest{OperationID: *operationID, OwnerBinding: *ownerBinding, ExpectedClaimRevision: *revision, ExpectedProviderIdentity: ProviderIdentityExpectation{LeaseID: *expectedLease, AttemptLeaseID: *expectedAttempt, Slug: *expectedSlug, ResourceID: *expectedResource}}
	if (lifecycle.OperationID != "" || lifecycle.OwnerBinding != "" || lifecycle.ExpectedClaimRevision != "" || !lifecycle.ExpectedProviderIdentity.empty()) && !backend.Spec().Features.Has(FeatureSessionDiskRetentionV1) {
		return Exit(2, "provider does not support retained lifecycle identity contract")
	}
	opts := leaseOptionsFromConfig(cfg)
	if action == "pause" {
		err = pausable.Pause(ctx, PauseRequest{Options: opts, ID: *id, Lifecycle: lifecycle})
	} else {
		err = pausable.Resume(ctx, ResumeRequest{Options: opts, ID: *id, Lifecycle: lifecycle})
	}
	if err != nil {
		return err
	}
	if *jsonOut {
		status, ok := backend.(interface {
			Status(context.Context, StatusRequest) (StatusView, error)
		})
		if !ok {
			return Exit(2, "provider has no lifecycle status receipt")
		}
		view, err := status.Status(ctx, StatusRequest{Options: opts, ID: *id})
		if err != nil {
			return err
		}
		if view.Retention != nil {
			target := "suspended"
			if action == "resume" {
				target = "running"
			}
			if view.State != target || view.Retention.Operation == nil || view.Retention.Operation.ID != lifecycle.OperationID || view.Retention.Operation.Phase != "observed" {
				return Exit(4, "lifecycle status readback remains uncertain; inspect the same lease")
			}
		}
		return json.NewEncoder(a.Stdout).Encode(view)
	}
	return nil
}
