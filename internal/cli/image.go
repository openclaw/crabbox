package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func (a App) imageCreate(ctx context.Context, args []string) error {
	fs := newFlagSet("image create", a.Stderr)
	id := fs.String("id", "", "lease id to image")
	name := fs.String("name", "", "provider image name")
	wait := fs.Bool("wait", false, "wait until the provider image is available")
	waitTimeout := fs.Duration("wait-timeout", 45*time.Minute, "maximum wait duration")
	noReboot := fs.Bool("no-reboot", true, "avoid rebooting the source AWS instance while creating the AMI")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *id == "" || *name == "" {
		return exit(2, "usage: crabbox image create --id <cbx_id> --name <image-name> [--wait]")
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	image, err := coord.CreateImage(ctx, *id, *name, *noReboot, checkpointStrategyImage)
	if err != nil {
		return err
	}
	if *wait {
		image, err = waitForImage(ctx, coord, image.ID, imageRefFromCoordinatorImage(image), *waitTimeout, a.Stderr)
		if err != nil {
			return err
		}
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(image)
	}
	fmt.Fprintf(a.Stdout, "image=%s name=%s state=%s region=%s\n", image.ID, image.Name, image.State, blank(image.Region, "-"))
	return nil
}

func (a App) imagePromote(ctx context.Context, args []string) error {
	fs := newFlagSet("image promote", a.Stderr)
	provider := fs.String("provider", "aws", "image provider: aws or azure")
	target := fs.String("target", "", "image target: linux, macos, or windows")
	osImage := fs.String("os", "", "portable Linux OS selector for promoted Linux AMIs")
	region := fs.String("region", "", "provider region or Azure location containing the image")
	serverType := fs.String("type", "", "AWS instance type or Azure VM size the image boots on")
	serverTypeAlias := fs.String("server-type", "", "alias for --type")
	architecture := fs.String("architecture", "", "AWS AMI architecture, for example x86_64_mac or arm64_mac")
	osVersion := fs.String("os-version", "", "OS version present in the image")
	var sdkVersions stringListFlag
	var runtimeVersions stringListFlag
	var variantSDKVersions stringListFlag
	var variantRuntimeVersions stringListFlag
	fs.Var(&sdkVersions, "sdk", "SDK present in name=version form; repeatable")
	fs.Var(&runtimeVersions, "runtime", "runtime present in name=version form; repeatable")
	fs.Var(&variantSDKVersions, "variant-sdk", "SDK that explicitly activates a catalog-only image; repeatable")
	fs.Var(&variantRuntimeVersions, "variant-runtime", "runtime that explicitly activates a catalog-only image; repeatable")
	browser := fs.Bool("browser", false, "image includes a browser")
	webView2 := fs.Bool("webview2", false, "image includes Microsoft WebView2")
	desktop := fs.Bool("desktop", false, "image includes desktop support")
	catalogOnly := fs.Bool("catalog-only", false, "publish an AWS capability variant without changing the default image")
	fastSnapshotRestore := fs.Bool("fast-snapshot-restore", false, "enable AWS Fast Snapshot Restore for the promoted AMI snapshots")
	var fastSnapshotRestoreAZs stringListFlag
	fs.Var(&fastSnapshotRestoreAZs, "fsr-az", "availability zone for Fast Snapshot Restore; repeatable")
	qualificationRef := fs.String("qualification-ref", "", "immutable signed AWS qualification OCI digest reference")
	promotionEvidence := fs.String("promotion-evidence", "", "verified promotion evidence JSON file")
	expectedCurrentImage := fs.String("expected-current-image", "", "expected current image id, or none for an absent default")
	expectedCurrentRevision := fs.String("expected-current-revision", "", "expected current default revision when an image is present")
	idempotencyKey := fs.String("idempotency-key", "", "stable protected promotion attempt key")
	workflowRunID := fs.String("workflow-run-id", "", "calling workflow run id")
	workflowRunAttempt := fs.String("workflow-run-attempt", "", "calling workflow run attempt")
	rollback := fs.Bool("rollback", false, "roll back to the prior successful protected default")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	protectedPromotion := flagWasSet(fs, "expected-current-image")
	if (!protectedPromotion && fs.NArg() != 1) || (protectedPromotion && fs.NArg() != 0) {
		return exit(2, "usage: crabbox image promote <image-id> [legacy options] | crabbox image promote --qualification-ref <oci@sha256:...> --promotion-evidence <file> --expected-current-image <id|none> [--expected-current-revision <revision>] --idempotency-key <key> --workflow-run-id <id> --workflow-run-attempt <attempt>")
	}
	if protectedPromotion {
		if strings.TrimSpace(*qualificationRef) == "" {
			return exit(2, "--qualification-ref is required with --expected-current-image")
		}
		evidence, err := loadImagePromotionEvidence(*promotionEvidence, strings.TrimSpace(*qualificationRef))
		if err != nil {
			return err
		}
		if strings.TrimSpace(*idempotencyKey) == "" {
			return exit(2, "--idempotency-key is required with --expected-current-image")
		}
		if strings.TrimSpace(*workflowRunID) == "" || strings.TrimSpace(*workflowRunAttempt) == "" {
			return exit(2, "--workflow-run-id and --workflow-run-attempt are required with --expected-current-image")
		}
		expectedImage := strings.TrimSpace(*expectedCurrentImage)
		expectedRevision := strings.TrimSpace(*expectedCurrentRevision)
		if expectedImage == "" {
			return exit(2, "--expected-current-image must be an image id or none")
		}
		if expectedImage == "none" {
			if expectedRevision != "" {
				return exit(2, "--expected-current-revision is invalid when --expected-current-image=none")
			}
		} else if expectedRevision == "" {
			return exit(2, "--expected-current-revision is required when the expected current image is present")
		}
		if *catalogOnly || *fastSnapshotRestore || len(fastSnapshotRestoreAZs) > 0 {
			return exit(2, "protected image promotion does not support catalog-only or Fast Snapshot Restore options")
		}
		for _, name := range []string{
			"provider", "target", "os", "region", "type", "server-type", "architecture",
			"os-version", "sdk", "runtime", "variant-sdk", "variant-runtime", "browser",
			"webview2", "desktop",
		} {
			if flagWasSet(fs, name) {
				return exit(2, "protected image promotion derives provider and scope from signed qualification evidence; --%s is not allowed", name)
			}
		}
		expected := CoordinatorImageDefaultState{State: "present", ImageID: expectedImage, Revision: expectedRevision}
		if expectedImage == "none" {
			expected = CoordinatorImageDefaultState{State: "absent"}
		}
		coord, err := configuredPromotionCoordinator()
		if err != nil {
			return err
		}
		operation := "promote"
		if *rollback {
			operation = "rollback"
		}
		result, err := coord.PromoteQualifiedImage(ctx, CoordinatorImagePromotionRequest{
			Schema:             "crabbox-image-promotion-request/v1",
			Operation:          operation,
			Expected:           expected,
			Evidence:           evidence,
			IdempotencyKey:     strings.TrimSpace(*idempotencyKey),
			WorkflowRunID:      strings.TrimSpace(*workflowRunID),
			WorkflowRunAttempt: strings.TrimSpace(*workflowRunAttempt),
		})
		if err != nil {
			return err
		}
		if *jsonOut {
			return json.NewEncoder(a.Stdout).Encode(result)
		}
		fmt.Fprintf(a.Stdout, "%s image=%s state=%s region=%s revision=%s\n", operation, result.Image.ID, result.Image.State, blank(result.Image.Region, "-"), blank(result.Image.Revision, "-"))
		return nil
	}
	if strings.TrimSpace(*qualificationRef) != "" || strings.TrimSpace(*promotionEvidence) != "" ||
		flagWasSet(fs, "expected-current-revision") ||
		flagWasSet(fs, "idempotency-key") || flagWasSet(fs, "workflow-run-id") ||
		flagWasSet(fs, "workflow-run-attempt") || *rollback {
		return exit(2, "protected promotion flags require --expected-current-image")
	}
	normalizedProvider := normalizeProviderName(*provider)
	if normalizedProvider != "aws" && normalizedProvider != "azure" {
		return exit(2, "unsupported image promotion provider %q; use aws or azure", *provider)
	}
	if normalizedProvider == "azure" && *catalogOnly {
		return exit(2, "--catalog-only is AWS-only")
	}
	if normalizedProvider == "azure" && (*fastSnapshotRestore || len(fastSnapshotRestoreAZs) > 0) {
		return exit(2, "Fast Snapshot Restore is AWS-only")
	}
	if *serverType == "" {
		*serverType = *serverTypeAlias
	}
	if err := validateImageVersion(strings.TrimSpace(*osVersion), "os-version"); err != nil {
		return err
	}
	sdks, err := parseImageVersions(sdkVersions, "sdk")
	if err != nil {
		return err
	}
	runtimes, err := parseImageVersions(runtimeVersions, "runtime")
	if err != nil {
		return err
	}
	variantSDKs, err := parseImageVersions(variantSDKVersions, "variant-sdk")
	if err != nil {
		return err
	}
	variantRuntimes, err := parseImageVersions(variantRuntimeVersions, "variant-runtime")
	if err != nil {
		return err
	}
	variantSelectors := imageVariantSelectors{SDKs: variantSDKs, Runtimes: variantRuntimes}
	if *catalogOnly && imageVariantSelectorsEmpty(variantSelectors) {
		return exit(2, "--catalog-only requires at least one --variant-sdk or --variant-runtime declaration")
	}
	if !*catalogOnly && !imageVariantSelectorsEmpty(variantSelectors) {
		return exit(2, "--variant-sdk and --variant-runtime require --catalog-only")
	}
	sdks, err = mergeImageVersions(sdks, variantSDKs, "sdk", "variant-sdk")
	if err != nil {
		return err
	}
	runtimes, err = mergeImageVersions(runtimes, variantRuntimes, "runtime", "variant-runtime")
	if err != nil {
		return err
	}
	if normalizedProvider == "azure" &&
		(strings.TrimSpace(*osVersion) != "" ||
			len(sdks) > 0 ||
			len(runtimes) > 0 ||
			*browser ||
			*webView2 ||
			*desktop) {
		return exit(2, "image capability declarations are AWS-only")
	}
	if flagWasSet(fs, "os") {
		normalized, err := normalizeOSImage(*osImage)
		if err != nil {
			return err
		}
		*osImage = normalized
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	image, err := coord.PromoteImage(ctx, fs.Arg(0), CoordinatorImageRef{
		Provider:               normalizedProvider,
		Region:                 *region,
		Target:                 *target,
		OSImage:                *osImage,
		ServerType:             *serverType,
		Architecture:           *architecture,
		CatalogOnly:            *catalogOnly,
		FastSnapshotRestore:    *fastSnapshotRestore,
		FastSnapshotRestoreAZs: fastSnapshotRestoreAZs,
		Capabilities: imageCapabilities{
			OSVersion: strings.TrimSpace(*osVersion),
			SDKs:      sdks,
			Runtimes:  runtimes,
			Browser:   *browser,
			WebView2:  *webView2,
			Desktop:   *desktop,
		},
		VariantSelectors: variantSelectors,
	})
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(image)
	}
	fmt.Fprintf(a.Stdout, "promoted image=%s name=%s state=%s region=%s", image.ID, image.Name, image.State, blank(image.Region, "-"))
	if image.CatalogOnly {
		fmt.Fprint(a.Stdout, " catalogOnly=true")
	}
	fmt.Fprintln(a.Stdout)
	return nil
}

func loadImagePromotionEvidence(file, qualificationRef string) (json.RawMessage, error) {
	if strings.TrimSpace(file) == "" {
		return nil, exit(2, "--promotion-evidence is required with --expected-current-image")
	}
	info, err := os.Stat(file)
	if err != nil {
		return nil, exit(2, "read promotion evidence: %v", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 1024*1024 {
		return nil, exit(2, "promotion evidence must be a non-empty regular file no larger than 1 MiB")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, exit(2, "read promotion evidence: %v", err)
	}
	var record struct {
		Schema        string `json:"schema"`
		Qualification struct {
			Reference string `json:"reference"`
		} `json:"qualification"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, exit(2, "promotion evidence is not valid JSON: %v", err)
	}
	if record.Schema != "crabbox-image-promotion-evidence/v1" {
		return nil, exit(2, "unsupported promotion evidence schema")
	}
	if record.Qualification.Reference != qualificationRef {
		return nil, exit(2, "promotion evidence qualification reference does not match --qualification-ref")
	}
	return json.RawMessage(data), nil
}

func (a App) imageFSRStatus(ctx context.Context, args []string) error {
	fs := newFlagSet("image fsr-status", a.Stderr)
	provider := fs.String("provider", "aws", "image provider: aws")
	region := fs.String("region", "", "AWS region containing the AMI or snapshot")
	var fastSnapshotRestoreAZs stringListFlag
	fs.Var(&fastSnapshotRestoreAZs, "fsr-az", "availability zone for Fast Snapshot Restore status; repeatable")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox image fsr-status <ami-id|snapshot-id> [--region <aws-region>] [--fsr-az <az>]")
	}
	normalizedProvider := normalizeProviderName(*provider)
	if normalizedProvider != "aws" {
		return exit(2, "unsupported image provider %q; Fast Snapshot Restore is AWS-only", *provider)
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	image, err := coord.FastSnapshotRestoreStatus(ctx, fs.Arg(0), CoordinatorImageRef{
		Provider:               normalizedProvider,
		Region:                 *region,
		FastSnapshotRestoreAZs: fastSnapshotRestoreAZs,
	})
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(image)
	}
	fmt.Fprintf(a.Stdout, "image=%s state=%s region=%s fsr=%d\n", image.ID, blank(image.State, "-"), blank(image.Region, "-"), len(image.FastSnapshotRestores))
	if len(image.FastSnapshotRestores) == 0 {
		fmt.Fprintln(a.Stdout, "fsr none")
		return nil
	}
	for _, status := range image.FastSnapshotRestores {
		fmt.Fprintf(a.Stdout, "fsr snapshot=%s az=%s state=%s reason=%s\n", status.SnapshotID, status.AvailabilityZone, blank(status.State, "-"), blank(status.StateTransitionReason, "-"))
	}
	return nil
}

func (a App) imageDelete(ctx context.Context, args []string) error {
	fs := newFlagSet("image delete", a.Stderr)
	provider := fs.String("provider", "aws", "image provider: aws, azure, gcp, or hetzner")
	region := fs.String("region", "", "region, location, or zone containing the image")
	project := fs.String("project", "", "GCP project containing the image")
	catalogOnly := fs.Bool("catalog-only", false, "unpublish AWS catalog-only variants without deleting the AMI")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox image delete <image-id> [--provider aws|azure|gcp|hetzner] [--region <region>] [--project <project>] [--catalog-only]")
	}
	normalizedProvider := normalizeProviderName(*provider)
	if normalizedProvider != "aws" && normalizedProvider != "azure" && normalizedProvider != "gcp" && normalizedProvider != "hetzner" {
		return exit(2, "unsupported image provider %q; use aws, azure, gcp, or hetzner", *provider)
	}
	if *catalogOnly && normalizedProvider != "aws" {
		return exit(2, "--catalog-only is AWS-only")
	}
	if normalizedProvider == "hetzner" {
		if strings.TrimSpace(*project) != "" {
			return exit(2, "--project is not supported for Hetzner image deletion")
		}
		store, err := defaultCheckpointStore()
		if err != nil {
			return err
		}
		lifecycle, ok := nativeCheckpointLifecycleProvider(Config{Provider: "hetzner"}, Server{})
		if !ok {
			return exit(2, "Hetzner snapshot lifecycle provider is unavailable")
		}
		record, err := deleteHetznerCheckpointImage(ctx, store, lifecycle, fs.Arg(0), strings.TrimSpace(*region))
		if err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "deleted image=%s provider=hetzner region=%s project=- checkpoint=%s\n", fs.Arg(0), blank(record.Native.Region, "-"), record.ID)
		return nil
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	ref := CoordinatorImageRef{Provider: normalizedProvider, Region: *region, Project: *project}
	if *catalogOnly {
		retired, err := coord.RetireCatalogImage(ctx, fs.Arg(0), ref)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "retired catalog-only image=%s provider=aws variants=%d\n", fs.Arg(0), retired.Retired)
		return nil
	}
	if err := coord.DeleteImage(ctx, fs.Arg(0), ref); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "deleted image=%s provider=%s region=%s project=%s\n", fs.Arg(0), normalizedProvider, blank(*region, "-"), blank(*project, "-"))
	return nil
}

func deleteHetznerCheckpointImage(ctx context.Context, store checkpointStore, lifecycle NativeCheckpointLifecycleProvider, imageID, region string) (checkpointRecord, error) {
	records, err := store.List()
	if err != nil {
		return checkpointRecord{}, err
	}
	matches := make([]checkpointRecord, 0, 1)
	for _, record := range records {
		if record.Kind == checkpointKindHetzner && record.Native.ImageID == imageID {
			matches = append(matches, record)
		}
	}
	if len(matches) == 0 {
		return checkpointRecord{}, exit(2, "refusing to delete Hetzner snapshot %s without an exact local hetzner-snapshot checkpoint record", imageID)
	}
	if len(matches) > 1 {
		return checkpointRecord{}, exit(2, "refusing to delete Hetzner snapshot %s because %d local checkpoint records claim it", imageID, len(matches))
	}
	record := matches[0]
	if region != "" && region != record.Native.Region {
		return checkpointRecord{}, exit(2, "Hetzner snapshot %s location mismatch: recorded=%s requested=%s", imageID, blank(record.Native.Region, "unknown"), region)
	}
	if err := lifecycle.DeleteNativeCheckpoint(ctx, nativeCheckpointResourceRequest(record)); err != nil {
		return checkpointRecord{}, err
	}
	if err := store.Delete(record.ID); err != nil {
		return checkpointRecord{}, err
	}
	return record, nil
}

func waitForImage(ctx context.Context, coord *CoordinatorClient, imageID string, ref CoordinatorImageRef, timeout time.Duration, stderr io.Writer) (CoordinatorImage, error) {
	deadline := time.Now().Add(timeout)
	var last CoordinatorImage
	for {
		image, err := coord.Image(ctx, imageID, ref)
		if err != nil {
			return CoordinatorImage{}, err
		}
		last = image
		state := strings.ToLower(image.State)
		if state == "available" || state == "ready" || state == "succeeded" || state == "completed" {
			return image, nil
		}
		if state == "failed" || state == "invalid" {
			return CoordinatorImage{}, exit(5, "image %s failed", imageID)
		}
		if time.Now().After(deadline) {
			return CoordinatorImage{}, exit(5, "timed out waiting for image %s; last state=%s", imageID, last.State)
		}
		_, _ = fmt.Fprintf(stderr, "waiting image=%s state=%s\n", imageID, blank(image.State, "pending"))
		select {
		case <-ctx.Done():
			return CoordinatorImage{}, ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

func imageRefFromCoordinatorImage(image CoordinatorImage) CoordinatorImageRef {
	return CoordinatorImageRef{
		Provider:     image.Provider,
		Region:       image.Region,
		Project:      image.Project,
		Kind:         image.Kind,
		Target:       image.Target,
		ServerType:   image.ServerType,
		Architecture: image.Architecture,
	}
}
